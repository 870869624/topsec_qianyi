package syncbase

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Progress 在终端上实时刷新迁移进度。
//
// 输出形态（终端支持 ANSI 时原地重绘，不支持时自动降级为按百分比打点）：
//
//	迁移中  DLPDATA.did_system_sync_user → identity.user
//	读取   [████████████░░░░░░░░░░░░░░░░░░]  42.13%   168,520 / 400,000 条
//	写入   [████████████░░░░░░░░░░░░░░░░░░]  42.13%   168,520 条    失败 0 条
//	速度   读 12,430 条/秒 · 写 11,980 条/秒      已用 00:00:17   剩余 00:00:23
type Progress struct {
	source string
	target string

	total int64

	read    atomic.Int64
	written atomic.Int64
	failed  atomic.Int64

	start time.Time

	tty      bool
	showBar  bool
	interval time.Duration

	mu       sync.Mutex
	drawn    int
	samples  []rateSample
	lastPct  int
	lastLog  time.Time
	finished bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
	width    int
}

type rateSample struct {
	t       time.Time
	read    int64
	written int64
}

// NewProgress 创建进度显示器。enabled 为 false 时不输出任何动态内容。
func NewProgress(source, target string, total int64, enabled bool) *Progress {
	p := &Progress{
		source:   source,
		target:   target,
		total:    total,
		start:    time.Now(),
		tty:      enabled && isTerminal(os.Stdout),
		showBar:  enabled,
		interval: 200 * time.Millisecond,
		stopCh:   make(chan struct{}),
		width:    termWidth(),
	}
	return p
}

// Start 启动后台重绘协程。
func (p *Progress) Start() {
	if !p.showBar {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		tk := time.NewTicker(p.interval)
		defer tk.Stop()
		for {
			select {
			case <-p.stopCh:
				return
			case now := <-tk.C:
				p.tick(now)
			}
		}
	}()
}

// AddRead 累加已读取的行数。
func (p *Progress) AddRead(n int64) { p.read.Add(n) }

// AddWritten 累加已成功写入的行数。
func (p *Progress) AddWritten(n int64) { p.written.Add(n) }

// AddFailed 累加失败的行数。
func (p *Progress) AddFailed(n int64) { p.failed.Add(n) }

// Counters 返回当前计数快照。
func (p *Progress) Counters() (read, written, failed int64) {
	return p.read.Load(), p.written.Load(), p.failed.Load()
}

// Elapsed 返回从 Start 起经过的时间。
func (p *Progress) Elapsed() time.Duration { return time.Since(p.start) }

// Rate 返回当前读写速率（条/秒）。
func (p *Progress) Rate() (readRate, writeRate float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ratesLocked(time.Now())
}

// Printf 在进度块“上方”安全打印一行内容，不会把进度条冲花。
func (p *Progress) Printf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.showBar && p.tty && p.drawn > 0 {
		// 先把进度块抹掉，写完再原样重画
		var sb strings.Builder
		fmt.Fprintf(&sb, "\x1b[%dA", p.drawn)
		for i := 0; i < p.drawn; i++ {
			sb.WriteString("\r\x1b[2K")
			if i < p.drawn-1 {
				sb.WriteString("\n")
			}
		}
		// 光标回到块首行
		sb.WriteString("\r")
		_, _ = os.Stdout.WriteString(sb.String())
		p.drawn = 0

		fmt.Fprintln(os.Stdout, msg)
		p.drawLocked(time.Now(), true)
		return
	}
	fmt.Fprintln(os.Stdout, msg)
}

// Finish 停止刷新并绘制最终状态。
func (p *Progress) Finish() {
	if !p.showBar {
		return
	}
	p.mu.Lock()
	if p.finished {
		p.mu.Unlock()
		return
	}
	p.finished = true
	p.mu.Unlock()

	close(p.stopCh)
	p.wg.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tty {
		p.drawLocked(time.Now(), true)
		if p.drawn > 0 {
			_, _ = os.Stdout.WriteString("\n")
		}
	} else {
		p.printPlainLine(true)
	}
}

// ------------------------------------------------------------------ 内部实现

func (p *Progress) tick(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}

	p.samples = append(p.samples, rateSample{t: now, read: p.read.Load(), written: p.written.Load()})
	for len(p.samples) > 2 && now.Sub(p.samples[0].t) > 5*time.Second {
		p.samples = p.samples[1:]
	}

	if p.tty {
		p.drawLocked(now, false)
		return
	}
	p.printPlainLine(false)
}

// drawLocked 重绘进度块，调用方需持有 p.mu。
func (p *Progress) drawLocked(now time.Time, force bool) {
	if !p.tty {
		return
	}
	readRate, writeRate := p.ratesLocked(now)
	lines := p.composeLines(readRate, writeRate, now)

	var sb strings.Builder
	if p.drawn > 0 {
		fmt.Fprintf(&sb, "\x1b[%dA", p.drawn)
	}
	for i, ln := range lines {
		sb.WriteString("\r\x1b[2K")
		sb.WriteString(ln)
		if i < len(lines)-1 {
			sb.WriteString("\n")
		}
	}
	_, _ = os.Stdout.WriteString(sb.String())
	p.drawn = len(lines)
	_ = force
}

// composeLines 生成进度块的四行文本。
func (p *Progress) composeLines(readRate, writeRate float64, now time.Time) []string {
	read, written, failed := p.Counters()
	total := p.total
	unknownTotal := total <= 0

	pct := 0.0
	if !unknownTotal {
		pct = float64(read) / float64(total) * 100
		if pct > 100 {
			pct = 100
		}
	}

	barW := 34
	line1 := fmt.Sprintf("  迁移中  %s → %s", p.source, p.target)

	var line2, line3 string
	if unknownTotal {
		// 源表行数统计失败或源表为空时退化为纯计数展示，不显示会误导的百分比
		line2 = fmt.Sprintf("  读取   已读取 %s 条（总数未知）", comma(read))
		line3 = fmt.Sprintf("  写入   已写入 %s 条    失败 %s 条", comma(written), comma(failed))
	} else {
		line2 = fmt.Sprintf("  读取   %s  %6.2f%%   %s / %s 条",
			renderBar(pct, barW), pct, comma(read), comma(total))
		line3 = fmt.Sprintf("  写入   %s  %6.2f%%   %s 条    失败 %s 条",
			renderBar(pct, barW), pct, comma(written), comma(failed))
	}

	eta := "--:--"
	switch {
	case unknownTotal:
	case read >= total:
		eta = "00:00"
	case readRate > 1:
		if remain := float64(total-read) / readRate; remain > 0 && remain < 24*3600 {
			eta = fmtDuration(time.Duration(remain * float64(time.Second)))
		}
	}

	line4 := fmt.Sprintf("  速度   读 %s 条/秒 · 写 %s 条/秒      已用 %s   剩余 %s",
		comma(int64(readRate)), comma(int64(writeRate)), fmtDuration(now.Sub(p.start)), eta)

	out := []string{line1, line2, line3, line4}
	for i, ln := range out {
		out[i] = truncate(ln, p.width)
	}
	return out
}

// printPlainLine 在非终端环境下退化为按百分比打点输出。
func (p *Progress) printPlainLine(final bool) {
	read, written, failed := p.Counters()
	total := p.total
	unknownTotal := total <= 0

	pct := 0
	if !unknownTotal {
		pct = int(float64(read) / float64(total) * 100)
		if pct > 100 {
			pct = 100
		}
	}
	if !final && pct < p.lastPct+5 && time.Since(p.lastLog) < 10*time.Second {
		return
	}
	if !final && pct == p.lastPct && time.Since(p.lastLog) < 10*time.Second {
		return
	}
	p.lastPct = pct
	p.lastLog = time.Now()

	elapsed := time.Since(p.start)
	rate := 0.0
	if sec := elapsed.Seconds(); sec > 0 {
		rate = float64(read) / sec
	}
	state := "进行中"
	if final {
		state = "已完成"
	}
	if unknownTotal {
		fmt.Printf("  [%s]  读取 %s 条  写入 %s 条  失败 %s 条  用时 %s  均速 %s 条/秒\n",
			state, comma(read), comma(written), comma(failed), fmtDuration(elapsed), comma(int64(rate)))
		return
	}
	fmt.Printf("  [%s] %3d%%  读取 %s/%s 条  写入 %s 条  失败 %s 条  用时 %s  均速 %s 条/秒\n",
		state, pct, comma(read), comma(total), comma(written), comma(failed),
		fmtDuration(elapsed), comma(int64(rate)))
}

func (p *Progress) ratesLocked(now time.Time) (readRate, writeRate float64) {
	if len(p.samples) >= 2 {
		first := p.samples[0]
		dt := now.Sub(first.t).Seconds()
		if dt > 0.2 {
			return float64(p.read.Load()-first.read) / dt, float64(p.written.Load()-first.written) / dt
		}
	}
	elapsed := now.Sub(p.start).Seconds()
	if elapsed < 0.5 {
		return 0, 0
	}
	return float64(p.read.Load()) / elapsed, float64(p.written.Load()) / elapsed
}

// ------------------------------------------------------------------ 工具函数

func renderBar(pct float64, width int) string {
	if width <= 0 {
		width = 20
	}
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

// comma 给整数加千分位。
func comma(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fmtDuration 把时长格式化成 HH:MM:SS。
func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d.Seconds())
	return fmt.Sprintf("%02d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

// isTerminal 判断是否是交互式终端；重定向到文件时返回 false。
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	if st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return true
}

func termWidth() int {
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 60 {
			return n
		}
	}
	return 110
}

// runeWidth 估算字符显示宽度，CJK 计 2 列。
func runeWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0x303E,
		r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x4E00 && r <= 0x9FFF,
		r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

// truncate 按显示宽度截断字符串，避免换行把进度块冲乱。
func truncate(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return s
	}
	w := 0
	for i, r := range s {
		w += runeWidth(r)
		if w > maxWidth {
			return s[:i]
		}
	}
	return s
}
