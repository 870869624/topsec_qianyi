// dmcheck —— 达梦数据库连接诊断工具（单文件、独立可执行）
//
// 用途：当迁移程序报「用户名或密码错误」「连接失败」时，用它把「连不上达梦」
// 拆成 6 个可独立验证的步骤，逐级定位问题究竟出在哪一步：
//
//  1. 配置文件定位      —— 程序到底读的是哪一份配置
//  2. 配置解析与字符体检 —— 读到的值有没有前后空格、不可见字符、被注释截断
//  3. 连接参数与 DSN     —— 密码被 URL 转义后，驱动实际拿去认证的是什么
//  4. 网络连通          —— DNS 解析 + TCP 建连
//  5. 驱动与身份认证     —— database/sql 建连 + Ping，并做人话错误分类
//  6. 连通性检查        —— 版本、当前用户、模式列表、源表是否存在
//
// 第 3 步会揭示一个很容易被忽略的坑：达梦 Go 驱动解析 DSN 时**不做 URL 解码**
// （见 dm8/n.go 的 parseDSN，只做字符串切分），而按 RFC 规范构造的 DSN 会把
// 密码里的 @ ? : # % 空格 等字符转义成 %XX。两者相遇的结果就是——
// 账号密码明明是对的，服务端却一直返回「用户名或密码错误」。
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "github.com/godoes/gorm-dameng/dm8" // 注册 database/sql 驱动，名字是 "dm"
)

const (
	appName = "dmcheck"
	rule    = "────────────────────────────────────────────────────────────────"
	ruleFat = "════════════════════════════════════════════════════════════════"
)

// ================================================================ 命令行参数

type cliOpts struct {
	envPath    string
	host       string
	port       int
	user       string
	pass       string
	schema     string
	table      string
	outPath    string
	timeout    time.Duration
	showSecret bool
	configOnly bool
}

func parseFlags() *cliOpts {
	// 达梦驱动会在包 init 里往 flag.CommandLine 注册自己的参数（如 DM_HOME），
	// 直接沿用会让帮助信息混入无关内容，这里换一个干净的标志集。
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	o := &cliOpts{}
	flag.StringVar(&o.envPath, "env", "", "配置文件路径；留空则自动查找（优先 ./dmcheck.env，其次 ./.env 并逐级向上）")
	flag.StringVar(&o.host, "host", "", "覆盖达梦地址，只影响本次诊断，不改配置文件")
	flag.IntVar(&o.port, "port", 0, "覆盖达梦端口")
	flag.StringVar(&o.user, "user", "", "覆盖达梦用户名")
	flag.StringVar(&o.pass, "pass", "", "覆盖达梦密码。排查利器：可绕过配置文件直接验证密码本身对不对")
	flag.StringVar(&o.schema, "schema", "", "覆盖模式名")
	flag.StringVar(&o.table, "table", "", "要检查的源表名，可带模式前缀；留空则检查两张默认源表")
	flag.StringVar(&o.outPath, "out", "dmcheck-report.txt", "诊断报告保存路径；填 - 表示只打印不存文件")
	flag.DurationVar(&o.timeout, "timeout", 15*time.Second, "每一步的超时时间")
	flag.BoolVar(&o.showSecret, "show-secret", false, "在报告中显示账号密码明文（默认脱敏）")
	flag.BoolVar(&o.configOnly, "config-only", false, "只做配置检查，不发起任何网络连接")
	flag.Parse()
	return o
}

// ================================================================== 报告输出

// Reporter 把内容同时写到终端和报告文件，便于现场把结果原样回传。
type Reporter struct {
	w          io.Writer
	showSecret bool
}

func newReporter(o *cliOpts) (*Reporter, func(), error) {
	var (
		f      *os.File
		w      io.Writer = os.Stdout
		closer           = func() {}
	)
	if o.outPath != "" && o.outPath != "-" {
		var err error
		f, err = os.Create(o.outPath)
		if err != nil {
			// 报告文件写不了不算致命错误，终端输出仍然有效
			fmt.Fprintf(os.Stderr, "! 无法创建报告文件 %s：%v（继续，仅打印到终端）\n", o.outPath, err)
		} else {
			w = io.MultiWriter(os.Stdout, f)
			closer = func() { _ = f.Close() }
		}
	}
	return &Reporter{w: w, showSecret: o.showSecret}, closer, nil
}

func (r *Reporter) p(format string, a ...any) { fmt.Fprintf(r.w, format, a...) }

func (r *Reporter) Banner() {
	r.p("%s\n 达梦连接诊断工具  %s\n", ruleFat, appName)
	r.p(" 时间 %s   运行环境 %s/%s   Go %s\n",
		time.Now().Format("2006-01-02 15:04:05"), runtime.GOOS, runtime.GOARCH, runtime.Version())
	r.p("%s\n", ruleFat)
}

func (r *Reporter) Step(n, total int, title string) {
	r.p("\n【%d/%d】%s\n%s\n", n, total, title, rule)
}

// Sub 输出步骤内的次级小节
func (r *Reporter) Sub(format string, a ...any) {
	r.p("\n  ▸ "+format+"\n", a...)
}

func (r *Reporter) OK(format string, a ...any)   { r.p("  ✓ "+format+"\n", a...) }
func (r *Reporter) Bad(format string, a ...any)  { r.p("  ✗ "+format+"\n", a...) }
func (r *Reporter) Warn(format string, a ...any) { r.p("  ! "+format+"\n", a...) }
func (r *Reporter) Info(format string, a ...any) { r.p("    "+format+"\n", a...) }

// Field 输出一条 key = value，用于配置回显
func (r *Reporter) Field(key, val string) {
	r.p("    %-14s = %s\n", key, val)
}

// mask 按 switch 决定显示明文还是脱敏值
func (r *Reporter) mask(secret string) string {
	if secret == "" {
		return "(未填写 / 空值)"
	}
	if r.showSecret {
		return fmt.Sprintf("%q（%d 个字符）", secret, utf8.RuneCountInString(secret))
	}
	return fmt.Sprintf("******（%d 个字符）", utf8.RuneCountInString(secret))
}

// ================================================================== 诊断结论

type finding struct {
	level  int // 0 正常 / 1 警告 / 2 致命
	title  string
	detail string
	advice []string
}

func (r *Reporter) Summary(fnd []finding) int {
	r.p("\n%s\n 诊 断 结 论\n%s\n", ruleFat, ruleFat)
	if len(fnd) == 0 {
		r.p("\n  没有发现任何问题。\n")
		return 0
	}

	ordered := make([]finding, len(fnd))
	copy(ordered, fnd)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].level > ordered[j].level })

	worst := 0
	fatal := 0
	for _, f := range ordered {
		mark, label := "✓", "正常"
		switch f.level {
		case 1:
			mark, label = "!", "警告"
		case 2:
			mark, label = "✗", "阻断"
		}
		if f.level > worst {
			worst = f.level
		}
		if f.level == 2 {
			fatal++
		}
		r.p("\n  %s [%s] %s\n", mark, label, f.title)
		if f.detail != "" {
			for _, ln := range strings.Split(f.detail, "\n") {
				r.p("      %s\n", ln)
			}
		}
		if len(f.advice) > 0 {
			r.p("\n      处理建议：\n")
			for i, a := range f.advice {
				r.p("        %d) %s\n", i+1, a)
			}
		}
	}

	r.p("\n%s\n", rule)
	switch {
	case fatal > 0:
		r.p(" 结论：连接未打通，共 %d 项阻断问题。请按上面的处理建议逐项排查。\n", fatal)
	case worst == 1:
		r.p(" 结论：连接已打通，但存在警告项，建议确认后再正式迁移。\n")
	default:
		r.p(" 结论：达梦连接完全正常，可以继续执行迁移程序。\n")
	}
	r.p("%s\n", ruleFat)
	return worst
}

// ============================================================== 达梦连接凭据

type dmCred struct {
	Host   string
	Port   int
	User   string
	Pass   string
	Schema string
}

func (c *dmCred) addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// escapedPass 返回密码在方式 A 的 DSN 里实际呈现的样子。
// 直接复用 url.UserPassword 的转义结果，保证与主程序完全一致。
func (c *dmCred) escapedPass() string {
	return strings.TrimPrefix(url.UserPassword("", c.Pass).String(), ":")
}

// query 返回达梦 DSN 的公共查询参数，与主程序保持一致。
func (c *dmCred) query() url.Values {
	q := url.Values{}
	q.Set("appName", appName)
	q.Set("connectTimeout", "30000")
	if c.Schema != "" {
		q.Set("schema", c.Schema)
	}
	return q
}

// dsnEscaped 复刻主程序 syncbase/env.go 中 DMConfig.DSN() 的构造方式：
// 交给 url.URL 自动转义。这也是问题所在——转义后驱动并不会解码。
func (c *dmCred) dsnEscaped() string {
	u := url.URL{
		Scheme:   "dm",
		User:     url.UserPassword(c.User, c.Pass),
		Host:     c.addr(),
		RawQuery: c.query().Encode(),
	}
	return u.String()
}

// dsnLiteral 把账号密码原样拼进 DSN，不做任何转义。
// 达梦驱动的 parseDSN 按「最后一个 @ 切账号、第一个 : 切密码」取值，
// 因此只要 DSN 带查询参数（appName 等），密码里出现任意字符都不会被切错。
func (c *dmCred) dsnLiteral() string {
	var b strings.Builder
	b.WriteString("dm://")
	if c.User != "" {
		b.WriteString(c.User)
		if c.Pass != "" {
			b.WriteByte(':')
			b.WriteString(c.Pass)
		}
		b.WriteByte('@')
	}
	b.WriteString(c.addr())
	if q := c.query().Encode(); q != "" {
		b.WriteByte('?')
		b.WriteString(q)
	}
	return b.String()
}

// displayDSN 手工拼一个「密码已脱敏、且不做任何转义」的连接串。
//
// 这里不能借 url.URL 来拼：它会把占位符里的 * 也一并转义成 %2A，
// 报告里出现 %2A%2A%2A 只会让人误以为连接串被搞坏了。
func (c *dmCred) displayDSN() string {
	var b strings.Builder
	b.WriteString("dm://")
	if c.User != "" {
		b.WriteString(c.User)
		if c.Pass != "" {
			b.WriteString(":******")
		}
		b.WriteByte('@')
	}
	b.WriteString(c.addr())
	if q := c.query().Encode(); q != "" {
		b.WriteByte('?')
		b.WriteString(q)
	}
	return b.String()
}

// ================================================================== 步骤 1

type candFile struct {
	path   string
	exists bool
	hasDB  bool
}

// stepLocate 定位配置文件，并把「所有候选文件」列出来。
// 现场最常见的一类事故就是：改对了 A 文件，程序读的却是 B 文件。
func stepLocate(r *Reporter, o *cliOpts, fnd *[]finding) (string, []candFile) {
	r.Step(1, 6, "配置文件定位")

	cwd, _ := os.Getwd()
	candidates := []string{
		filepath.Join(cwd, "dmcheck.env"),
		filepath.Join(cwd, ".env"),
		filepath.Join(cwd, "demo", ".env"),
		filepath.Join(filepath.Dir(cwd), ".env"),
	}

	var list []candFile
	for _, p := range candidates {
		c := candFile{path: p}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			c.exists = true
			if kv, err := readEnvFile(p); err == nil && hasDBConfig(kv) {
				c.hasDB = true
			}
		}
		list = append(list, c)
	}

	r.Sub("候选配置文件（按查找优先级排列）")
	for _, c := range list {
		switch {
		case !c.exists:
			r.Info("—   %s  （不存在）", c.path)
		case c.hasDB:
			r.Info("·   %s  （存在，含数据库配置）", c.path)
		default:
			r.Info("·   %s  （存在，但不含 DM_HOST，会被跳过）", c.path)
		}
	}

	// 确定实际使用哪一份
	var used string
	switch {
	case o.envPath != "":
		used = o.envPath
		r.p("\n")
		if _, err := os.Stat(used); err != nil {
			r.Bad("命令行指定的配置文件不可用：%v", err)
			*fnd = append(*fnd, finding{
				level:  2,
				title:  "配置文件不存在",
				detail: fmt.Sprintf("%s", used),
				advice: []string{"检查 -env 后面的路径是否正确，注意区分 Windows / Linux 的路径分隔符"},
			})
			return "", list
		}
		r.OK("-env 显式指定，使用 %s", used)
	default:
		for _, c := range list {
			if c.exists && c.hasDB {
				used = c.path
				break
			}
		}
		r.p("\n")
		if used == "" {
			r.Bad("没有找到任何含数据库连接配置的文件")
			*fnd = append(*fnd, finding{
				level:  2,
				title:  "找不到可用的配置文件",
				detail: "程序会在当前目录及上级目录查找 dmcheck.env / .env，且要求文件里含 DM_HOST",
				advice: []string{
					"把 dmcheck.env 与可执行文件放在同一目录",
					"或用 -env /完整/路径/dmcheck.env 显式指定",
					"注意：文件名必须严格是 dmcheck.env 或 .env，不要存成 dmcheck.env.txt",
				},
			})
			return "", list
		}
		r.OK("自动查找命中：%s", used)
	}

	if used != "" {
		if kv, err := readEnvFile(used); err == nil && !hasDBConfig(kv) {
			r.Warn("该文件里没有 DM_HOST / MONGO_HOST，可能不是数据库配置文件")
		}
	}
	return used, list
}

// ================================================================== 步骤 2

type envLine struct {
	no      int
	rawLine string // 该行原文，用于在报告里如实回显
	key     string
	valRaw  string // 等号右侧原文（未做任何处理）
	val     string // 最终生效值
	skip    string // 被跳过的原因
	note    string // 值被解析器处理过的说明
}

type parsedEnv struct {
	path     string
	kv       map[string]string
	lines    []envLine
	overrode []string // 被系统环境变量覆盖的键
}

// stepParse 解析配置并逐字符体检，重点检查那些「肉眼看不出、却能让认证失败」的值。
func stepParse(r *Reporter, path string) (*parsedEnv, *[]finding) {
	r.Step(2, 6, "配置解析与字符体检")

	var fnd []finding
	fail := func(title, detail string, advice ...string) {
		fnd = append(fnd, finding{level: 2, title: title, detail: detail, advice: advice})
	}

	if path == "" {
		r.Bad("上一步未确定配置文件，跳过")
		fail("无法解析配置", "配置文件没有定位成功")
		return nil, &fnd
	}

	pe, err := parseEnvFile(path)
	if err != nil {
		r.Bad("%v", err)
		fail("解析配置文件失败", err.Error(), "确认文件编码为 UTF-8，且当前用户有读取权限")
		return nil, &fnd
	}
	r.OK("已解析 %s", pe.path)
	r.Info("解析规则与主程序 migrate_user / migrate_org 完全一致")

	if len(pe.skipdLines()) > 0 {
		r.Sub("被跳过的行")
		for _, l := range pe.skipdLines() {
			r.Info("第 %d 行  %s   （%s）", l.no, l.skip, strconv.Quote(l.raw()))
		}
	}

	if nl := pe.notedLines(); len(nl) > 0 {
		r.Sub("值被解析器自动处理过的行（会悄悄改变值，务必核对）")
		for _, l := range nl {
			r.Info("第 %d 行  %s=%s", l.no, l.key, l.valRaw)
			r.Info("             生效值 %s", strconv.Quote(l.val))
			r.Info("             %s", l.note)
		}
		fnd = append(fnd, finding{
			level: 1,
			title: "有配置行的值被解析器改写",
			detail: "解析器会去掉行尾注释和值两侧的引号，如果密码里本来就有 # 或引号，\n" +
				"就会被当成语法标记吃掉，导致服务端收到的密码和你以为的不一样。",
			advice: []string{
				"逐行核对上面列出的「生效值」是不是你真正想要的值",
				"密码里确实需要 # 号时，确保 # 前面没有空格",
			},
		})
	}

	if len(pe.overrode) > 0 {
		r.Sub("被系统环境变量覆盖的配置项")
		for _, k := range pe.overrode {
			r.Warn("%s 被环境变量覆盖，配置文件里的值不生效", k)
		}
		fnd = append(fnd, finding{
			level:  1,
			title:  "配置被环境变量覆盖",
			detail: strings.Join(pe.overrode, "、") + " 被当前 shell 的环境变量覆盖了",
			advice: []string{
				"用 env | grep -E '^(DM_|MONGO_)' 查看",
				"unset DM_PASS 之后再运行，或用 -pass 显式指定",
			},
		})
	}
	return pe, &fnd
}

// ================================================================== 步骤 2b

// credFrom 组装连接凭据，同时做逐字符体检。
func credFrom(r *Reporter, pe *parsedEnv, o *cliOpts) (*dmCred, *[]finding) {
	var fnd []finding

	c := &dmCred{
		Host:   pe.get("DM_HOST"),
		Port:   pe.getInt(5236, "DM_PORT"),
		User:   pe.get("DM_USER"),
		Pass:   pe.get("DM_PASS"),
		Schema: pe.get("DM_SCHEMA"),
	}

	// 命令行覆盖：不写回配置文件，只作用于本次诊断
	var ov []string
	if o.host != "" {
		c.Host, ov = o.host, append(ov, "DM_HOST")
	}
	if o.port != 0 {
		c.Port, ov = o.port, append(ov, "DM_PORT")
	}
	if o.user != "" {
		c.User, ov = o.user, append(ov, "DM_USER")
	}
	if o.pass != "" {
		c.Pass, ov = o.pass, append(ov, "DM_PASS")
	}
	if o.schema != "" {
		c.Schema, ov = o.schema, append(ov, "DM_SCHEMA")
	}

	r.Sub("解析结果")
	r.Field("DM_HOST", strconv.Quote(c.Host))
	r.Field("DM_PORT", strconv.Itoa(c.Port))
	r.Field("DM_USER", strconv.Quote(c.User))
	r.Field("DM_PASS", r.mask(c.Pass))
	r.Field("DM_SCHEMA", func() string {
		if c.Schema == "" {
			return "(未设置，登录后使用用户默认模式)"
		}
		return strconv.Quote(c.Schema)
	}())
	if len(ov) > 0 {
		r.Info("")
		r.Warn("以下项由命令行覆盖：%s", strings.Join(ov, "、"))
	}

	var missing []string
	if c.Host == "" {
		missing = append(missing, "DM_HOST")
	}
	if c.User == "" {
		missing = append(missing, "DM_USER")
	}
	if len(missing) > 0 {
		r.p("\n")
		r.Bad("缺少必填项：%s", strings.Join(missing, "、"))
		fnd = append(fnd, finding{
			level:  2,
			title:  "配置项缺失",
			detail: "缺少 " + strings.Join(missing, "、"),
			advice: []string{"补齐配置文件后重跑；字段名要和 dmcheck.env 里的一致（区分大小写）"},
		})
	}

	r.Sub("字符体检（这些是肉眼很难发现的问题）")
	fnd = append(fnd, auditValue(r, "DM_USER", c.User)...)
	fnd = append(fnd, auditValue(r, "DM_PASS", c.Pass)...)

	return c, &fnd
}

// auditValue 逐字符检查一个配置值，把「隐形杀手」找出来。
func auditValue(r *Reporter, key, val string) []finding {
	if val == "" {
		return nil
	}
	var fnd []finding
	problems := 0

	// 0. 值中间的空格：可能是密码本身的一部分，也可能是不小心多打的
	if strings.Contains(val, " ") {
		r.Info("%s 含空格字符（若密码本身就含空格则属正常，否则要删掉）", key)
	}

	// 1. 前后空白：解析器一般已自动去掉，这里兜底
	if trimmed := strings.TrimSpace(val); trimmed != val {
		problems++
		r.Warn("%s 首尾有空白字符（去掉后长度 %d，现在是 %d）",
			key, utf8.RuneCountInString(trimmed), utf8.RuneCountInString(val))
		r.Info("完整形态：%s", strconv.Quote(val))
		fnd = append(fnd, finding{
			level: 2,
			title: key + " 首尾带有空白字符",
			detail: fmt.Sprintf("实际值 %s\n这几乎肯定是复制粘贴时带进来的，服务端会把它当成密码的一部分",
				strconv.Quote(val)),
			advice: []string{
				"打开配置文件，把该行删掉重新输入一遍（不要从聊天工具里复制）",
				"确认行尾没有多余空格，特别注意等号右侧",
			},
		})
	}

	// 2. 不可见 / 控制字符
	var invis []string
	for _, ch := range val {
		if ch == ' ' || ch == '\t' {
			continue
		}
		if ch < 0x20 || ch == 0x7f || (ch >= 0x80 && !unicode.IsPrint(ch)) {
			invis = append(invis, fmt.Sprintf("U+%04X", ch))
		}
	}
	if len(invis) > 0 {
		problems++
		r.Warn("%s 含 %d 个不可见字符：%s", key, len(invis), strings.Join(invis, " "))
		fnd = append(fnd, finding{
			level:  2,
			title:  key + " 含有不可见字符",
			detail: "检测到 " + strings.Join(invis, " ") + "，例如不间断空格 U+00A0、零宽空格 U+200B",
			advice: []string{"删除该行重新手输；这类字符多为从网页或聊天工具复制而来"},
		})
	}

	// 3. 非 ASCII（全角字母数字是最典型的坑）
	var wide []string
	for _, ch := range val {
		if ch > 0x7f && unicode.IsPrint(ch) {
			wide = append(wide, fmt.Sprintf("%c(U+%04X)", ch, ch))
			if len(wide) >= 8 {
				break
			}
		}
	}
	if len(wide) > 0 {
		problems++
		r.Warn("%s 含非 ASCII 字符：%s", key, strings.Join(wide, " "))
		fnd = append(fnd, finding{
			level:  1,
			title:  key + " 含有全角或中文字符",
			detail: strings.Join(wide, " "),
			advice: []string{
				"确认这是密码本身的一部分，而不是输入法留下的全角字符",
				"全角字母数字（如 ａｂｃ１２３）与半角在服务端是两个不同的字符",
			},
		})
	}

	// 4. 会被 URL 转义的字符 —— 这是本次诊断最关键的检查
	esc := escapedRunes(val)
	if len(esc) > 0 {
		var parts []string
		for _, e := range esc {
			parts = append(parts, fmt.Sprintf("%s → %s", runeLabel(e.r), e.encoded))
		}
		r.Warn("%s 含 %d 个会被 URL 转义的字符：%s", key, len(esc), strings.Join(parts, "、"))
		r.Info("详见第 3 步：这会让驱动拿转义后的字符串去认证")
		fnd = append(fnd, finding{
			level: 2,
			title: key + " 含有会被 URL 转义的字符",
			detail: "命中：" + strings.Join(parts, "、") + "\n" +
				"主程序用的是 url.URL 构造 DSN，它会把上面这些字符转义成 %XX，\n" +
				"而达梦 Go 驱动解析 DSN 时不做反向解码，于是服务端收到的密码与真实密码不一致。",
			advice: []string{
				"第 5 步会用「不转义」的方式自动复验一次，看是否正是这个原因",
				"若确认是这个原因，需要把主程序 syncbase/env.go 的 DSN() 改为字面量拼接",
			},
		})
	}

	switch {
	case problems > 0:
		// 上面已逐条列出，这里不再重复总结，避免出现自相矛盾的两行结论
	case len(esc) > 0:
		r.Info("%s 自身没有异常字符，但含会被 URL 转义的字符（见上）", key)
	default:
		r.OK("%s 干净：无首尾空白、无不可见字符、无需转义字符", key)
	}
	return fnd
}

// ================================================================== 步骤 3

// stepDSN 展示两种 DSN 构造方式的差异，指出驱动「实际收到」的密码是什么。
func stepDSN(r *Reporter, c *dmCred) []finding {
	r.Step(3, 6, "连接参数与 DSN 构造")

	var fnd []finding

	r.Sub("连接串（密码已脱敏）")
	r.Info("%s", c.displayDSN())

	// 用真实 DSN 做比较，不能用显示串：脱敏后两者恒等，比较就失去意义了
	dsnA, dsnB := c.dsnEscaped(), c.dsnLiteral()

	if dsnA == dsnB {
		r.Sub("两种构造方式的差异")
		r.OK("无差异 —— 账号密码里没有需要 URL 转义的字符，这不是问题所在")
		return fnd
	}

	nEsc := len(escapedRunes(c.User)) + len(escapedRunes(c.Pass))
	r.Sub("两种构造方式的差异")
	r.Warn("账号或密码里有 %d 种字符会被 URL 转义", nEsc)

	r.Sub("驱动实际拿去认证的密码")
	r.Info("方式 A：%s", r.mask(c.escapedPass()))
	r.Info("方式 B：%s", r.mask(c.Pass))
	r.Info("")
	r.Info("达梦 Go 驱动的 parseDSN 只做字符串切分（dm8/n.go），不做 URL 解码，")
	r.Info("所以方式 A 送到服务端的是转义后的字符串，并不是你的真实密码。")

	fnd = append(fnd, finding{
		level: 2,
		title: "密码被 URL 转义，服务端收到的是转义后的字符串",
		detail: fmt.Sprintf("真实密码经 URL 转义后变成 %s\n驱动不解码，认证时用的就是这个转义结果，因此服务端报「用户名或密码错误」",
			r.mask(c.escapedPass())),
		advice: []string{
			"第 5 步会自动用「不转义」的 DSN 复验一次来确认",
			"确认后需要把程序里构造 DSN 的地方改为字面量拼接（本工具 dsnLiteral 的写法）",
			"临时规避：改用不含 @ ? : # % 空格 等字符的密码",
		},
	})
	return fnd
}

// ================================================================== 步骤 4

// stepNetwork 分 DNS 与 TCP 两层验证网络，把「不在内网」「端口没开」区分开。
// 返回的 ok 为 false 时，说明网络层就没通，调用方应跳过后续认证环节。
func stepNetwork(r *Reporter, c *dmCred, o *cliOpts) ([]finding, bool) {
	r.Step(4, 6, "网络连通性")

	var fnd []finding
	host, port := c.Host, strconv.Itoa(c.Port)

	r.Sub("DNS / 主机名解析")
	addrs, err := net.DefaultResolver.LookupHost(context.Background(), host)
	if err != nil {
		// 直接写 IP 是正常做法，解析失败不算问题
		if net.ParseIP(host) != nil {
			r.OK("%s 是 IP 地址，无需解析", host)
		} else {
			r.Bad("无法解析主机名 %s：%v", host, err)
			fnd = append(fnd, finding{
				level:  2,
				title:  "主机名解析失败",
				detail: err.Error(),
				advice: []string{"改用 IP 地址填写 DM_HOST", "或确认 /etc/hosts、DNS 配置是否正确"},
			})
			return fnd, false
		}
	} else {
		r.OK("%s → %s", host, strings.Join(addrs, ", "))
		if len(addrs) > 1 {
			r.Info("解析出多个地址，注意是否连到了预期的那台")
		}
	}

	r.Sub("TCP 建连 " + net.JoinHostPort(host, port))
	start := time.Now()
	d := net.Dialer{Timeout: o.timeout}
	conn, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort(host, port))
	cost := time.Since(start)
	if err != nil {
		r.Bad("TCP 连接失败（耗时 %s）", cost.Round(time.Millisecond))
		r.Info("%v", err)
		title, detail, advice := classifyNetwork(err)
		fnd = append(fnd, finding{level: 2, title: title, detail: detail, advice: advice})
		return fnd, false
	}
	defer func() { _ = conn.Close() }()

	r.OK("TCP 连接成功，耗时 %s", cost.Round(time.Millisecond))
	r.Info("本地 %s  →  远端 %s", conn.LocalAddr(), conn.RemoteAddr())
	r.Info("这一层通了，说明网络和端口没有问题，问题只可能在认证或库内对象上")
	return fnd, true
}

// ================================================================== 步骤 5

// stepAuth 做真正的数据库认证，并在失败时按「主程序方式 → 字面量方式」的顺序复验。
func stepAuth(r *Reporter, c *dmCred, o *cliOpts) (*sql.DB, []finding) {
	r.Step(5, 6, "达梦驱动与身份认证")

	var fnd []finding

	r.Sub("驱动加载")
	drivers := sql.Drivers()
	if !contains(drivers, "dm") {
		r.Bad(`database/sql 未注册名为 "dm" 的驱动，已注册：%v`, drivers)
		fnd = append(fnd, finding{
			level:  2,
			title:  "达梦驱动未加载",
			detail: fmt.Sprintf("已注册驱动：%v", drivers),
			advice: []string{"程序编译时缺少 github.com/godoes/gorm-dameng/dm8 的匿名导入，需要重新编译"},
		})
		return nil, fnd
	}
	r.OK(`驱动 "dm" 已注册`)
	r.Info("%s", driverVersion())

	// ---- 尝试 1：URL 转义的 DSN（更符合 URL 规范，但达梦驱动不解码）
	r.Sub("尝试 1：用 URL 转义的 DSN 连接")
	db, err := pingDM(c.dsnEscaped(), o.timeout)
	if err == nil {
		r.OK("认证成功，账号密码正确")
		if len(escapedRunes(c.Pass)) > 0 {
			r.Warn("注意：密码含特殊字符，本次却成功了，说明转义恰好没有影响 —— 但仍然建议修掉这个隐患")
		}
		return db, fnd
	}

	r.Bad("认证失败")
	r.Info("%v", err)
	title, detail, advice := classifyDB(err)
	authFail := finding{level: 2, title: title, detail: detail, advice: advice}

	// ---- 尝试 2：只有在密码确实含会被转义的字符时才复验，避免多余的错误登录
	esc := escapedRunes(c.Pass)
	if len(esc) == 0 {
		r.p("\n")
		r.Info("密码里没有需要转义的字符，因此不存在「转义导致认证失败」的可能，")
		r.Info("不做第二次尝试（重复失败登录可能触发账号锁定）。")
		fnd = append(fnd, authFail)
		return nil, fnd
	}

	r.Sub("尝试 2：改用字面量 DSN（密码不转义）复验")
	r.Info("目的：确认失败是否由 URL 转义引起。这是本周期的第 2 次登录尝试。")
	db2, err2 := pingDM(c.dsnLiteral(), o.timeout)
	if err2 == nil {
		r.OK("认证成功！")
		r.p("\n")
		r.Warn("根因确认：失败与账号密码本身无关，而是 DSN 里的密码被 URL 转义了。")
		fnd = append(fnd, finding{
			level: 2,
			title: "根因确认：密码被 URL 转义导致认证失败",
			detail: "同一份账号密码，\n" +
				"  用 URL 转义的 DSN        → 认证失败\n" +
				"  用字面量的 DSN（不转义） → 认证成功\n" +
				"说明服务端收到的密码被 %XX 转义序列污染了。",
			advice: []string{
				"修复主程序 syncbase/env.go 的 DSN()，改为字面量拼接",
				"或临时改用不含 @ ? : # % 空格 等字符的密码",
				"修复前不要反复重跑迁移程序，避免不必要的失败登录",
			},
		})
		_ = db2.Close()
		return nil, fnd
	}

	r.Bad("仍然失败")
	r.Info("%v", err2)
	r.Info("两种构造方式都失败，说明问题不在 URL 转义上。")
	fnd = append(fnd, authFail)
	return nil, fnd
}

// ================================================================== 步骤 6

// stepProbe 连接成功后确认库内情况：版本、当前用户、模式、源表。
func stepProbe(r *Reporter, db *sql.DB, c *dmCred, o *cliOpts) []finding {
	r.Step(6, 6, "库内连通性检查")

	var fnd []finding
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	r.Sub("基础查询")
	if v, err := queryOne(ctx, db, "SELECT 1 FROM DUAL"); err != nil {
		r.Bad("SELECT 1 FROM DUAL 失败：%v", err)
		fnd = append(fnd, finding{
			level:  2,
			title:  "基础查询失败",
			detail: err.Error(),
			advice: []string{"能认证但不让查询，通常是账号被限制或权限不足，联系 DBA 确认"},
		})
		return fnd
	} else {
		r.OK("SELECT 1 FROM DUAL → %s", v)
	}

	if v := queryFirstOK(ctx, db,
		"SELECT USER FROM DUAL",
		"SELECT CURRENT_USER FROM DUAL",
		"SELECT SF_GET_USER() FROM DUAL",
	); v != "" {
		r.OK("当前登录用户 → %s", v)
	} else {
		r.Info("未能取得当前登录用户（各版本内置函数名略有差异，不影响迁移）")
	}

	if v := queryFirstOK(ctx, db, "SELECT BUILD_VERSION FROM V$INSTANCE", "SELECT * FROM V$VERSION"); v != "" {
		r.OK("数据库版本 → %s", shorten(v, 100))
	} else {
		r.Info("未能取得版本号（可能是账号缺少 V$ 视图权限，不影响迁移）")
	}

	r.Sub("模式列表")
	if v, err := queryColumn(ctx, db, "SELECT DISTINCT OWNER FROM ALL_TABLES ORDER BY OWNER"); err != nil {
		r.Warn("查询模式列表失败：%v", err)
	} else {
		r.Info("%s", strings.Join(v, "  "))
		if c.Schema != "" && !contains(v, c.Schema) {
			r.Bad("配置里的模式 %s 不在上表中 —— 这个实例上没有这个模式", c.Schema)
			fnd = append(fnd, finding{
				level:  2,
				title:  fmt.Sprintf("模式 %s 不存在", c.Schema),
				detail: "可用的模式： " + strings.Join(v, " "),
				advice: []string{
					"确认 DM_SCHEMA 是否写错（大小写敏感）",
					"确认迁移的源库地址是否填成了另一套环境",
				},
			})
		}
	}

	// 源表检查
	r.Sub("源表检查")
	tables := []string{"did_system_sync_user", "did_system_sync_org"}
	if o.table != "" {
		tables = []string{o.table}
	}
	for _, t := range tables {
		owner, name := splitTable(t)
		if owner == "" {
			owner = c.Schema
		}
		rows, err := queryAll(ctx, db,
			"SELECT OWNER, TABLE_NAME FROM ALL_TABLES WHERE UPPER(TABLE_NAME) = ?", strings.ToUpper(name))
		if err != nil {
			r.Warn("%s 查询失败：%v", name, err)
			continue
		}
		switch {
		case len(rows) == 0:
			r.Bad("未找到表 %s", name)
			fnd = append(fnd, finding{
				level:  1,
				title:  fmt.Sprintf("源表 %s 不存在", name),
				detail: fmt.Sprintf("在整库范围内都没找到名为 %s 的表", name),
				advice: []string{
					"确认连的是不是存放源表的那个达梦实例",
					"用 -table 指定实际表名再试一次",
				},
			})
		case owner != "" && !containsAny(rows, owner):
			r.Warn("表 %s 存在，但不在模式 %s 下：%s", name, owner, strings.Join(rows, " / "))
			r.Info("确认好后可用 -table 模式名.表名 指定")
		default:
			full := fmt.Sprintf("%s.%s", strings.SplitN(rows[0], ".", 2)[0], name)
			if cnt, err := queryOne(ctx, db, "SELECT COUNT(*) FROM "+quoteTable(rows[0])); err == nil {
				r.OK("%s 存在，共 %s 条记录", strings.Join(rows, " / "), cnt)
			} else {
				r.OK("%s 存在（统计条数失败：%v）", full, err)
			}
		}
	}
	return fnd
}

// =================================================================== 主流程

func main() {
	o := parseFlags()

	rep, closeRep, err := newReporter(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(2)
	}
	defer closeRep()

	rep.Banner()

	var fnd []finding

	path, _ := stepLocate(rep, o, &fnd)

	pe, f2 := stepParse(rep, path)
	fnd = append(fnd, *f2...)
	if pe == nil {
		rep.Summary(fnd)
		os.Exit(1)
	}

	cred, f3 := credFrom(rep, pe, o)
	fnd = append(fnd, *f3...)
	if cred.Host == "" || cred.User == "" {
		rep.Summary(fnd)
		os.Exit(1)
	}

	fnd = append(fnd, stepDSN(rep, cred)...)

	if o.configOnly {
		rep.p("\n%s\n", rule)
		rep.Info("-config-only 已指定，跳过所有网络与数据库检查。")
		code := rep.Summary(fnd)
		if code >= 2 {
			os.Exit(1)
		}
		return
	}

	netFind, netOK := stepNetwork(rep, cred, o)
	fnd = append(fnd, netFind...)

	if !netOK {
		rep.p("\n")
		rep.Info("网络层没有打通，跳过后续的认证与库内检查。")
		rep.Info("（网络不通时去试认证，得到的只会是误导性的错误）")
	} else {
		db, f5 := stepAuth(rep, cred, o)
		fnd = append(fnd, f5...)
		if db != nil {
			defer func() { _ = db.Close() }()
			fnd = append(fnd, stepProbe(rep, db, cred, o)...)
		}
	}

	rep.p("\n")
	if o.outPath != "" && o.outPath != "-" {
		rep.Info("完整报告已保存到 %s", o.outPath)
	}
	rep.Info("回传时请把这份报告整体发出，不要只截取报错那一行。")

	if rep.Summary(fnd) >= 2 {
		os.Exit(1)
	}
}

// ================================================================ 配置解析层
// 以下解析逻辑刻意复刻 syncbase/env.go，保证诊断结论与主程序行为一致。

func readEnvFile(path string) (map[string]string, error) {
	pe, err := parseEnvFile(path)
	if err != nil {
		return nil, err
	}
	return pe.kv, nil
}

func hasDBConfig(kv map[string]string) bool {
	return strings.TrimSpace(kv["DM_HOST"]) != "" || strings.TrimSpace(kv["MONGO_HOST"]) != ""
}

func parseEnvFile(path string) (*parsedEnv, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析配置文件路径失败: %w", err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("打开配置文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	pe := &parsedEnv{path: abs, kv: make(map[string]string)}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for no := 1; sc.Scan(); no++ {
		raw := sc.Text()
		line := strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
		switch {
		case line == "":
			pe.lines = append(pe.lines, envLine{no: no, rawLine: raw, skip: "空行"})
			continue
		case line[0] == '#' || line[0] == ';':
			pe.lines = append(pe.lines, envLine{no: no, rawLine: raw, skip: "注释"})
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			pe.lines = append(pe.lines, envLine{no: no, rawLine: raw, skip: "没有等号，格式不合法"})
			continue
		}
		noComment := stripComment(val)
		final := strings.Trim(noComment, `"'`)

		// 把解析器的两次「自动容错」显式记下来：
		// 它们会悄悄改变值，而这恰恰是现场最容易踩的坑。
		note := ""
		switch {
		case noComment != strings.TrimSpace(val):
			note = "行尾注释被去掉，只有 # 或 ; 之前的部分才是有效值"
		case final != noComment:
			note = "值两侧的引号被自动去掉"
		}

		pe.kv[key] = final
		pe.lines = append(pe.lines, envLine{no: no, rawLine: raw, key: key, valRaw: val, val: final, note: note})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	// 与主程序一致：同名系统环境变量覆盖配置文件
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		if strings.HasPrefix(k, "DM_") || strings.HasPrefix(k, "MONGO_") {
			if old, exists := pe.kv[k]; !exists || old != v {
				pe.kv[k] = v
				pe.overrode = append(pe.overrode, k)
			}
		}
	}
	return pe, nil
}

func stripComment(val string) string {
	v := strings.TrimSpace(val)
	if v == "" || v[0] == '"' || v[0] == '\'' {
		return v
	}
	for _, mark := range []string{" #", "\t#", " ;"} {
		if i := strings.Index(v, mark); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
	}
	return v
}

func (p *parsedEnv) get(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(p.kv[k]); v != "" {
			return v
		}
	}
	return ""
}

func (p *parsedEnv) getInt(def int, keys ...string) int {
	if v := p.get(keys...); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func (p *parsedEnv) skipdLines() []envLine {
	var out []envLine
	for _, l := range p.lines {
		if l.skip != "" && l.skip != "空行" && l.skip != "注释" {
			out = append(out, l)
		}
	}
	return out
}

// notedLines 返回「值被解析器处理过」的行，用于提醒现场核对生效值。
func (p *parsedEnv) notedLines() []envLine {
	var out []envLine
	for _, l := range p.lines {
		if l.note != "" {
			out = append(out, l)
		}
	}
	return out
}

func (l envLine) raw() string { return l.rawLine }

// ============================================================== 字符与工具函数

type escChar struct {
	r       rune
	encoded string
}

// escapedRunes 找出会被 url.UserPassword 转义的字符。
// 直接拿标准库逐个试，保证与主程序 DSN() 的转义行为完全一致。
func escapedRunes(s string) []escChar {
	var out []escChar
	seen := make(map[rune]bool)
	for _, r := range s {
		if seen[r] {
			continue
		}
		seen[r] = true
		enc := url.UserPassword("", string(r)).String() // 形如 ":a" 或 ":%40"
		if enc != ":"+string(r) {
			out = append(out, escChar{r: r, encoded: strings.TrimPrefix(enc, ":")})
		}
	}
	return out
}

func runeLabel(r rune) string {
	switch r {
	case ' ':
		return "空格"
	case '\t':
		return `\t`
	case '\n':
		return `\n`
	case '\r':
		return `\r`
	}
	switch {
	case r < 0x20 || r == 0x7f:
		return fmt.Sprintf(`\x%02X`, r)
	case !unicode.IsPrint(r):
		return fmt.Sprintf(`\u%04X`, r)
	}
	return "'" + string(r) + "'"
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func containsAny(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func splitTable(t string) (owner, name string) {
	if i := strings.Index(t, "."); i >= 0 {
		return strings.Trim(t[:i], `"`), strings.Trim(t[i+1:], `"`)
	}
	return "", strings.Trim(t, `"`)
}

// quoteTable 把 OWNER.TABLE_NAME 转成可安全写进 SQL 的带引号形式。
func quoteTable(full string) string {
	parts := strings.SplitN(full, ".", 2)
	if len(parts) == 1 {
		return `"` + parts[0] + `"`
	}
	return `"` + parts[0] + `"."` + parts[1] + `"`
}

func shorten(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// ================================================================ 数据库操作

func pingDM(dsn string, timeout time.Duration) (*sql.DB, error) {
	db, err := sql.Open("dm", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func queryOne(ctx context.Context, db *sql.DB, q string) (string, error) {
	row := db.QueryRowContext(ctx, q)
	var v any
	if err := row.Scan(&v); err != nil {
		return "", err
	}
	return fmt.Sprint(v), nil
}

func queryColumn(ctx context.Context, db *sql.DB, q string) ([]string, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return out, err
		}
		out = append(out, fmt.Sprint(v))
	}
	return out, rows.Err()
}

// queryAll 把每行拼成 "A.B" 形式的字符串列表。
func queryAll(ctx context.Context, db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var a, b string
		if err := rows.Scan(&a, &b); err != nil {
			return out, err
		}
		out = append(out, a+"."+b)
	}
	return out, rows.Err()
}

// queryFirstOK 依次尝试多条 SQL，返回第一条成功且非空的结果。
// 达梦不同版本的内置函数与视图名称有差异，用这种方式兜底最省事。
func queryFirstOK(ctx context.Context, db *sql.DB, queries ...string) string {
	for _, q := range queries {
		if v, err := queryOne(ctx, db, q); err == nil && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func driverVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "驱动版本：未知"
	}
	for _, d := range bi.Deps {
		if d.Path == "github.com/godoes/gorm-dameng" {
			return "驱动版本：" + d.Path + " " + d.Version
		}
	}
	return "驱动版本：github.com/godoes/gorm-dameng（未记录）"
}

// ================================================================ 错误分类器

type classifyRule struct {
	title  string
	detail string
	advice []string
	hit    func(lower string) bool
}

func matchAny(lower string, keys ...string) func(string) bool {
	return func(s string) bool {
		for _, k := range keys {
			if strings.Contains(s, k) {
				return true
			}
		}
		return false
	}
}

// classifyDB 把数据库返回的错误翻译成「是什么问题 + 怎么办」。
func classifyDB(err error) (string, string, []string) {
	msg := err.Error()
	l := strings.ToLower(msg)

	// 达梦常见错误码与文案，注意不同版本措辞略有差异
	rules := []classifyRule{
		{
			title:  "服务端拒绝认证：用户名或密码错误",
			detail: "服务端明确返回了认证失败。注意：这个提示只说明「收到的那对账号密码不匹配」，并不代表你写在配置文件里的密码是错的。",
			advice: []string{
				"先看第 3 步的 DSN 对比：如果两种构造方式不同，很可能是密码被 URL 转义了",
				"用 -pass '真实密码' 直接指定密码再跑一次，绕过配置文件验证密码本身",
				"确认用户名大小写：达梦默认区分大小写，SYSDBA 与 sysdba 是两个账号",
				"确认这个账号没有因多次失败登录被锁定（联系 DBA 查 FAILED_LOGIN_ATTEMPTS）",
			},
			hit: matchAny(l, "用户名或密码错误", "-2501", "invalid username", "invalid password",
				"authentication failed", "login failed", "log in failed", "授权失败", "凭证"),
		},
		{
			title:  "账号已被锁定",
			detail: "反复用错误密码登录会触发达梦的失败次数上限，账号被临时锁定。此时即使密码改对了也连不上。",
			advice: []string{
				"联系 DBA 解锁：ALTER USER 用户名 ACCOUNT UNLOCK",
				"解锁前不要再反复重试，否则会重新计时",
			},
			hit: matchAny(l, "锁定", "locked", "-2507"),
		},
		{
			title:  "密码已过期",
			detail: "账号密码超过有效期，需要先改密码。",
			advice: []string{"联系 DBA 重置密码后更新配置文件"},
			hit:    matchAny(l, "密码已过期", "密码过期", "expired", "-2508"),
		},
		{
			title:  "网络通信异常",
			detail: "TCP 层能连上，但认证握手阶段通信中断。常见于端口被中间设备重置、达梦服务未完全就绪、或驱动与服务器版本不匹配。",
			advice: []string{
				"确认 5236 端口没有被防火墙做深度拦截",
				"在数据库服务器上用 disql 本地连一次，排除服务端问题",
				"确认驱动版本与达梦版本兼容（当前驱动 gorm-dameng v0.7.2）",
			},
			hit: matchAny(l, "6001", "网络通信异常", "communication", "connection reset", "broken pipe", "eof"),
		},
		{
			title:  "连接超时",
			detail: "建连或认证阶段超时。",
			advice: []string{
				"加大超时：-timeout 60s",
				"确认网络链路稳定，不要经过不稳定的代理或 VPN",
			},
			hit: matchAny(l, "timeout", "超时", "deadline exceeded"),
		},
		{
			title:  "达到最大会话数",
			detail: "数据库侧的连接数已经用满，新的连接被拒绝。",
			advice: []string{"联系 DBA 查看 V$SESSIONS 并释放空闲连接", "错峰执行迁移"},
			hit:    matchAny(l, "最大会话", "会话数", "exceed", "too many", "maximum number of"),
		},
		{
			title:  "无效的模式名",
			detail: "配置里的 DM_SCHEMA 在这个实例上不存在。",
			advice: []string{
				"留空 DM_SCHEMA，由数据库使用登录用户的默认模式",
				"或改成该实例真实存在的模式名（第 6 步会列出全部模式）",
			},
			hit: matchAny(l, "无效的模式名", "-2103", "invalid schema"),
		},
		{
			title:  "DSN 格式不合法",
			detail: "连接串没能通过驱动自身的解析。多半是密码或用户名里含有把连接串「拼坏」的字符。",
			advice: []string{
				"这正是本工具第 3 步要暴露的问题——确认密码里是否含 @ ? : # 等字符",
				"若确认，需要把主程序的 DSN 构造改为字面量拼接",
			},
			hit: matchAny(l, "dsn_invalid", "invalid dsn", "连接串", "格式错误"),
		},
	}

	for _, r := range rules {
		if r.hit(l) {
			return r.title, r.detail + "\n原始错误：" + msg, r.advice
		}
	}
	return "数据库连接失败（未归类的错误）",
		"原始错误：" + msg,
		[]string{
			"把这份报告整体回传给开发方",
			"同时确认：用 dbeaver / disql 等工具用同样的账号密码能否连上",
		}
}

func classifyNetwork(err error) (string, string, []string) {
	l := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(l, "i/o timeout") ||
		strings.Contains(l, "timeout"):
		return "TCP 连接超时（网络不通或被防火墙丢包）",
			"原始错误：" + err.Error(),
			[]string{
				"确认这台机器和达梦服务器在同一内网，或已连上 VPN",
				"在能连通的机器上用 telnet 达梦IP 达梦端口 交叉验证一次",
				"确认目标端口没有被防火墙策略 DROP（表现为超时而不是拒绝）",
			}
	case strings.Contains(l, "connection refused") || strings.Contains(l, "actively refused"):
		return "TCP 连接被拒绝（端口没有服务在监听）",
			"原始错误：" + err.Error(),
			[]string{
				"确认达梦实例已启动",
				"确认端口号正确：达梦默认 5236，注意不要写成旧环境的 5237",
			}
	case strings.Contains(l, "no route to host"):
		return "目标主机不可达（路由问题）",
			"原始错误：" + err.Error(),
			[]string{"确认 IP 地址写对了", "确认本机路由表与网段可达性"}
	case strings.Contains(l, "network is unreachable"):
		return "网络不可达（本机没有到达该网段的路由）",
			"原始错误：" + err.Error(),
			[]string{"通常是没连内网或 VPN 未生效，先确认网络环境"}
	case strings.Contains(l, "no such host"):
		return "主机名无法解析",
			"原始错误：" + err.Error(),
			[]string{"把 DM_HOST 改成 IP 地址"}
	}
	return "TCP 连接失败",
		"原始错误：" + err.Error(),
		[]string{"把这份报告整体回传，并确认本机到数据库服务器的网络策略"}
}
