package syncbase

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/godoes/gorm-dameng/dm8" // 注册达梦 database/sql 驱动，驱动名 "dm"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// OpenDM 建立达梦连接并做一次 Ping 验证。
// rowPrefetch 控制每批预取行数，直接决定流式读取时的内存占用。
func OpenDM(ctx context.Context, cfg *DMConfig, rowPrefetch int, timeout time.Duration) (*sql.DB, error) {
	db, err := sql.Open("dm", cfg.DSN(rowPrefetch))
	if err != nil {
		return nil, fmt.Errorf("打开达梦连接失败: %w", err)
	}

	// 迁移是「单点串行读取 + 并发写入」，读连接不需要多，避免把源库连接池占满。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(10 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("达梦连接失败(%s): %w", cfg.SafeDSN(), err)
	}
	return db, nil
}

// OpenMongo 建立 MongoDB 连接并做一次 Ping 验证。
//
// 这里用 SetHosts + SetAuth 组装连接，而不是拼接 mongodb:// 连接串——
// 账号密码交给驱动处理，省掉自己转义特殊字符（@ : / 等）的麻烦，
// 也就不会出现密码里的特殊字符把连接串拼坏这类问题。
func OpenMongo(ctx context.Context, cfg *MongoConfig, timeout time.Duration) (*mongo.Client, error) {
	opts := options.Client().
		SetHosts([]string{cfg.Addr()}).
		SetConnectTimeout(timeout).
		SetServerSelectionTimeout(timeout).
		SetMaxPoolSize(30).
		SetRetryWrites(false)

	if cfg.User != "" {
		opts.SetAuth(options.Credential{
			Username:   cfg.User,
			Password:   cfg.Password,
			AuthSource: cfg.AuthSource,
		})
	}

	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("连接 MongoDB 失败: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("MongoDB 连接失败(%s): %w", cfg.SafeURI(), err)
	}
	return client, nil
}
