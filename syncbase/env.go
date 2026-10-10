// Package syncbase 提供达梦 → MongoDB 数据迁移所需的公共基础设施：
// 配置加载、数据库连接、实时进度展示、批量写入流水线。
//
// 具体到某张表的字段映射，由 demo/migrate_user、demo/migrate_org 各自实现。
package syncbase

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ============================================================== 配置文件读取

// env 表示一份解析完成的 .env：键保持原样大小写。
type env map[string]string

// get 返回第一个非空的配置值，用于同一含义的多个写法。
func (e env) get(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(e[k]); v != "" {
			return v
		}
	}
	return ""
}

// getInt 读取整数配置，缺失或格式非法时返回 def。
func (e env) getInt(def int, keys ...string) int {
	if v := e.get(keys...); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Config 是一次完整的连接配置。
type Config struct {
	DM    *DMConfig
	Mongo *MongoConfig
	// File 是实际使用的配置文件绝对路径，便于在启动信息里显示。
	File string
}

// LoadConfig 读取配置文件并组装达梦 + MongoDB 连接配置。
//
// envPath 为空时自动查找 .env，查找顺序：
//  1. 当前工作目录下的 demo/ 子目录，及其各级父目录
//  2. 当前工作目录，及其各级父目录
//
// 只接受「含数据库连接配置」的那份文件——项目根目录的 .env 存的是 SM2/SM4 密钥，
// 不含 DM_HOST / MONGO_HOST，会被自动跳过。
func LoadConfig(envPath string) (*Config, error) {
	e, file, err := loadEnv(envPath)
	if err != nil {
		return nil, err
	}

	dm, err := newDMConfig(e)
	if err != nil {
		return nil, fmt.Errorf("配置文件 %s: %w", file, err)
	}
	mg, err := newMongoConfig(e)
	if err != nil {
		return nil, fmt.Errorf("配置文件 %s: %w", file, err)
	}
	return &Config{DM: dm, Mongo: mg, File: file}, nil
}

// loadEnv 定位并解析配置文件。explicitPath 非空时直接用该路径，不做查找。
func loadEnv(explicitPath string) (env, string, error) {
	if explicitPath != "" {
		e, file, err := readEnvFile(explicitPath)
		if err != nil {
			return nil, "", err
		}
		if !hasDBConfig(e) {
			return nil, "", fmt.Errorf(
				"配置文件 %s 里没有 DM_HOST / MONGO_HOST，可能不是数据库配置", file)
		}
		return e, file, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", fmt.Errorf("获取当前工作目录失败: %w", err)
	}
	// demo/ 优先：从模块根目录运行时，迁移用的 .env 放在 demo/ 下
	for _, start := range []string{filepath.Join(cwd, "demo"), cwd} {
		if e, file, err := searchEnvUp(start); err == nil {
			return e, file, nil
		}
	}
	return nil, "", fmt.Errorf(
		"未找到含数据库配置的 .env（已从 %s 及其父目录查找），请用 -env 显式指定", cwd)
}

// searchEnvUp 从 dir 起逐级向上查找 .env，返回第一个含数据库连接配置的文件。
func searchEnvUp(dir string) (env, string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, "", err
	}
	for cur := abs; ; {
		p := filepath.Join(cur, ".env")
		if st, statErr := os.Stat(p); statErr == nil && !st.IsDir() {
			if e, _, readErr := readEnvFile(p); readErr == nil && hasDBConfig(e) {
				return e, p, nil
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil, "", fmt.Errorf("%s 及其父目录下没有含数据库配置的 .env", abs)
		}
		cur = parent
	}
}

// readEnvFile 解析一份 .env。
// 格式为手写的 key=value，不是 shell 脚本，不要写 export；# 或 ; 开头为注释。
func readEnvFile(path string) (env, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("解析配置文件路径失败: %w", err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, "", fmt.Errorf("打开配置文件失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	e := make(env)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		// 去掉 BOM，兼容 Windows 下编辑过的文件
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		e[key] = strings.Trim(stripComment(val), `"'`)
	}
	if err := sc.Err(); err != nil {
		return nil, "", fmt.Errorf("读取配置文件失败: %w", err)
	}

	applyEnvOverrides(e)
	return e, abs, nil
}

// stripComment 去掉行尾注释。被引号包裹的值视为字面量，原样保留。
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

// applyEnvOverrides 用同名系统环境变量覆盖 .env 中的配置项，便于临时切换目标库：
//
//	MONGO_DB=identity_test ./migrate_user -dry-run
//
// 只认 DM_ 与 MONGO_ 前缀，无需维护白名单，新增配置项自动生效。
func applyEnvOverrides(e env) {
	for _, kv := range os.Environ() {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || val == "" {
			continue
		}
		if strings.HasPrefix(key, "DM_") || strings.HasPrefix(key, "MONGO_") {
			e[key] = val
		}
	}
}

// hasDBConfig 判断解析结果里是否含数据库连接配置。
// 用于把项目根目录那份只放 SM2/SM4 密钥的 .env 排除掉。
func hasDBConfig(e env) bool {
	return e.get("DM_HOST") != "" || e.get("MONGO_HOST") != ""
}

// ================================================================ 达梦配置

// DMConfig 达梦数据库连接配置，对应 .env 中的 DM_* 项。
type DMConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	// Schema 是登录后的默认模式。留空表示不指定，由达梦沿用登录用户的默认模式。
	// 源表名通常已带模式前缀（如 DLPDATA.did_system_sync_user），此项一般无需设置。
	Schema string
}

func newDMConfig(e env) (*DMConfig, error) {
	c := &DMConfig{
		Host:     e.get("DM_HOST"),
		Port:     e.getInt(5236, "DM_PORT"),
		User:     e.get("DM_USER", "DM_USERNAME"),
		Password: e.get("DM_PASS", "DM_PASSWORD"),
		Schema:   e.get("DM_SCHEMA"),
	}
	if c.Host == "" {
		return nil, errors.New("缺少达梦连接地址 DM_HOST")
	}
	if c.User == "" {
		return nil, errors.New("缺少达梦用户名 DM_USER")
	}
	return c, nil
}

// Addr 返回 host:port（IPv6 会自动加方括号）。
func (c *DMConfig) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// DSN 构建达梦驱动可识别的连接串。
//
// 关键参数：
//   - rowPrefetch：每批从服务端预取的记录数，直接决定流式读取的内存占用与往返次数；
//   - connectTimeout：建连超时（毫秒）；
//   - schema：登录后的默认模式，留空则不下发该参数。
//
// 这里必须用**字面量拼接**，不能用 url.URL 来拼 —— 达梦驱动的 parseDSN
// （gorm-dameng/dm8/n.go）按「最后一个 @ 切账号、第一个 : 切密码」取值，
// 全程只做字符串切分，**不做 URL 解码**；而 url.URL.String() 会把密码里的
// @ ? : # % 空格 等字符转义成 %XX，于是驱动拿着 "P%40ss" 这种字符串去认证，
// 服务端的回应就是「用户名或密码错误」——账号密码明明是对的，却怎么也连不上。
//
// 字面量拼接是安全的：只要连接串带上查询参数（下面恒有 appName），
// 驱动用于切分 query 的是最后一个 ?，而密码里即使含 ? 也一定排在它前面，
// 因此密码中出现任何字符都不会被切错。
func (c *DMConfig) DSN(rowPrefetch int) string {
	q := url.Values{}
	q.Set("appName", "dm2mongo")
	q.Set("connectTimeout", "30000")
	if c.Schema != "" {
		q.Set("schema", c.Schema)
	}
	if rowPrefetch > 0 {
		q.Set("rowPrefetch", strconv.Itoa(rowPrefetch))
	}

	var b strings.Builder
	b.WriteString("dm://")
	if c.User != "" {
		b.WriteString(c.User)
		if c.Password != "" {
			b.WriteByte(':')
			b.WriteString(c.Password)
		}
		b.WriteByte('@')
	}
	b.WriteString(c.Addr())
	if qs := q.Encode(); qs != "" {
		b.WriteByte('?')
		b.WriteString(qs)
	}
	return b.String()
}

// SafeDSN 返回隐去密码的连接串，用于日志输出。任何情况下都不得输出明文密码。
func (c *DMConfig) SafeDSN() string {
	s := fmt.Sprintf("dm://%s:***@%s", c.User, c.Addr())
	if c.Schema != "" {
		s += "?schema=" + c.Schema
	}
	return s
}

// ============================================================== MongoDB 配置

// MongoConfig MongoDB 连接配置，对应 .env 中的 MONGO_* 项。
type MongoConfig struct {
	Host       string
	Port       int
	User       string
	Password   string
	Database   string
	AuthSource string
}

func newMongoConfig(e env) (*MongoConfig, error) {
	c := &MongoConfig{
		Host:       e.get("MONGO_HOST"),
		Port:       e.getInt(27017, "MONGO_PORT"),
		User:       e.get("MONGO_USERNAME", "MONGO_USER"),
		Password:   e.get("MONGO_PASSWORD"),
		Database:   e.get("MONGO_DB", "MONGO_DATABASE"),
		AuthSource: e.get("MONGO_AUTH_SOURCE"),
	}
	if c.Host == "" {
		return nil, errors.New("缺少 MongoDB 连接地址 MONGO_HOST")
	}
	if c.Database == "" {
		c.Database = "identity"
	}
	if c.AuthSource == "" {
		// MongoDB 的管理账号默认建在 admin 库
		c.AuthSource = "admin"
	}
	return c, nil
}

// Addr 返回 host:port（IPv6 会自动加方括号）。
func (c *MongoConfig) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// SafeURI 返回隐去密码的连接串，用于日志输出。
// 这里直接格式化，不做 URL 往返解析，避免 "***" 被编码成 %2A%2A%2A。
func (c *MongoConfig) SafeURI() string {
	if c.User == "" {
		return "mongodb://" + c.Addr()
	}
	return fmt.Sprintf("mongodb://%s:***@%s/?authSource=%s", c.User, c.Addr(), c.AuthSource)
}
