package syncbase

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NullTime 兼容达梦驱动可能返回的多种时间表示：time.Time / []byte / string / nil。
// 标准库 sql.NullTime 只接受 time.Time，遇到字符串会直接报错，所以这里自行实现。
type NullTime struct {
	Time  time.Time
	Valid bool
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999 -07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02 15:04:05",
	"2006/01/02",
}

// Scan 实现 sql.Scanner。
func (n *NullTime) Scan(v any) error {
	switch t := v.(type) {
	case nil:
		n.Time, n.Valid = time.Time{}, false
		return nil
	case time.Time:
		n.Time, n.Valid = t, true
		return nil
	case string:
		return n.parse(t)
	case []byte:
		return n.parse(string(t))
	case int64:
		n.Time = time.Unix(t/1000, (t%1000)*int64(time.Millisecond))
		n.Valid = true
		return nil
	default:
		return n.parse(fmt.Sprint(v))
	}
}

func (n *NullTime) parse(s string) error {
	s = strings.TrimSpace(s)
	if s == "" || s == "<nil>" || s == "null" {
		n.Time, n.Valid = time.Time{}, false
		return nil
	}
	for _, layout := range timeLayouts {
		if ts, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			n.Time, n.Valid = ts, true
			return nil
		}
	}
	return fmt.Errorf("无法解析时间值 %q", s)
}

// Ptr 转成 *time.Time，空值返回 nil，可直接写入 bson 的 *time.Time 字段。
func (n NullTime) Ptr() *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time
	return &t
}

// Text 返回格式化后的时间字符串。
func (n NullTime) Text() string {
	if !n.Valid {
		return ""
	}
	return n.Time.Format("2006-01-02 15:04:05")
}

// Str 把可空字符串转成普通字符串，空值统一为 ""。
func Str(v sql.NullString) string {
	if v.Valid {
		return v.String
	}
	return ""
}

// NumStr 把可空整数转成字符串。
// 目标模型（identity.User.UserStatus / identity.Organization.SortNo）在 Go 侧是 string，
// 必须转成字符串写入，否则应用反序列化会因类型不匹配报错。
func NumStr(v sql.NullInt64) string {
	if v.Valid {
		return strconv.FormatInt(v.Int64, 10)
	}
	return ""
}

// Num 把可空整数转成 int，空值返回 0。
func Num(v sql.NullInt64) int {
	if v.Valid {
		return int(v.Int64)
	}
	return 0
}
