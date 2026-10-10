// Command _seed 在达梦里造出迁移工具所需的源表与测试数据。
//
// 仅用于联调测试，**不属于交付物**。
// 使用前请确认：目标达梦实例是你自己的测试实例，且模式名（默认 DLPDATA）不是在生产库上。
//
//	go run ./demo/_seed                      # 默认：用户表 500 条 + 组织表 300 条
//	go run ./demo/_seed -rows 2000 -org-rows 800
//	go run ./demo/_seed -clean=false         # 追加数据而不清空
//	go run ./demo/_seed -drop                # 只删除测试模式下的两张表后退出
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"git.cloud.top/ngdlp/server/demo/syncbase"
)

var (
	schemaName  = flag.String("schema", "DLPDATA", "要创建/写入的模式名")
	userRows    = flag.Int("rows", 500, "用户表写入条数")
	orgRows     = flag.Int("org-rows", 300, "组织表写入条数")
	cleanFirst  = flag.Bool("clean", true, "写入前清空两张表的已有数据")
	dropTables  = flag.Bool("drop", false, "只删除两张表后退出，不写数据")
	connTimeout = flag.Duration("conn-timeout", 20*time.Second, "数据库连接超时")
)

// ============================================================== 建表 DDL
// 与 达梦表结构/*.txt 里的原始 DDL 保持一致：列名大小写、列顺序、类型全部照搬。
// 唯一改动是去掉 STORAGE(ON "DLP_DATA") —— 那是原库的表空间，目标实例上不一定存在。

func userDDL(schema string) string {
	return fmt.Sprintf(`CREATE TABLE "%s"."did_system_sync_user" (`, schema) + `
"uid" VARCHAR(255),
"user_id" VARCHAR(255),
"login_name" VARCHAR(255),
"real_name" VARCHAR(255),
"user_status" INT,
"gender" VARCHAR(50),
"identity_no" VARCHAR(255),
"birth_day" VARCHAR(50),
"mobile" VARCHAR(100),
"office_phone" VARCHAR(100),
"email" VARCHAR(500),
"position_code" VARCHAR(255),
"position_name" VARCHAR(500),
"JOB" VARCHAR(500),
"duty" VARCHAR(500),
"degree_code" VARCHAR(255),
"comments" VARCHAR(2000),
"base_org_id" VARCHAR(255),
"base_org_name" VARCHAR(500),
"saphr_user_id" VARCHAR(255),
"PASSWORD" VARCHAR(1000),
"address" VARCHAR(2000),
"user_type" VARCHAR(255),
"start_date" VARCHAR(100),
"end_date" VARCHAR(100),
"up_id" VARCHAR(255),
"creator_id" VARCHAR(255),
"create_time" TIMESTAMP(6),
"ods_insert_time" TIMESTAMP(6),
"tw_insert_time" TIMESTAMP(6))`
}

func orgDDL(schema string) string {
	return fmt.Sprintf(`CREATE TABLE "%s"."did_system_sync_org" (`, schema) + `
"base_org_id" VARCHAR(255),
"base_org_code" VARCHAR(255),
"base_org_name" VARCHAR(500),
"short_name" VARCHAR(500),
"base_org_type" VARCHAR(255),
"parent_base_org_id" VARCHAR(255),
"sort_no" INT,
"base_org_desc" VARCHAR(2000),
"saphr_org_id" VARCHAR(255),
"manager" VARCHAR(255),
"address_id" VARCHAR(1000),
"address_code" VARCHAR(255),
"primary_business" VARCHAR(2000),
"org_english_name" VARCHAR(500),
"linkeman_email" VARCHAR(500),
"linkman_fax" VARCHAR(255),
"linkman_phone" VARCHAR(255),
"func_type" VARCHAR(255),
"uid" VARCHAR(255),
"creator_id" VARCHAR(255),
"create_time" TIMESTAMP(6),
"tw_insert_time" TIMESTAMP(6),
"ods_insert_time" TIMESTAMP(6))`
}

// ============================================================== 插入语句

func userInsert(schema string) string {
	return fmt.Sprintf(`INSERT INTO "%s"."did_system_sync_user" (
"uid","user_id","login_name","real_name","user_status","gender","identity_no","birth_day","mobile","office_phone",
"email","position_code","position_name","JOB","duty","degree_code","comments","base_org_id","base_org_name","saphr_user_id",
"PASSWORD","address","user_type","start_date","end_date","up_id","creator_id","create_time","ods_insert_time","tw_insert_time"
) VALUES (%s)`, schema, placeholders(30))
}

func orgInsert(schema string) string {
	return fmt.Sprintf(`INSERT INTO "%s"."did_system_sync_org" (
"base_org_id","base_org_code","base_org_name","short_name","base_org_type",
"parent_base_org_id","sort_no","base_org_desc","saphr_org_id","manager",
"address_id","address_code","primary_business","org_english_name","linkeman_email",
"linkman_fax","linkman_phone","func_type","uid","creator_id",
"create_time","tw_insert_time","ods_insert_time"
) VALUES (%s)`, schema, placeholders(23))
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ============================================================== 测试数据池

var (
	surnames  = []string{"张", "王", "李", "赵", "陈", "刘", "杨", "黄", "周", "吴", "徐", "孙", "马", "朱", "胡"}
	givenName = []string{"伟", "芳", "娜", "敏", "静", "磊", "强", "军", "洋", "勇", "艳", "杰", "涛", "明", "超"}
	positions = []string{"高级工程师", "部门经理", "技术总监", "产品经理", "测试工程师", "数据分析师", "运维工程师", "前端开发", "后端开发", "架构师"}
	jobs      = []string{"研发", "测试", "运维", "产品", "数据", "安全", "售前", "售后"}
	duties    = []string{"负责核心系统研发与维护", "负责产品质量保障", "负责线上环境稳定性", "负责需求分析与设计", "负责数据平台建设", "负责安全合规审计"}
	orgWords  = []string{"研发中心", "产品部", "测试中心", "运维部", "数据中心", "安全部", "综合管理部", "市场部"}
	cities    = []string{"北京市海淀区中关村南大街", "上海市浦东新区世纪大道", "广州市天河区体育西路", "深圳市南山区科技园", "成都市高新区天府大道", "杭州市西湖区文三路"}
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ 注入失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := syncbase.LoadConfig("")
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	fmt.Printf("  目标达梦    %s\n", cfg.DM.SafeDSN())
	fmt.Printf("  测试模式    %s\n\n", *schemaName)

	db, err := syncbase.OpenDM(ctx, cfg.DM, 500, *connTimeout)
	if err != nil {
		return err
	}
	defer db.Close()

	if *dropTables {
		return dropAll(ctx, db)
	}

	if err := ensureSchema(ctx, db); err != nil {
		return err
	}
	if err := ensureTables(ctx, db); err != nil {
		return err
	}
	if *cleanFirst {
		if err := cleanTables(ctx, db); err != nil {
			return err
		}
	}

	if *userRows > 0 {
		if err := insertUsers(ctx, db, *userRows); err != nil {
			return fmt.Errorf("写入用户表失败: %w", err)
		}
	}
	if *orgRows > 0 {
		if err := insertOrgs(ctx, db, *orgRows); err != nil {
			return fmt.Errorf("写入组织表失败: %w", err)
		}
	}

	fmt.Println()
	return report(ctx, db)
}

// ------------------------------------------------------------------ 结构

func ensureSchema(ctx context.Context, db *sql.DB) error {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCH' AND NAME = ?`, *schemaName).Scan(&n)
	if err != nil {
		return fmt.Errorf("检查模式是否存在失败: %w", err)
	}
	if n > 0 {
		fmt.Printf("  模式 %s 已存在，跳过创建\n", *schemaName)
		return nil
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA "%s"`, *schemaName)); err != nil {
		return fmt.Errorf("创建模式 %s 失败: %w", *schemaName, err)
	}
	fmt.Printf("  已创建模式 %s\n", *schemaName)
	return nil
}

func ensureTables(ctx context.Context, db *sql.DB) error {
	for _, t := range []struct {
		name string
		ddl  func(string) string
	}{
		{"did_system_sync_user", userDDL},
		{"did_system_sync_org", orgDDL},
	} {
		var n int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER = ? AND TABLE_NAME = ?`,
			*schemaName, t.name).Scan(&n)
		if err != nil {
			return fmt.Errorf("检查表 %s 是否存在失败: %w", t.name, err)
		}
		if n > 0 {
			fmt.Printf("  表 %s.%s 已存在，跳过创建\n", *schemaName, t.name)
			continue
		}
		if _, err := db.ExecContext(ctx, t.ddl(*schemaName)); err != nil {
			return fmt.Errorf("创建表 %s.%s 失败: %w", *schemaName, t.name, err)
		}
		fmt.Printf("  已创建表 %s.%s\n", *schemaName, t.name)
	}
	return nil
}

// cleanTables 清空两张表。删除前先把原有条数打出来，避免误删数据时毫无察觉。
func cleanTables(ctx context.Context, db *sql.DB) error {
	for _, t := range []string{"did_system_sync_user", "did_system_sync_org"} {
		var n int64
		q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s"."%s"`, *schemaName, t)
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return fmt.Errorf("统计 %s 失败: %w", t, err)
		}
		if n == 0 {
			continue
		}
		fmt.Printf("  ! 表 %s.%s 原有 %d 条数据，先清空（-clean=false 可保留）\n", *schemaName, t, n)
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM "%s"."%s"`, *schemaName, t)); err != nil {
			return fmt.Errorf("清空 %s 失败: %w", t, err)
		}
	}
	return nil
}

func dropAll(ctx context.Context, db *sql.DB) error {
	for _, t := range []string{"did_system_sync_user", "did_system_sync_org"} {
		if _, err := db.ExecContext(ctx,
			fmt.Sprintf(`DROP TABLE "%s"."%s"`, *schemaName, t)); err != nil {
			fmt.Printf("  删除表 %s.%s 失败（可能本就不存在）: %v\n", *schemaName, t, err)
			continue
		}
		fmt.Printf("  已删除表 %s.%s\n", *schemaName, t)
	}
	return nil
}

// ------------------------------------------------------------------ 写数据

func insertUsers(ctx context.Context, db *sql.DB, n int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, userInsert(*schemaName))
	if err != nil {
		return err
	}
	defer stmt.Close()

	rng := rand.New(rand.NewSource(20260929))
	base := time.Date(2024, 3, 15, 9, 30, 0, 0, time.Local)

	for i := 1; i <= n; i++ {
		if _, err := stmt.ExecContext(ctx, userRowArgs(i, rng, base)...); err != nil {
			return fmt.Errorf("第 %d 条: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("  已写入用户表 %d 条\n", n)
	return nil
}

// userRowArgs 生成一行用户数据。刻意覆盖各种边界，用来验证迁移的健壮性：
// NULL 值、中文、中英文引号、反斜杠、含 # 的密码、超长文本、大写列 "JOB"/"PASSWORD"。
func userRowArgs(i int, rng *rand.Rand, base time.Time) []any {
	uid := fmt.Sprintf("U%06d", i)
	userID := fmt.Sprintf("EMP%06d", i)
	loginName := fmt.Sprintf("user%06d", i)
	realName := surnames[rng.Intn(len(surnames))] + givenName[rng.Intn(len(givenName))]

	// user_status 是 INT；每 13 条留一条 NULL，验证「INT → 字符串」的转换
	var userStatus any = i % 3
	if i%13 == 0 {
		userStatus = nil
	}

	gender := fmt.Sprintf("%d", 1+i%2)

	identityNo := fmt.Sprintf("11010119900101%04d", i%10000)
	if i%9 == 0 {
		identityNo = fmt.Sprintf("11010119900101%03dX", i%1000)
	}

	birthDay := fmt.Sprintf("19%02d-%02d-%02d", 70+i%30, 1+i%12, 1+i%28)
	mobile := fmt.Sprintf("138%08d", i)
	officePhone := fmt.Sprintf("010-8888%04d", i%10000)

	var email any = fmt.Sprintf("user%06d@example.com", i)
	if i%5 == 0 {
		email = nil
	}

	positionCode := fmt.Sprintf("POS%04d", i%200)
	positionName := positions[i%len(positions)]
	job := jobs[i%len(jobs)]
	duty := duties[i%len(duties)]
	degreeCode := fmt.Sprintf("%02d", 1+i%6)

	var comments any = "常规测试数据"
	switch {
	case i%29 == 0:
		comments = nil
	case i%23 == 0:
		comments = strings.Repeat("超长文本", 120) // 360 字
	case i%17 == 0:
		comments = "含中文双引号\"和'单引号'以及反斜杠\\和制表符\t的注释"
	case i%7 == 0:
		comments = `他说："这个字段要小心"；她答：'我知道'`
	}

	baseOrgID := fmt.Sprintf("ORG%05d", 1+i%120)
	baseOrgName := orgWords[i%len(orgWords)] + fmt.Sprintf("第%d组", 1+i%20)
	saphrUserID := fmt.Sprintf("SAP%08d", 10000000+i)

	var password any = fmt.Sprintf("$2a$10$abcdefghijklmnopqrstuv%04d", i%10000)
	switch {
	case i%31 == 0:
		password = nil
	case i%19 == 0:
		password = `P@ss#2024&word'quote\backslash`
	case i%11 == 0:
		password = "Pass#2024"
	}

	address := fmt.Sprintf("%s%d号%d层", cities[i%len(cities)], 1+i%500, 1+i%30)
	userType := fmt.Sprintf("%d", 1+i%4)
	startDate := fmt.Sprintf("20%02d-0%d-01", 10+i%15, 1+i%9)

	var endDate any
	if i%6 == 0 {
		endDate = "2025-12-31"
	}

	upID := fmt.Sprintf("U%06d", 1+i%50)
	creatorID := "U000001"

	// 三个时间字段各自独立的 NULL 分布，验证 NullTime 是否写 null 而不是零值时间
	var createTime any = base.Add(time.Duration(i) * time.Minute)
	if i%7 == 0 {
		createTime = nil
	}
	var odsInsertTime any = base.Add(time.Duration(i) * 2 * time.Minute)
	if i%11 == 0 {
		odsInsertTime = nil
	}
	var twInsertTime any = base.Add(time.Duration(i) * 3 * time.Minute)
	if i%19 == 0 {
		twInsertTime = nil
	}

	return []any{
		uid, userID, loginName, realName, userStatus,
		gender, identityNo, birthDay, mobile, officePhone,
		email, positionCode, positionName, job, duty,
		degreeCode, comments, baseOrgID, baseOrgName, saphrUserID,
		password, address, userType, startDate, endDate,
		upID, creatorID, createTime, odsInsertTime, twInsertTime,
	}
}

func insertOrgs(ctx context.Context, db *sql.DB, n int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, orgInsert(*schemaName))
	if err != nil {
		return err
	}
	defer stmt.Close()

	rng := rand.New(rand.NewSource(19880604))
	base := time.Date(2024, 3, 15, 8, 0, 0, 0, time.Local)

	for j := 1; j <= n; j++ {
		if _, err := stmt.ExecContext(ctx, orgRowArgs(j, rng, base)...); err != nil {
			return fmt.Errorf("第 %d 条: %w", j, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("  已写入组织表 %d 条\n", n)
	return nil
}

// orgRowArgs 生成一行组织数据。
// 注意 linkeman_email（源表少一个 e）——迁移后应落在目标字段 linke_man_email，
// 用可辨识的值方便核对映射是否写对。
func orgRowArgs(j int, rng *rand.Rand, base time.Time) []any {
	baseOrgID := fmt.Sprintf("ORG%05d", j)
	baseOrgCode := fmt.Sprintf("C%05d", j)
	baseOrgName := fmt.Sprintf("%s%d部", orgWords[j%len(orgWords)], j)
	shortName := fmt.Sprintf("组织%d", j)
	baseOrgType := fmt.Sprintf("%d", 1+j%3)

	var parentBaseOrgID any // 根节点没有上级
	if j > 1 {
		parentBaseOrgID = fmt.Sprintf("ORG%05d", 1+j/3)
	}

	// sort_no 是 INT，每 13 条一个 NULL
	var sortNo any = j
	if j%13 == 0 {
		sortNo = nil
	}

	var baseOrgDesc any = "组织描述信息"
	switch {
	case j%29 == 0:
		baseOrgDesc = nil
	case j%23 == 0:
		baseOrgDesc = strings.Repeat("组织长描述", 100)
	case j%17 == 0:
		baseOrgDesc = `描述里含"双引号"与'单引号'`
	}

	saphrOrgID := fmt.Sprintf("SORG%08d", 20000000+j)
	manager := surnames[rng.Intn(len(surnames))] + givenName[rng.Intn(len(givenName))]

	addressID := fmt.Sprintf("ADDR%06d", j)
	addressCode := fmt.Sprintf("1101%06d", j)
	primaryBusiness := fmt.Sprintf("主营业务%d", 1+j%30)
	orgEnglishName := fmt.Sprintf("Org %04d Division", j)

	var linkemanEmail any = fmt.Sprintf("linkeman%05d@example.com", j)
	if j%7 == 0 {
		linkemanEmail = nil
	}

	linkmanFax := fmt.Sprintf("010-6666%04d", j%10000)
	linkmanPhone := fmt.Sprintf("139%08d", j)
	funcType := fmt.Sprintf("%d", 1+j%2)
	uid := fmt.Sprintf("O%06d", j)
	creatorID := "U000001"

	var createTime any = base.Add(time.Duration(j) * time.Minute)
	if j%5 == 0 {
		createTime = nil
	}
	var twInsertTime any = base.Add(time.Duration(j) * 2 * time.Minute)
	if j%9 == 0 {
		twInsertTime = nil
	}
	var odsInsertTime any = base.Add(time.Duration(j) * 3 * time.Minute)
	if j%11 == 0 {
		odsInsertTime = nil
	}

	return []any{
		baseOrgID, baseOrgCode, baseOrgName, shortName, baseOrgType,
		parentBaseOrgID, sortNo, baseOrgDesc, saphrOrgID, manager,
		addressID, addressCode, primaryBusiness, orgEnglishName, linkemanEmail,
		linkmanFax, linkmanPhone, funcType, uid, creatorID,
		createTime, twInsertTime, odsInsertTime,
	}
}

// ------------------------------------------------------------------ 汇总

func report(ctx context.Context, db *sql.DB) error {
	fmt.Println(strings.Repeat("─", 66))
	for _, t := range []struct{ name, label string }{
		{"did_system_sync_user", "用户表"},
		{"did_system_sync_org", "组织表"},
	} {
		var n int64
		q := fmt.Sprintf(`SELECT COUNT(*) FROM "%s"."%s"`, *schemaName, t.name)
		if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return fmt.Errorf("统计 %s 失败: %w", t.name, err)
		}
		fmt.Printf("  %s  %-28s %d 条\n", t.label, *schemaName+"."+t.name, n)
	}
	fmt.Println(strings.Repeat("─", 66))
	fmt.Println()
	fmt.Println("下一步：")
	fmt.Println("  go run ./demo/migrate_user -count-only")
	fmt.Println("  go run ./demo/migrate_user -dry-run -limit 20 -verbose")
	fmt.Println()
	return nil
}
