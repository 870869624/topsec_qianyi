// Command migrate_user 把达梦 DLPDATA.did_system_sync_user 的存量数据迁移到 MongoDB 的 identity.user。
//
// 这是一个可以独立编译、独立运行的脚本，不依赖 migrate_org：
//
//	go run ./demo/migrate_user -dry-run -limit 200   # 先试跑 200 条，核对字段映射
//	go run ./demo/migrate_user                       # 正式全量迁移（约 40 万条）
//	go run ./demo/migrate_user -clean delete         # 只清空数据、保留目标集合已有索引
//	go run ./demo/migrate_user -count-only           # 只统计源表条数
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
	defaultSourceTable = "DLPDATA.did_system_sync_user"
	defaultTargetColl  = "user"
)

// userColumns 达梦侧列名，顺序必须与 userRow.ScanTargets() 严格一致。
// 注意 "JOB"、"PASSWORD" 在库中是大写标识符，其余为小写，这是建表 DDL 决定的。
var userColumns = []string{
	"uid", "user_id", "login_name", "real_name", "user_status",
	"gender", "identity_no", "birth_day", "mobile", "office_phone",
	"email", "position_code", "position_name", "JOB", "duty",
	"degree_code", "comments", "base_org_id", "base_org_name", "saphr_user_id",
	"PASSWORD", "address", "user_type", "start_date", "end_date",
	"up_id", "creator_id", "create_time", "ods_insert_time", "tw_insert_time",
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
		fmt.Fprint(out, "用户表迁移：达梦 DLPDATA.did_system_sync_user → MongoDB identity.user\n\n")
		fmt.Fprint(out, "示例:\n")
		fmt.Fprint(out, "  试跑 200 条:  go run ./demo/migrate_user -dry-run -limit 200\n")
		fmt.Fprint(out, "  正式迁移:     go run ./demo/migrate_user\n")
		fmt.Fprint(out, "  保留索引清空: go run ./demo/migrate_user -clean delete\n\n")
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

	fmt.Printf("  用户表迁移  %s → %s.%s\n", fullTable, cfg.Mongo.Database, *mongoColl)
	fmt.Printf("  配置文件    %s\n", cfg.File)
	fmt.Printf("  达梦        %s\n", cfg.DM.SafeDSN())
	fmt.Printf("  MongoDB     %s (db=%s)\n", cfg.Mongo.SafeURI(), cfg.Mongo.Database)
	if *dryRun {
		fmt.Printf("  模式        试运行 dry-run，不会写入任何数据\n")
	}
	fmt.Println()

	log.Infof("开始迁移用户表: source=%s target=%s.%s dryRun=%v batch=%d pageSize=%d workers=%d",
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
		fmt.Printf("  源表 %s 共 %s 条记录\n\n", fullTable, formatComma(n))
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
		Name:        "用户表",
		SourceTable: fullTable,
		Columns:     syncbase.QuoteIdents(userColumns, idCase),
		TargetDB:    cfg.Mongo.Database,
		TargetColl:  *mongoColl,
		NewRow:      func() syncbase.RowScanner { return new(userRow) },
		Indexes: []syncbase.IndexSpec{
			{Name: "uid_1", Keys: bson.D{{Key: "uid", Value: 1}}},
			{Name: "login_name_1", Keys: bson.D{{Key: "login_name", Value: 1}}},
			{Name: "user_id_1", Keys: bson.D{{Key: "user_id", Value: 1}}},
			{Name: "base_org_id_1", Keys: bson.D{{Key: "base_org_id", Value: 1}}},
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
		if err := verify(fullTable, userColumns, stats); err != nil {
			fmt.Fprintf(os.Stderr, "  ! 字段映射自检未通过: %v\n", err)
		}
	}
	return nil
}

// ------------------------------------------------------------------ 用户行

// userRow 对应达梦 did_system_sync_user 表的一行，字段顺序与 userColumns 一一对应。
type userRow struct {
	UID           sql.NullString
	UserID        sql.NullString
	LoginName     sql.NullString
	RealName      sql.NullString
	UserStatus    sql.NullInt64 // 达梦中是 INT，目标集合里要转成字符串
	Gender        sql.NullString
	IdentityNo    sql.NullString
	BirthDay      sql.NullString
	Mobile        sql.NullString
	OfficePhone   sql.NullString
	Email         sql.NullString
	PositionCode  sql.NullString
	PositionName  sql.NullString
	Job           sql.NullString
	Duty          sql.NullString
	DegreeCode    sql.NullString
	Comments      sql.NullString
	BaseOrgID     sql.NullString
	BaseOrgName   sql.NullString
	SaphrUserID   sql.NullString
	Password      sql.NullString
	Address       sql.NullString
	UserType      sql.NullString
	StartDate     sql.NullString
	EndDate       sql.NullString
	UpID          sql.NullString
	CreatorID     sql.NullString
	CreateTime    syncbase.NullTime
	OdsInsertTime syncbase.NullTime
	TwInsertTime  syncbase.NullTime
}

// ScanTargets 顺序必须与 userColumns 完全一致。
func (r *userRow) ScanTargets() []any {
	return []any{
		&r.UID, &r.UserID, &r.LoginName, &r.RealName, &r.UserStatus,
		&r.Gender, &r.IdentityNo, &r.BirthDay, &r.Mobile, &r.OfficePhone,
		&r.Email, &r.PositionCode, &r.PositionName, &r.Job, &r.Duty,
		&r.DegreeCode, &r.Comments, &r.BaseOrgID, &r.BaseOrgName, &r.SaphrUserID,
		&r.Password, &r.Address, &r.UserType, &r.StartDate, &r.EndDate,
		&r.UpID, &r.CreatorID, &r.CreateTime, &r.OdsInsertTime, &r.TwInsertTime,
	}
}

func (r *userRow) Document() any {
	now := time.Now().Unix()
	return &userDoc{
		ID:            primitive.NewObjectID(),
		UID:           syncbase.Str(r.UID),
		UserID:        syncbase.Str(r.UserID),
		LoginName:     syncbase.Str(r.LoginName),
		RealName:      syncbase.Str(r.RealName),
		UserStatus:    syncbase.NumStr(r.UserStatus),
		Gender:        syncbase.Str(r.Gender),
		IdentityNo:    syncbase.Str(r.IdentityNo),
		BirthDay:      syncbase.Str(r.BirthDay),
		Mobile:        syncbase.Str(r.Mobile),
		OfficePhone:   syncbase.Str(r.OfficePhone),
		Email:         syncbase.Str(r.Email),
		PositionCode:  syncbase.Str(r.PositionCode),
		PositionName:  syncbase.Str(r.PositionName),
		Job:           syncbase.Str(r.Job),
		Duty:          syncbase.Str(r.Duty),
		DegreeCode:    syncbase.Str(r.DegreeCode),
		Comments:      syncbase.Str(r.Comments),
		BaseOrgID:     syncbase.Str(r.BaseOrgID),
		BaseOrgName:   syncbase.Str(r.BaseOrgName),
		SaphrUserID:   syncbase.Str(r.SaphrUserID),
		Password:      syncbase.Str(r.Password),
		Address:       syncbase.Str(r.Address),
		UserType:      syncbase.Str(r.UserType),
		StartDate:     syncbase.Str(r.StartDate),
		EndDate:       syncbase.Str(r.EndDate),
		UpID:          syncbase.Str(r.UpID),
		CreatorID:     syncbase.Str(r.CreatorID),
		CreateTime:    r.CreateTime.Ptr(),
		OdsInsertTime: r.OdsInsertTime.Ptr(),
		TwInsertTime:  r.TwInsertTime.Ptr(),
		SerialNum:     "",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// userDoc 与 pkg/model/identity.User 的 bson tag 保持一一对应，
// 这样应用侧用 identity.User 反序列化不会出现字段丢失或类型冲突。
type userDoc struct {
	ID            primitive.ObjectID `bson:"_id,omitempty"`
	UID           string             `bson:"uid"`
	UserID        string             `bson:"user_id"`
	LoginName     string             `bson:"login_name"`
	RealName      string             `bson:"real_name"`
	UserStatus    string             `bson:"user_status"`
	Gender        string             `bson:"gender"`
	IdentityNo    string             `bson:"identity_no"`
	BirthDay      string             `bson:"birth_day"`
	Mobile        string             `bson:"mobile"`
	OfficePhone   string             `bson:"office_phone"`
	Email         string             `bson:"email"`
	PositionCode  string             `bson:"position_code"`
	PositionName  string             `bson:"position_name"`
	Job           string             `bson:"job"`
	Duty          string             `bson:"duty"`
	DegreeCode    string             `bson:"degree_code"`
	Comments      string             `bson:"comments"`
	BaseOrgID     string             `bson:"base_org_id"`
	BaseOrgName   string             `bson:"base_org_name"`
	SaphrUserID   string             `bson:"saphr_user_id"`
	Password      string             `bson:"password"`
	Address       string             `bson:"address"`
	UserType      string             `bson:"user_type"`
	StartDate     string             `bson:"start_date"`
	EndDate       string             `bson:"end_date"`
	UpID          string             `bson:"up_id"`
	CreatorID     string             `bson:"creator_id"`
	CreateTime    *time.Time         `bson:"create_time"`
	OdsInsertTime *time.Time         `bson:"ods_insert_time"`
	TwInsertTime  *time.Time         `bson:"tw_insert_time"`
	SerialNum     string             `bson:"serial_num"`
	CreatedAt     int64              `bson:"created_at"`
	UpdatedAt     int64              `bson:"updated_at"`
}

// verify 做一次廉价的映射自检：源表列数与扫描目标数是否一致。
func verify(fullTable string, cols []string, stats *syncbase.Stats) error {
	n := len((&userRow{}).ScanTargets())
	if n != len(cols) {
		return fmt.Errorf("扫描目标 %d 个与源表列 %d 个不匹配", n, len(cols))
	}
	if stats.Failed > 0 {
		return fmt.Errorf("有 %d 条数据扫描/写入失败，请查看日志", stats.Failed)
	}
	return nil
}

func formatComma(n int64) string {
	s := fmt.Sprintf("%d", n)
	var b []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, c)
	}
	return string(b)
}
