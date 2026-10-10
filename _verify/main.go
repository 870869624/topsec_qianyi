// Command _verify 抽查迁移结果，重点验证易错字段。
// 临时排查工具，用完即删。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"git.cloud.top/ngdlp/server/demo/syncbase"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func main() {
	cfg, err := syncbase.LoadConfig("")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	client, err := syncbase.OpenMongo(ctx, cfg.Mongo, 20*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = client.Disconnect(context.Background()) }()

	db := client.Database(cfg.Mongo.Database)

	fmt.Println("============ 用户表抽查 ============")
	dumpMany(ctx, db.Collection("user"), "uid", []string{"U000001", "U000013", "U000007"})

	fmt.Println("\n============ 组织表抽查 ============")
	dumpMany(ctx, db.Collection("organization"), "uid", []string{"O000001", "O000007"})

	fmt.Println("\n============ 字段类型 / 命名自检 ============")
	checkUser(ctx, db)
	checkOrg(ctx, db)
}

func dumpMany(ctx context.Context, c *mongo.Collection, key string, vals []string) {
	for _, v := range vals {
		var doc bson.M
		if err := c.FindOne(ctx, bson.M{key: v}).Decode(&doc); err != nil {
			fmt.Printf("  取 %s=%s 失败: %v\n", key, v, err)
			continue
		}
		b, _ := json.MarshalIndent(doc, "  ", "  ")
		fmt.Printf("  --- %s = %s ---\n  %s\n", key, v, string(b))
	}
}

type report struct {
	fail int
	ok   int
}

func (r *report) assert(cond bool, desc, detail string) {
	if cond {
		r.ok++
		fmt.Printf("  ✓ %s\n", desc)
		return
	}
	r.fail++
	fmt.Printf("  ✗ %s  —— %s\n", desc, detail)
}

func checkUser(ctx context.Context, db *mongo.Database) {
	r := &report{}
	c := db.Collection("user")

	// 1. user_status 必须是字符串（源表是 INT）
	var intStatus int64
	err := c.FindOne(ctx, bson.M{"uid": "U000001", "user_status": bson.M{"$type": "string"}}).Decode(&bson.M{})
	r.assert(err == nil, "user_status 是字符串类型", "查不到 user_status 为字符串的文档")

	// 确认库里确实没有数字型的 user_status
	intStatus, err = c.CountDocuments(ctx, bson.M{"user_status": bson.M{"$type": "int"}})
	r.assert(err == nil && intStatus == 0, "不存在数字型 user_status（无类型污染）",
		fmt.Sprintf("发现 %d 条数字型", intStatus))

	// 2. 组织表字段名应为 linke_man_email，不应出现 linkeman_email
	nLinkemanEmail, e1 := c.CountDocuments(ctx, bson.M{"linkeman_email": bson.M{"$exists": true}})
	r.assert(e1 == nil && nLinkemanEmail == 0, "用户表不含 linkeman_email 字段",
		fmt.Sprintf("%d 条", nLinkemanEmail))

	// 3. serial_num 应为空字符串
	var doc bson.M
	if err := c.FindOne(ctx, bson.M{"uid": "U000001"}).Decode(&doc); err == nil {
		s, ok := doc["serial_num"]
		r.assert(ok && s == "", "serial_num 是空字符串", fmt.Sprintf("实际值: %v (%T)", s, s))
	}

	// 4. NULL 时间应写成 null 而不是零值时间
	var nullTimeCount int64
	nullTimeCount, _ = c.CountDocuments(ctx, bson.M{"create_time": nil})
	nullTimeCount2, _ := c.CountDocuments(ctx, bson.M{"ods_insert_time": nil})
	r.assert(nullTimeCount > 0 && nullTimeCount2 > 0,
		fmt.Sprintf("NULL 时间字段写成了 null（create_time %d 条、ods_insert_time %d 条为空）",
			nullTimeCount, nullTimeCount2), "没有发现 null 时间，NULL 可能被写成了零值时间")

	// 5. 中文与特殊字符应完整保留
	var cn bson.M
	if err := c.FindOne(ctx, bson.M{"uid": "U000017"}).Decode(&cn); err == nil {
		comments, _ := cn["comments"].(string)
		r.assert(len(comments) > 0, "含引号/反斜杠的 comments 已写入",
			fmt.Sprintf("comments=%q", comments))
	}
	var cn2 bson.M
	if err := c.FindOne(ctx, bson.M{"uid": "U000023"}).Decode(&cn2); err == nil {
		comments, _ := cn2["comments"].(string)
		r.assert(len([]rune(comments)) >= 360, fmt.Sprintf("超长 comments 完整保留（%d 字）", len([]rune(comments))),
			fmt.Sprintf("只有 %d 字", len([]rune(comments))))
	}

	fmt.Printf("  → 用户表自检：通过 %d 项，失败 %d 项\n", r.ok, r.fail)
}

func checkOrg(ctx context.Context, db *mongo.Database) {
	r := &report{}
	c := db.Collection("organization")

	cnt, _ := c.CountDocuments(ctx, bson.M{"linke_man_email": bson.M{"$exists": true}})
	r.assert(cnt > 0, fmt.Sprintf("linke_man_email 字段存在（%d 条）", cnt), "目标字段缺失")

	bad, _ := c.CountDocuments(ctx, bson.M{"linkeman_email": bson.M{"$exists": true}})
	r.assert(bad == 0, "不存在拼写错误的 linkeman_email 字段", fmt.Sprintf("%d 条", bad))

	var doc bson.M
	if err := c.FindOne(ctx, bson.M{"uid": "O00001"}).Decode(&doc); err == nil {
		v, ok := doc["linke_man_email"]
		r.assert(ok, fmt.Sprintf("linke_man_email 值示例: %v", v), "字段不存在")
	}

	// sort_no 必须转成字符串（源表 INT）
	intSort, _ := c.CountDocuments(ctx, bson.M{"sort_no": bson.M{"$type": "int"}})
	r.assert(intSort == 0, "不存在数字型 sort_no", fmt.Sprintf("发现 %d 条数字型", intSort))
	strSort, _ := c.CountDocuments(ctx, bson.M{"sort_no": bson.M{"$type": "string"}})
	r.assert(strSort > 0, fmt.Sprintf("sort_no 是字符串类型（%d 条）", strSort), "没有字符串型 sort_no")

	fmt.Printf("  → 组织表自检：通过 %d 项，失败 %d 项\n", r.ok, r.fail)
}
