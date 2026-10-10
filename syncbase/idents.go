package syncbase

import (
	"fmt"
	"strings"
)

// IdentCase 控制达梦标识符在 SQL 中的大小写折叠方式。
//
// 达梦/Oracle 系的规则是：不加引号的标识符一律折叠为大写，加引号的则严格区分大小写。
// 项目给的建表 DDL 里混用了两种写法——表名和大多数字段是小写加引号，`"JOB"`、`"PASSWORD"`
// 却是大写加引号。因此默认用 as-is 严格照搬 DDL 中的大小写。
// 若实际库中对象是以未加引号方式创建的（即全部大写），改用 upper 即可。
type IdentCase string

const (
	// IdentAsIs 保持 DDL 中的原始大小写（默认）。
	IdentAsIs IdentCase = "as-is"
	// IdentUpper 统一转为大写。
	IdentUpper IdentCase = "upper"
	// IdentLower 统一转为小写。
	IdentLower IdentCase = "lower"
)

// ParseIdentCase 解析命令行传入的大小写策略。
func ParseIdentCase(s string) (IdentCase, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "as-is", "asis", "native":
		return IdentAsIs, nil
	case "upper", "up":
		return IdentUpper, nil
	case "lower", "low":
		return IdentLower, nil
	default:
		return "", fmt.Errorf("不支持的标识符大小写策略 %q，可选 as-is / upper / lower", s)
	}
}

func (c IdentCase) apply(name string) string {
	switch c {
	case IdentUpper:
		return strings.ToUpper(name)
	case IdentLower:
		return strings.ToLower(name)
	default:
		return name
	}
}

// QuoteIdent 按策略转换大小写并加上双引号。
// 双引号是达梦标识符里唯一需要转义的字符，这里直接拒绝，避免 SQL 注入面。
func QuoteIdent(name string, c IdentCase) string {
	n := strings.Trim(strings.TrimSpace(name), `"`)
	return `"` + strings.ReplaceAll(c.apply(n), `"`, `""`) + `"`
}

// QuoteIdents 批量处理列名。
func QuoteIdents(names []string, c IdentCase) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = QuoteIdent(n, c)
	}
	return out
}

// QualifyTable 处理 `schema.table` 形式的全限定表名，逐段加引号。
func QualifyTable(full string, c IdentCase) string {
	parts := strings.Split(strings.TrimSpace(full), ".")
	for i, p := range parts {
		parts[i] = QuoteIdent(p, c)
	}
	return strings.Join(parts, ".")
}
