package syncbase

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Logger 把迁移过程中的明细写进日志文件，同时把错误同步打印到终端。
// 进度条会不断重绘终端，所以正常信息只进文件，避免把进度条冲花。
type Logger struct {
	mu      sync.Mutex
	file    *os.File
	verbose bool
	stderr  io.Writer
	sink    *Progress
}

// NewLogger 创建日志器。path 为空时只向终端输出错误。
func NewLogger(path string, verbose bool) (*Logger, error) {
	l := &Logger{verbose: verbose, stderr: os.Stderr}
	if path == "" {
		return l, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件 %s 失败: %w", path, err)
	}
	l.file = f
	return l, nil
}

// SetSink 绑定进度显示器。绑定后终端输出会先抹掉进度块再打印，避免把进度条冲花。
func (l *Logger) SetSink(p *Progress) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.sink = p
	l.mu.Unlock()
}

func (l *Logger) print(format string, a ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	sink := l.sink
	l.mu.Unlock()
	if sink != nil {
		sink.Printf(format, a...)
		return
	}
	fmt.Fprintf(l.stderr, format+"\n", a...)
}

// Close 关闭日志文件。
func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}

// Verbose 返回是否开启明细模式。
func (l *Logger) Verbose() bool { return l != nil && l.verbose }

func (l *Logger) write(level, format string, a ...any) {
	if l == nil {
		return
	}
	line := fmt.Sprintf("%s [%s] %s\n", time.Now().Format("2006-01-02 15:04:05.000"), level, fmt.Sprintf(format, a...))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_, _ = l.file.WriteString(line)
	}
}

// Infof 记录普通信息（只落文件；开启 -verbose 时同时打印到终端）。
func (l *Logger) Infof(format string, a ...any) {
	l.write("INFO", format, a...)
	if l != nil && l.verbose {
		l.print("  · "+format, a...)
	}
}

// Warnf 记录告警（落文件并打印终端）。
func (l *Logger) Warnf(format string, a ...any) {
	l.write("WARN", format, a...)
	l.print("  ! "+format, a...)
}

// Errorf 记录错误（落文件并打印终端）。
func (l *Logger) Errorf(format string, a ...any) {
	l.write("ERROR", format, a...)
	l.print("  ✗ "+format, a...)
}
