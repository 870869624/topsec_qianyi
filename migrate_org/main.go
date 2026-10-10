// Command migrate_org 把达梦 DLPDATA.did_system_sync_org 的存量数据迁移到 MongoDB 的 identity.organization。
//
// 这是一个可以独立编译、独立运行的脚本，不依赖 migrate_user：
//
//	go run ./demo/migrate_org -dry-run -limit 200   # 先试跑 200 条，核对字段映射
//	go run ./demo/migrate_org                       # 正式全量迁移（约 10 万条）
//	go run ./demo/migrate_org -clean delete         # 只清空数据、保留目标集合已有索引
//	go run ./demo/migrate_org -count-only           # 只统计源表条数
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"shutong/syncbase"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	defaultSourceTable = "DLPDATA.did_system_sync_org"
	defaultTargetColl  = "organization"
)

// orgColumns 达梦侧列名，顺序必须与 orgRow.ScanTargets() 严格一致。
var orgColumns = []string{
	"base_org_id", "base_org_code", "base_org_name", "short_name", "base_org_type",
	"parent_base_org_id", "sort_no", "base_org_desc", "saphr_org_id", "manager",
	"address_id", "address_code", "primary_business", "org_english_name", "linkeman_email",
	"linkman_fax", "linkman_phone", "func_type", "uid", "creator_id",
	"create_time", "tw_insert_time", "ods_insert_time",
}

var (
	envPath     = flag.String("env", "", "配置文件路径；留空则自动从当前目录逐级向上查找 .env")
	sourceTable = flag.String("dm-table", defaultSourceTable, "达梦源表全限定名（不带引号）")
	identCase   = flag.String("dm-case", "as-is", "达梦标识符大小写折叠：as-is / upper / lower")
	schemaName  = flag.String("dm-schema", "", "达梦模式名；留空取 .env 的 DM_SCHEMA，两者都空则不指定默认模式")
	mongoDB     = flag.String("mongo-db", "", "目标库名；留空取 .env 的 MONGO_DB，默认 identity")
	mongoColl   = flag.String("mongo-collection", defaultTargetColl, "目标集合名")
	batchSize   = flag.Int("batch", 1000, "批量写入条数")
	pageSize    = flag.Int64("page-size", 20000, "达梦分页读取页大小；设为 0 改为单次流式读取")
	workers     = flag.Int("workers", 2, "并发写入协程数")
	limit       = flag.Int64("limit", 0, "仅迁移前 N 条，用于试跑；0 表示全部")
	cleanMode   = flag.String("clean", "drop", "迁移前处理目标集合：drop 删集合 / delete 清数据保留索引 / none 不清")
	dryRun      = flag.Bool("dry-run", false, "只读取并转换，不写入 Mongo")
	noProgress  = flag.Bool("no-progress", false, "关闭实时进度条")
	ensureIndex = flag.Bool("ensure-index", true, "迁移完成后补建常用索引")
	rowPrefetch = flag.Int("row-prefetch", 500, "达梦驱动每批预取行数，影响读取内存占用")
	verbose     = flag.Bool("verbose", false, "在终端打印明细日志")
	logPath     = flag.String("log", "", "日志文件路径；留空则不写文件")
	timeout     = flag.Duration("timeout", 60*time.Minute, "整次迁移超时时间")
	connTimeout = flag.Duration("conn-timeout", 15*time.Second, "数据库连接超时")
	countOnly   = flag.Bool("count-only", false, "只统计源表行数，不做任何迁移")
)

func main() {
	setupUsage()
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ 迁移失败: %v\n", err)
		os.Exit(1)
	}
}

// setupUsage 定制帮助信息。达梦驱动会在 init 阶段往默认 FlagSet 注册 DM_HOME，
// 这里把它过滤掉，避免干扰阅读。
func setupUsage() {
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprint(out, "组织表迁移：达梦 DLPDATA.did_system_sync_org → MongoDB identity.organization\n\n")
		fmt.Fprint(out, "示例:\n")
		fmt.Fprint(out, "  试跑 200 条:  go run ./demo/migrate_org -dry-run -limit 200\n")
		fmt.Fprint(out, "  正式迁移:     go run ./demo/migrate_org\n")
		fmt.Fprint(out, "  保留索引清空: go run ./demo/migrate_org -clean delete\n\n")
		fmt.Fprint(out, "选项:\n")
		flag.VisitAll(func(f *flag.Flag) {
			if f.Name == "DM_HOME" {
				return
			}
			def := ""
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				def = fmt.Sprintf(" (默认 %s)", f.DefValue)
			}
			fmt.Fprintf(out, "  -%s\n        %s%s\n", f.Name, f.Usage, def)
		})
	}
}

func run() error {
	// ---------------- 1. 读取配置 ----------------
	cfg, err := syncbase.LoadConfig(*envPath)
	if err != nil {
		return err
	}
	// 命令行参数的优先级高于配置文件
	if *schemaName != "" {
		cfg.DM.Schema = *schemaName
	}
	if *mongoDB != "" {
		cfg.Mongo.Database = *mongoDB
	}

	idCase, err := syncbase.ParseIdentCase(*identCase)
	if err != nil {
		return err
	}

	log, err := syncbase.NewLogger(*logPath, *verbose)
	if err != nil {
		return err
	}
	defer log.Close()

	fullTable := syncbase.QualifyTable(*sourceTable, idCase)

	fmt.Printf("  组织表迁移  %s → %s.%s\n", fullTable, cfg.Mongo.Database, *mongoColl)
	fmt.Printf("  配置文件    %s\n", cfg.File)
	fmt.Printf("  达梦        %s\n", cfg.DM.SafeDSN())
	fmt.Printf("  MongoDB     %s (db=%s)\n", cfg.Mongo.SafeURI(), cfg.Mongo.Database)
	if *dryRun {
		fmt.Printf("  模式        试运行 dry-run，不会写入任何数据\n")
	}
	fmt.Println()

	log.Infof("开始迁移组织表: source=%s target=%s.%s dryRun=%v batch=%d pageSize=%d workers=%d",
		fullTable, cfg.Mongo.Database, *mongoColl, *dryRun, *batchSize, *pageSize, *workers)

	// ---------------- 2. 建立连接 ----------------
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	dm, err := syncbase.OpenDM(ctx, cfg.DM, *rowPrefetch, *connTimeout)
	if err != nil {
		return err
	}
	defer dm.Close()
	log.Infof("达梦连接成功")

	if *countOnly {
		n, err := syncbase.CountSource(ctx, dm, fullTable)
		if err != nil {
			return fmt.Errorf("统计源表行数失败（可尝试 -dm-case upper）: %w", err)
		}
		fmt.Printf("  源表 %s 共 %d 条记录\n\n", fullTable, n)
		return nil
	}

	client, err := syncbase.OpenMongo(ctx, cfg.Mongo, *connTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	log.Infof("MongoDB 连接成功")

	// ---------------- 3. 执行迁移 ----------------
	table := &syncbase.Table{
		Name:        "组织表",
		SourceTable: fullTable,
		Columns:     syncbase.QuoteIdents(orgColumns, idCase),
		TargetDB:    cfg.Mongo.Database,
		TargetColl:  *mongoColl,
		NewRow:      func() syncbase.RowScanner { return new(orgRow) },
		Indexes: []syncbase.IndexSpec{
			{Name: "uid_1", Keys: bson.D{{Key: "uid", Value: 1}}},
			{Name: "base_org_id_1", Keys: bson.D{{Key: "base_org_id", Value: 1}}},
			{Name: "parent_base_org_id_1", Keys: bson.D{{Key: "parent_base_org_id", Value: 1}}},
			{Name: "base_org_code_1", Keys: bson.D{{Key: "base_org_code", Value: 1}}},
		},
	}

	opts := syncbase.Options{
		BatchSize:   *batchSize,
		PageSize:    *pageSize,
		Limit:       *limit,
		Workers:     *workers,
		DryRun:      *dryRun,
		CleanMode:   *cleanMode,
		EnsureIndex: *ensureIndex,
		RowPrefetch: *rowPrefetch,
		NoProgress:  *noProgress,
	}

	stats, runErr := syncbase.Run(ctx, dm, client, table, opts, log)
	syncbase.PrintStats(stats, *dryRun)

	if runErr != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("迁移被中断或超时: %w", ctx.Err())
		}
		return runErr
	}

	// ---------------- 4. 落地校验 ----------------
	if !*dryRun {
		if err := verify(orgColumns, stats); err != nil {
			fmt.Fprintf(os.Stderr, "  ! 字段映射自检未通过: %v\n", err)
		}
	}
	return nil
}

// ------------------------------------------------------------------ 组织行

// orgRow 对应达梦 did_system_sync_org 表的一行，字段顺序与 orgColumns 一一对应。
type orgRow struct {
	BaseOrgID       sql.NullString
	BaseOrgCode     sql.NullString
	BaseOrgName     sql.NullString
	ShortName       sql.NullString
	BaseOrgType     sql.NullString
	ParentBaseOrgID sql.NullString
	SortNo          sql.NullInt64 // 达梦中是 INT，目标集合里要转成字符串
	BaseOrgDesc     sql.NullString
	SaphrOrgID      sql.NullString
	Manager         sql.NullString
	AddressID       sql.NullString
	AddressCode     sql.NullString
	PrimaryBusiness sql.NullString
	OrgEnglishName  sql.NullString
	LinkemanEmail   sql.NullString
	LinkmanFax      sql.NullString
	LinkmanPhone    sql.NullString
	FuncType        sql.NullString
	UID             sql.NullString
	CreatorID       sql.NullString
	CreateTime      syncbase.NullTime
	TwInsertTime    syncbase.NullTime
	OdsInsertTime   syncbase.NullTime
}

// ScanTargets 顺序必须与 orgColumns 完全一致。
func (r *orgRow) ScanTargets() []any {
	return []any{
		&r.BaseOrgID, &r.BaseOrgCode, &r.BaseOrgName, &r.ShortName, &r.BaseOrgType,
		&r.ParentBaseOrgID, &r.SortNo, &r.BaseOrgDesc, &r.SaphrOrgID, &r.Manager,
		&r.AddressID, &r.AddressCode, &r.PrimaryBusiness, &r.OrgEnglishName, &r.LinkemanEmail,
		&r.LinkmanFax, &r.LinkmanPhone, &r.FuncType, &r.UID, &r.CreatorID,
		&r.CreateTime, &r.TwInsertTime, &r.OdsInsertTime,
	}
}

func (r *orgRow) Document() any {
	now := time.Now().Unix()
	return &orgDoc{
		ID:              primitive.NewObjectID(),
		UID:             syncbase.Str(r.UID),
		BaseOrgID:       syncbase.Str(r.BaseOrgID),
		BaseOrgCode:     syncbase.Str(r.BaseOrgCode),
		BaseOrgName:     syncbase.Str(r.BaseOrgName),
		ShortName:       syncbase.Str(r.ShortName),
		BaseOrgType:     syncbase.Str(r.BaseOrgType),
		ParentBaseOrgID: syncbase.Str(r.ParentBaseOrgID),
		SortNo:          syncbase.NumStr(r.SortNo),
		BaseOrgDesc:     syncbase.Str(r.BaseOrgDesc),
		SaphrOrgID:      syncbase.Str(r.SaphrOrgID),
		Manager:         syncbase.Str(r.Manager),
		AddressID:       syncbase.Str(r.AddressID),
		AddressCode:     syncbase.Str(r.AddressCode),
		PrimaryBusiness: syncbase.Str(r.PrimaryBusiness),
		OrgEnglishName:  syncbase.Str(r.OrgEnglishName),
		// 注意：达梦列名是 linkeman_email（少一个 n），目标集合里叫 linke_man_email
		LinkeManEmail: syncbase.Str(r.LinkemanEmail),
		LinkmanFax:    syncbase.Str(r.LinkmanFax),
		LinkmanPhone:  syncbase.Str(r.LinkmanPhone),
		FuncType:      syncbase.Str(r.FuncType),
		CreatorID:     syncbase.Str(r.CreatorID),
		CreateTime:    r.CreateTime.Ptr(),
		TwInsertTime:  r.TwInsertTime.Ptr(),
		OdsInsertTime: r.OdsInsertTime.Ptr(),
		SerialNum:     "",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// orgDoc 与 pkg/model/identity.Organization 的 bson tag 保持一一对应。
type orgDoc struct {
	ID              primitive.ObjectID `bson:"_id,omitempty"`
	UID             string             `bson:"uid"`
	BaseOrgID       string             `bson:"base_org_id"`
	BaseOrgCode     string             `bson:"base_org_code"`
	BaseOrgName     string             `bson:"base_org_name"`
	ShortName       string             `bson:"short_name"`
	BaseOrgType     string             `bson:"base_org_type"`
	ParentBaseOrgID string             `bson:"parent_base_org_id"`
	SortNo          string             `bson:"sort_no"`
	BaseOrgDesc     string             `bson:"base_org_desc"`
	SaphrOrgID      string             `bson:"saphr_org_id"`
	Manager         string             `bson:"manager"`
	AddressID       string             `bson:"address_id"`
	AddressCode     string             `bson:"address_code"`
	PrimaryBusiness string             `bson:"primary_business"`
	OrgEnglishName  string             `bson:"org_english_name"`
	LinkeManEmail   string             `bson:"linke_man_email"`
	LinkmanFax      string             `bson:"linkman_fax"`
	LinkmanPhone    string             `bson:"linkman_phone"`
	FuncType        string             `bson:"func_type"`
	CreatorID       string             `bson:"creator_id"`
	CreateTime      *time.Time         `bson:"create_time"`
	TwInsertTime    *time.Time         `bson:"tw_insert_time"`
	OdsInsertTime   *time.Time         `bson:"ods_insert_time"`
	SerialNum       string             `bson:"serial_num"`
	CreatedAt       int64              `bson:"created_at"`
	UpdatedAt       int64              `bson:"updated_at"`
}

func verify(cols []string, stats *syncbase.Stats) error {
	n := len((&orgRow{}).ScanTargets())
	if n != len(cols) {
		return fmt.Errorf("扫描目标 %d 个与源表列 %d 个不匹配", n, len(cols))
	}
	if stats.Failed > 0 {
		return fmt.Errorf("有 %d 条数据扫描/写入失败，请查看日志", stats.Failed)
	}
	return nil
}
