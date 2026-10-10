package syncbase

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// CapturedIndex 保存目标集合原有的索引定义。
//
// 采用 -clean drop 时集合被整体删除，索引会一并消失。若线上集合本来带有唯一索引，
// 直接 drop 会造成静默退化，因此这里先把索引快照下来，写完数据后再原样重建。
type CapturedIndex struct {
	Name                    string
	Keys                    bson.D
	Unique                  bool
	Sparse                  bool
	PartialFilterExpression bson.Raw
	ExpireAfterSeconds      *int32
	Collation               *options.Collation
}

// CaptureIndexes 读取目标集合现有索引（不含内置的 _id_）。
// 集合不存在时返回空列表且不报错。
func CaptureIndexes(ctx context.Context, client *mongo.Client, db, coll string) ([]CapturedIndex, error) {
	cur, err := client.Database(db).Collection(coll).Indexes().List(ctx)
	if err != nil {
		if isNamespaceNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 %s.%s 索引失败: %w", db, coll, err)
	}
	defer cur.Close(ctx)

	var out []CapturedIndex
	for cur.Next(ctx) {
		var doc struct {
			Name                    string             `bson:"name"`
			Key                     bson.D             `bson:"key"`
			Unique                  bool               `bson:"unique"`
			Sparse                  bool               `bson:"sparse"`
			PartialFilterExpression bson.Raw           `bson:"partialFilterExpression"`
			ExpireAfterSeconds      *int32             `bson:"expireAfterSeconds"`
			Collation               *options.Collation `bson:"collation"`
		}
		if err := cur.Decode(&doc); err != nil {
			return out, fmt.Errorf("解析索引定义失败: %w", err)
		}
		if doc.Name == "_id_" || len(doc.Key) == 0 {
			continue
		}
		out = append(out, CapturedIndex{
			Name:                    doc.Name,
			Keys:                    doc.Key,
			Unique:                  doc.Unique,
			Sparse:                  doc.Sparse,
			PartialFilterExpression: doc.PartialFilterExpression,
			ExpireAfterSeconds:      doc.ExpireAfterSeconds,
			Collation:               doc.Collation,
		})
	}
	return out, cur.Err()
}

// RestoreIndexes 重建此前快照下来的索引。
func RestoreIndexes(ctx context.Context, client *mongo.Client, db, coll string, idxs []CapturedIndex, log *Logger) {
	if len(idxs) == 0 {
		return
	}
	models := make([]mongo.IndexModel, 0, len(idxs))
	for _, ix := range idxs {
		opt := options.Index().SetName(ix.Name).SetUnique(ix.Unique).SetSparse(ix.Sparse)
		if ix.ExpireAfterSeconds != nil {
			opt.SetExpireAfterSeconds(*ix.ExpireAfterSeconds)
		}
		if len(ix.PartialFilterExpression) > 0 {
			opt.SetPartialFilterExpression(ix.PartialFilterExpression)
		}
		if ix.Collation != nil {
			opt.SetCollation(ix.Collation)
		}
		models = append(models, mongo.IndexModel{Keys: ix.Keys, Options: opt})
	}

	if _, err := client.Database(db).Collection(coll).Indexes().CreateMany(ctx, models); err != nil {
		log.Warnf("重建 %s.%s 原有索引失败，请手工确认: %v", db, coll, err)
		return
	}
	log.Infof("已重建 %s.%s 原有索引 %d 个", db, coll, len(models))
}

// EnsureIndexes 补建迁移脚本内置的索引，跳过与已有索引同名的项。
func EnsureIndexes(ctx context.Context, client *mongo.Client, tbl *Table, skip map[string]bool, log *Logger) {
	if len(tbl.Indexes) == 0 {
		return
	}
	coll := client.Database(tbl.TargetDB).Collection(tbl.TargetColl)
	for _, spec := range tbl.Indexes {
		if skip[spec.Name] {
			continue
		}
		opt := options.Index().SetName(spec.Name)
		if spec.Unique {
			opt.SetUnique(true)
		}
		if _, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: spec.Keys, Options: opt}); err != nil {
			log.Warnf("创建索引 %s.%s[%s] 失败: %v", tbl.TargetDB, tbl.TargetColl, spec.Name, err)
			continue
		}
		log.Infof("已创建索引 %s.%s[%s]", tbl.TargetDB, tbl.TargetColl, spec.Name)
		skip[spec.Name] = true
	}
}
