package syncbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// RowScanner 描述一行源数据：既能作为 sql.Rows.Scan 的扫描目标，也能转换成 Mongo 文档。
type RowScanner interface {
	// ScanTargets 返回传给 sql.Rows.Scan 的字段指针，顺序必须与 Table.Columns 一致。
	ScanTargets() []any
	// Document 把当前行转换成待写入 Mongo 的文档（带 bson tag 的结构体指针）。
	// 返回 nil 表示跳过该行。
	Document() any
}

// IndexSpec 描述一个需要在目标集合上创建的索引。
type IndexSpec struct {
	Name   string
	Keys   bson.D
	Unique bool
}

// Table 描述一张待迁移的表及其字段映射。
type Table struct {
	// Name 逻辑表名，仅用于展示，如 "用户表"。
	Name string
	// SourceTable 达梦侧全限定表名，需与库中实际标识符大小写一致，如 `"DLPDATA"."did_system_sync_user"`。
	SourceTable string
	// Columns 达梦侧列名（已按需加双引号），顺序必须与 RowScanner.ScanTargets 严格一致。
	Columns []string
	// TargetDB / TargetColl 目标库与集合。
	TargetDB   string
	TargetColl string
	// NewRow 每读取一行调用一次，返回新的扫描载体。
	NewRow func() RowScanner
	// Indexes 迁移完成后在目标集合上补建的索引。
	Indexes []IndexSpec
}

func (t *Table) selectAllSQL() string {
	return "SELECT " + strings.Join(t.Columns, ", ") + " FROM " + t.SourceTable
}

// selectPageSQL 用 ROWNUM 做分页，保证每次只把一页数据拉进内存。
// 占位符顺序为：结束行号(end)、起始行号(start)。
func (t *Table) selectPageSQL() string {
	cols := strings.Join(t.Columns, ", ")
	return fmt.Sprintf(
		`SELECT %s FROM (SELECT %s, ROWNUM AS "__dm_rn" FROM %s WHERE ROWNUM <= ?) WHERE "__dm_rn" > ?`,
		cols, cols, t.SourceTable,
	)
}

// Options 迁移选项。
type Options struct {
	BatchSize   int
	PageSize    int64
	Limit       int64
	Workers     int
	DryRun      bool
	CleanMode   string // drop | delete | none
	EnsureIndex bool
	RowPrefetch int
	NoProgress  bool
}

func (o *Options) normalize() {
	if o.BatchSize <= 0 {
		o.BatchSize = 1000
	}
	if o.Workers <= 0 {
		o.Workers = 2
	}
	if o.RowPrefetch <= 0 {
		o.RowPrefetch = 500
	}
	if o.CleanMode == "" {
		o.CleanMode = "drop"
	}
}

// Stats 记录一次迁移的结果。
type Stats struct {
	Table       string
	SourceTable string
	TargetNS    string
	SourceCount int64
	TargetCount int64
	Read        int64
	Written     int64
	Failed      int64
	Cleaned     int64
	Elapsed     time.Duration
	Rate        float64
}

// CountSource 统计达梦源表总行数，用于进度条分母。
func CountSource(ctx context.Context, db *sql.DB, sourceTable string) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+sourceTable).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CountTarget 统计目标集合当前文档数。
func CountTarget(ctx context.Context, client *mongo.Client, db, coll string) (int64, error) {
	return client.Database(db).Collection(coll).CountDocuments(ctx, bson.D{})
}

// PrepareTarget 在迁移前处理目标集合。
// drop：直接删除集合（最快，但会一并删除已有索引）；delete：保留集合结构与索引，仅清空数据。
func PrepareTarget(ctx context.Context, client *mongo.Client, db, coll, mode string, log *Logger) (int64, error) {
	c := client.Database(db).Collection(coll)
	switch strings.ToLower(mode) {
	case "drop":
		before, err := c.CountDocuments(ctx, bson.D{})
		if err == nil && before > 0 {
			log.Infof("目标集合 %s.%s 原有 %d 条数据，准备删除", db, coll, before)
		}
		if err := c.Drop(ctx); err != nil && !isNamespaceNotFound(err) {
			return 0, fmt.Errorf("删除目标集合 %s.%s 失败: %w", db, coll, err)
		}
		return before, nil
	case "delete":
		res, err := c.DeleteMany(ctx, bson.D{})
		if err != nil {
			return 0, fmt.Errorf("清空目标集合 %s.%s 失败: %w", db, coll, err)
		}
		log.Infof("已清空目标集合 %s.%s，删除 %d 条旧数据（索引保留）", db, coll, res.DeletedCount)
		return res.DeletedCount, nil
	case "none":
		log.Infof("已指定不清空目标集合，将直接追加写入")
		return 0, nil
	default:
		return 0, fmt.Errorf("不支持的清空方式 %q，可选 drop / delete / none", mode)
	}
}

func isNamespaceNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "ns not found") ||
		strings.Contains(msg, "NamespaceNotFound") ||
		strings.Contains(msg, "ns does not exist")
}

// Run 执行一次完整迁移：统计 → 清空目标 → 读取写入 → 校验 → 建索引。
func Run(ctx context.Context, db *sql.DB, client *mongo.Client, tbl *Table, o Options, log *Logger) (*Stats, error) {
	o.normalize()
	start := time.Now()
	st := &Stats{
		Table:       tbl.Name,
		SourceTable: tbl.SourceTable,
		TargetNS:    tbl.TargetDB + "." + tbl.TargetColl,
	}

	// ---------- 1. 统计源表行数 ----------
	total, err := CountSource(ctx, db, tbl.SourceTable)
	if err != nil {
		return st, fmt.Errorf("统计源表 %s 行数失败（请确认表名大小写与 -dm-table 参数）: %w", tbl.SourceTable, err)
	}
	st.SourceCount = total
	expected := total
	if o.Limit > 0 && o.Limit < expected {
		expected = o.Limit
	}
	log.Infof("源表 %s 共 %d 条记录，本次计划迁移 %d 条", tbl.SourceTable, total, expected)

	// ---------- 2. 处理目标集合 ----------
	// drop 模式会连索引一起删掉，所以先快照，等数据写完再原样重建，避免线上索引静默丢失。
	var captured []CapturedIndex
	if o.DryRun {
		log.Infof("dry-run 模式：跳过目标集合清理，只读取不写入")
	} else {
		if strings.EqualFold(o.CleanMode, "drop") {
			if idxs, idxErr := CaptureIndexes(ctx, client, tbl.TargetDB, tbl.TargetColl); idxErr != nil {
				log.Warnf("%v", idxErr)
			} else if len(idxs) > 0 {
				captured = idxs
				log.Infof("已记录目标集合原有索引 %d 个，迁移完成后将重建", len(idxs))
			}
		}
		cleaned, cleanErr := PrepareTarget(ctx, client, tbl.TargetDB, tbl.TargetColl, o.CleanMode, log)
		if cleanErr != nil {
			return st, cleanErr
		}
		st.Cleaned = cleaned
	}
	coll := client.Database(tbl.TargetDB).Collection(tbl.TargetColl)

	// ---------- 3. 启动进度显示 ----------
	prog := NewProgress(tbl.SourceTable, st.TargetNS, expected, !o.NoProgress)
	log.SetSink(prog)
	prog.Start()

	// ---------- 4. 读取 + 批量写入流水线 ----------
	readErr := pipeline(ctx, db, coll, tbl, &o, prog, log, expected)

	prog.Finish()
	log.SetSink(nil)

	st.Read, st.Written, st.Failed = prog.Counters()
	st.Elapsed = time.Since(start)
	if sec := st.Elapsed.Seconds(); sec > 0 {
		st.Rate = float64(st.Written) / sec
	}

	if readErr != nil {
		return st, readErr
	}

	// ---------- 5. 校验与善后 ----------
	if o.DryRun {
		log.Infof("dry-run 结束，未写入任何数据")
		return st, nil
	}

	if cnt, cntErr := CountTarget(ctx, client, tbl.TargetDB, tbl.TargetColl); cntErr == nil {
		st.TargetCount = cnt
	} else {
		log.Warnf("统计目标集合 %s 文档数失败: %v", st.TargetNS, cntErr)
	}

	if o.EnsureIndex {
		skip := make(map[string]bool, len(captured))
		for _, ix := range captured {
			skip[ix.Name] = true
		}
		RestoreIndexes(ctx, client, tbl.TargetDB, tbl.TargetColl, captured, log)
		EnsureIndexes(ctx, client, tbl, skip, log)
	}
	return st, nil
}

// pipeline 是「单协程顺序读 + 多协程并发批量写」的生产者消费者模型。
// 生产者负责从达梦分页/流式读取并转换文档，消费者负责 InsertMany 落库。
func pipeline(ctx context.Context, db *sql.DB, coll *mongo.Collection, tbl *Table, o *Options, prog *Progress, log *Logger, expected int64) error {
	batchCh := make(chan []any, o.Workers*2)

	var wg sync.WaitGroup
	for i := 0; i < o.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for docs := range batchCh {
				if o.DryRun {
					prog.AddWritten(int64(len(docs)))
					continue
				}
				ok, bad, err := insertBatch(ctx, coll, docs)
				prog.AddWritten(ok)
				prog.AddFailed(bad)
				if err != nil {
					prog.Printf("批量写入失败（%d 条）: %v", len(docs), err)
					log.Errorf("批量写入失败（%d 条）: %v", len(docs), err)
				} else if bad > 0 {
					log.Warnf("批量写入部分失败：成功 %d 条，失败 %d 条", ok, bad)
				}
			}
		}()
	}

	batch := make([]any, 0, o.BatchSize)
	var scanned int64

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		out := batch
		batch = make([]any, 0, o.BatchSize)
		select {
		case batchCh <- out:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// consume 消费一个结果集，把行转换成文档填进 batch。
	consume := func(rows *sql.Rows) error {
		defer rows.Close()
		for rows.Next() {
			if o.Limit > 0 && scanned >= o.Limit {
				return nil
			}
			scanned++
			prog.AddRead(1)

			row := tbl.NewRow()
			if err := rows.Scan(row.ScanTargets()...); err != nil {
				prog.AddFailed(1)
				log.Errorf("第 %d 行扫描失败: %v", scanned, err)
				continue
			}
			doc := row.Document()
			if doc == nil {
				continue
			}
			batch = append(batch, doc)
			if len(batch) >= o.BatchSize {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("遍历结果集出错（已读取 %d 行）: %w", scanned, err)
		}
		return nil
	}

	var readErr error
	if o.PageSize > 0 {
		limit := expected
		if limit <= 0 {
			limit = o.PageSize
		}
		sqlText := tbl.selectPageSQL()
		log.Infof("分页读取模式：每页 %d 条，SQL = %s", o.PageSize, sqlText)
		for start := int64(0); start < limit; start += o.PageSize {
			if err := ctx.Err(); err != nil {
				readErr = err
				break
			}
			end := start + o.PageSize
			if end > limit {
				end = limit
			}
			rows, err := db.QueryContext(ctx, sqlText, end, start)
			if err != nil {
				readErr = fmt.Errorf("分页查询失败（第 %d~%d 行）: %w", start+1, end, err)
				break
			}
			if err := consume(rows); err != nil {
				readErr = err
				break
			}
			if o.Limit > 0 && scanned >= o.Limit {
				break
			}
		}
	} else {
		sqlText := tbl.selectAllSQL()
		log.Infof("流式读取模式：SQL = %s", sqlText)
		rows, err := db.QueryContext(ctx, sqlText)
		if err != nil {
			readErr = fmt.Errorf("查询源表失败: %w", err)
		} else if err := consume(rows); err != nil {
			readErr = err
		}
	}

	if err := flush(); err != nil && readErr == nil {
		readErr = err
	}
	close(batchCh)
	wg.Wait()

	if readErr == nil && o.Limit == 0 && expected > 0 && scanned != expected {
		log.Warnf("实际读取行数 %d 与源表统计值 %d 不一致，请留意源表是否在迁移过程中发生变化", scanned, expected)
	}
	return readErr
}

// insertBatch 批量写入。ordered=false 时部分失败不影响其余文档写入。
// 返回成功条数、失败条数与致命错误。
func insertBatch(ctx context.Context, coll *mongo.Collection, docs []any) (ok, bad int64, err error) {
	if len(docs) == 0 {
		return 0, 0, nil
	}
	_, err = coll.InsertMany(ctx, docs, options.InsertMany().SetOrdered(false))
	if err == nil {
		return int64(len(docs)), 0, nil
	}

	// ordered=false 时，部分文档失败不会回滚其余文档，用 BulkWriteException 精确统计失败条数。
	var bwe mongo.BulkWriteException
	if errors.As(err, &bwe) {
		bad = int64(len(bwe.WriteErrors))
		if bad > int64(len(docs)) {
			bad = int64(len(docs))
		}
		return int64(len(docs)) - bad, bad, nil
	}
	return 0, int64(len(docs)), err
}

// EnsureIndexes / RestoreIndexes / CaptureIndexes 见 indexes.go

// PrintStats 输出迁移结果汇总。
func PrintStats(st *Stats, dryRun bool) {
	title := st.Table
	if title == "" {
		title = st.TargetNS
	}
	fmt.Println()
	fmt.Println(strings.Repeat("─", 66))
	if dryRun {
		fmt.Printf("  试运行结束（未写入数据）· %s\n", title)
	} else {
		fmt.Printf("  迁移完成 · %s\n", title)
	}
	fmt.Println(strings.Repeat("─", 66))
	fmt.Printf("  源表        %-34s %s 条\n", st.SourceTable, comma(st.SourceCount))
	if !dryRun {
		fmt.Printf("  目标集合    %-34s %s 条\n", st.TargetNS, comma(st.TargetCount))
		fmt.Printf("  清空旧数据  %s 条\n", comma(st.Cleaned))
	}
	fmt.Printf("  读取        %s 条\n", comma(st.Read))
	fmt.Printf("  写入        %s 条\n", comma(st.Written))
	if st.Failed > 0 {
		fmt.Printf("  失败        %s 条\n", comma(st.Failed))
	}
	fmt.Printf("  耗时        %s   平均 %s 条/秒\n", fmtDuration(st.Elapsed), comma(int64(st.Rate)))
	if !dryRun && st.TargetCount > 0 && st.Written != st.TargetCount {
		fmt.Printf("  提示        写入 %s 条与目标集合实际 %s 条不一致（可能是重复执行未清空所致）\n",
			comma(st.Written), comma(st.TargetCount))
	}
	fmt.Println(strings.Repeat("─", 66))
}
