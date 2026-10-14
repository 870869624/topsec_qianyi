// Command migrate_user_redis 把达梦 DLPDATA.did_system_sync_user 的用户数据迁移到 Redis 缓存。
//
//	go run ./migrate_user_redis   # 将达梦用户表数据写入Redis缓存
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"shutong/syncbase"

	_ "github.com/godoes/gorm-dameng/dm8" // 注册 database/sql 驱动
	"github.com/redis/go-redis/v9"
)

const (
	defaultSourceTable = "DLPDATA.did_system_sync_user"
)

// UserInfo 缓存的用户信息结构
type UserInfo struct {
	LoginName   string `json:"login_name"`
	RealName    string `json:"real_name"`
	BaseOrgName string `json:"base_org_name"`
}

var (
	envPath     = flag.String("env", "", "配置文件路径；留空则自动从当前目录逐级向上查找 .env")
	sourceTable = flag.String("dm-table", defaultSourceTable, "达梦源表全限定名（不带引号）")
	connTimeout = flag.Duration("conn-timeout", 15*time.Second, "数据库连接超时")
	dryRun      = flag.Bool("dry-run", false, "试运行模式，不实际写入Redis")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n✗ Redis缓存写入失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// ---------------- 1. 读取配置 ----------------
	cfg, err := syncbase.LoadConfig(*envPath)
	if err != nil {
		return err
	}

	// 读取Redis配置
	redisCfg, err := loadRedisConfig(*envPath)
	if err != nil {
		return fmt.Errorf("读取Redis配置失败: %w", err)
	}

	fullTable := *sourceTable

	fmt.Printf("  用户表Redis缓存写入  %s → redis://%s/db%d\n", fullTable, redisCfg.Host, redisCfg.DB)
	fmt.Printf("  配置文件    %s\n", cfg.File)
	fmt.Printf("  达梦        %s\n", cfg.DM.SafeDSN())
	fmt.Printf("  Redis       %s\n", redisCfg.SafeAddr())
	if *dryRun {
		fmt.Printf("  模式        试运行 dry-run，不会写入任何Redis数据\n")
	}

	fmt.Println()

	// ---------------- 2. 建立连接 ----------------
	ctx := context.Background()

	dm, err := syncbase.OpenDM(ctx, cfg.DM, 500, *connTimeout)
	if err != nil {
		return err
	}
	defer dm.Close()
	fmt.Println("达梦连接成功")

	redisClient := redis.NewClient(&redis.Options{
		Addr:     redisCfg.Host,
		Password: redisCfg.Pass,
		DB:       redisCfg.DB,
	})
	defer func() { _ = redisClient.Close() }()

	// 测试Redis连接
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("Redis连接失败: %w", err)
	}
	fmt.Println("Redis连接成功")

	// ---------------- 3. 清除已有缓存 ----------------
	if !*dryRun {
		fmt.Println("开始清除已有 Redis 缓存...")
		cleared, err := clearExistingCache(ctx, redisClient)
		if err != nil {
			return fmt.Errorf("清除缓存失败: %w", err)
		}
		fmt.Printf("已清除 %d 个缓存键\n", cleared)
	} else {
		fmt.Println("试运行模式: 跳过清除缓存")
	}

	// ---------------- 4. 加载 org 表映射 (base_org_id -> base_org_desc) ----------------
	fmt.Println("加载组织表映射...")
	orgMap, err := loadOrgMap(ctx, dm, cfg, fullTable)
	if err != nil {
		return fmt.Errorf("加载组织表映射失败: %w", err)
	}
	fmt.Printf("加载组织映射 %d 条\n", len(orgMap))

	// ---------------- 5. 执行数据迁移至Redis缓存 ----------------
	total, err := syncbase.CountSource(ctx, dm, quoteTableName(fullTable))
	if err != nil {
		return fmt.Errorf("统计源表行数失败: %w", err)
	}

	// 显示源表记录总数
	fmt.Printf("源表 %s 共 %d 条记录，开始写入Redis缓存...\n", fullTable, total)

	// 查询达梦数据并写入Redis
	// 查询需要的字段: uid, login_name, real_name, base_org_id, base_org_name
	// 使用达梦的双引号格式确保表名正确引用
	sqlText := fmt.Sprintf("SELECT \"uid\", \"login_name\", \"real_name\", \"base_org_id\", \"base_org_name\" FROM %s", quoteTableName(fullTable))

	// 如果是试运行，先进行测试写入
	if *dryRun {
		fmt.Println("试运行模式: 开始测试Redis写入能力...")
		testResult := testRedisWriteCapability(ctx, redisClient)
		if testResult != nil {
			return fmt.Errorf("试运行测试失败: %w", testResult)
		}
		fmt.Println("试运行测试通过，模拟写入Redis缓存...")
	}

	rows, err := dm.QueryContext(ctx, sqlText)
	if err != nil {
		return fmt.Errorf("查询源表失败: %w", err)
	}
	defer rows.Close()

	fmt.Println("开始处理用户数据...")
	scanned := 0
	successCount := 0
	failureCount := 0

	for rows.Next() {
		var uid, loginName, realName, baseOrgID, baseOrgName sql.NullString
		err := rows.Scan(&uid, &loginName, &realName, &baseOrgID, &baseOrgName)
		if err != nil {
			failureCount++
			fmt.Printf("读取第 %d 行数据失败: %v\n", scanned+1, err)
			continue
		}

		scanned++

		// 构建用户信息：优先用 org 表的 base_org_desc，降级用 user 表的 base_org_name
		resolvedOrgName := resolveOrgName(syncbase.Str(baseOrgID), syncbase.Str(baseOrgName), orgMap)
		userInfo := &UserInfo{
			LoginName:   syncbase.Str(loginName),
			RealName:    syncbase.Str(realName),
			BaseOrgName: resolvedOrgName,
		}

		// 只有当登录名不为空时才写入缓存
		if userInfo.LoginName != "" {
			// 根据是否试运行决定是否真正写入Redis
			if *dryRun {
				// 试运行模式，仅模拟写入
				if err := simulateSetUserInfoToCache(ctx, redisClient, userInfo); err != nil {
					failureCount++
					fmt.Printf("第 %d 条数据模拟写入缓存失败 (登录名: %s): %v\n", scanned, userInfo.LoginName, err)
				} else {
					successCount++
				}
			} else {
				// 正常模式，真正写入Redis
				if err := setUserInfoToCache(ctx, redisClient, userInfo); err != nil {
					failureCount++
					fmt.Printf("第 %d 条数据写入缓存失败 (登录名: %s): %v\n", scanned, userInfo.LoginName, err)
				} else {
					successCount++
				}
			}
		} else {
			fmt.Printf("第 %d 条数据登录名为空，跳过缓存写入\n", scanned)
		}

		// 显示进度 (每1000条显示一次)
		if scanned%1000 == 0 {
			fmt.Printf("已处理 %d/%d 条，成功 %d，失败 %d\n", scanned, total, successCount, failureCount)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历结果集出错: %w", err)
	}

	fmt.Printf("\nRedis缓存写入完成！\n")
	if *dryRun {
		fmt.Printf("  模式        试运行结束（未写入数据）\n")
	}
	fmt.Printf("总计处理: %d 条\n", scanned)
	fmt.Printf("成功写入: %d 条\n", successCount)
	fmt.Printf("写入失败: %d 条\n", failureCount)

	return nil
}

// quoteTableName 将表名格式化为达梦数据库可识别的格式
// 例如: DLPDATA.did_system_sync_user -> "DLPDATA"."did_system_sync_user"
func quoteTableName(tableName string) string {
	parts := strings.Split(tableName, ".")
	if len(parts) == 2 {
		return fmt.Sprintf(`"%s"."%s"`, parts[0], parts[1])
	}
	return fmt.Sprintf(`"%s"`, tableName)
}

// setUserInfoToCache 将用户信息设置到缓存
func setUserInfoToCache(ctx context.Context, redisClient *redis.Client, userInfo *UserInfo) error {
	if userInfo.LoginName == "" {
		return fmt.Errorf("用户登录名为空，无法设置缓存")
	}

	key := buildLoginNameKey(userInfo.LoginName)
	jsonData, err := json.Marshal(userInfo)
	if err != nil {
		return err
	}

	// 不设置过期时间，永久存储
	return redisClient.Set(ctx, key, string(jsonData), time.Duration(0)).Err()
}

// simulateSetUserInfoToCache 模拟将用户信息设置到缓存（试运行模式）
func simulateSetUserInfoToCache(ctx context.Context, redisClient *redis.Client, userInfo *UserInfo) error {
	if userInfo.LoginName == "" {
		return fmt.Errorf("用户登录名为空，无法设置缓存")
	}

	// 在试运行模式下，只是测试Redis连接能力而不真正写入
	return testRedisWriteCapability(ctx, redisClient)
}

// testRedisWriteCapability 测试Redis写入能力
func testRedisWriteCapability(ctx context.Context, redisClient *redis.Client) error {
	testKey := fmt.Sprintf("test:connection:%d", time.Now().Unix())
	testValue := "test_value"

	// 尝试写入一个测试值
	err := redisClient.Set(ctx, testKey, testValue, time.Duration(0)).Err()
	if err != nil {
		return err
	}

	// 读取测试值验证
	result, err := redisClient.Get(ctx, testKey).Result()
	if err != nil {
		return err
	}

	// 删除测试值
	_ = redisClient.Del(ctx, testKey).Err()

	if result != testValue {
		return fmt.Errorf("Redis读写测试失败，期望值: %s, 实际值: %s", testValue, result)
	}

	return nil
}

// resolveOrgName 解析组织名称：优先用 org 表的 base_org_desc，没查到则降级用 user 表的 base_org_name
func resolveOrgName(baseOrgID, baseOrgName string, orgMap map[string]string) string {
	if baseOrgID == "" {
		// 没有 base_org_id，直接返回 user 表的 base_org_name
		return baseOrgName
	}
	// 尝试从 org 映射中查找
	if desc, ok := orgMap[baseOrgID]; ok && desc != "" {
		return desc
	}
	// 没查到，降级用 user 表的 base_org_name
	return baseOrgName
}

// loadOrgMap 从 org 表加载 base_org_id -> base_org_desc 映射
func loadOrgMap(ctx context.Context, dm *sql.DB, cfg *syncbase.Config, userTable string) (map[string]string, error) {
	// 从 userTable 提取模式名 (如 DLPDATA)
	parts := strings.Split(userTable, ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("源表名格式错误，应为 模式.表名: %s", userTable)
	}
	schema := parts[0]
	orgTable := fmt.Sprintf(`"%s"."did_system_sync_org"`, schema)

	sqlText := fmt.Sprintf("SELECT \"base_org_id\", \"base_org_desc\" FROM %s", orgTable)
	rows, err := dm.QueryContext(ctx, sqlText)
	if err != nil {
		return nil, fmt.Errorf("查询组织表失败: %w", err)
	}
	defer rows.Close()

	orgMap := make(map[string]string)
	for rows.Next() {
		var orgID, orgDesc sql.NullString
		if err := rows.Scan(&orgID, &orgDesc); err != nil {
			continue // 跳过解析失败的行
		}
		if orgID.Valid && orgID.String != "" {
			desc := ""
			if orgDesc.Valid {
				desc = orgDesc.String
			}
			orgMap[orgID.String] = desc
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历组织表结果集出错: %w", err)
	}
	return orgMap, nil
}

// clearExistingCache 清除所有 user:login_name:* 模式的缓存键
func clearExistingCache(ctx context.Context, redisClient *redis.Client) (int64, error) {
	var cleared int64
	var cursor uint64
	pattern := "user:login_name:*"

	// 使用 SCAN 命令安全遍历（不会阻塞 Redis）
	for {
		keys, nextCursor, err := redisClient.Scan(ctx, cursor, pattern, 1000).Result()
		if err != nil {
			return cleared, fmt.Errorf("SCAN 命令失败: %w", err)
		}

		if len(keys) > 0 {
			delCount, err := redisClient.Del(ctx, keys...).Result()
			if err != nil {
				return cleared, fmt.Errorf("删除缓存键失败: %w", err)
			}
			cleared += delCount
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return cleared, nil
}

// buildLoginNameKey 构建以登录名为键的Redis键
func buildLoginNameKey(loginName string) string {
	return fmt.Sprintf("user:login_name:%s", loginName)
}

// RedisConfig Redis配置结构
type RedisConfig struct {
	Host string
	Pass string
	DB   int
}

// SafeAddr 获取脱敏的Redis地址
func (r *RedisConfig) SafeAddr() string {
	password := "******"
	if r.Pass != "" {
		password = "******"
	}
	addrParts := strings.Split(r.Host, ":")
	if len(addrParts) == 2 {
		return fmt.Sprintf("redis://%s:%s@%s:%s/db%d", r.Host, password, addrParts[0], addrParts[1], r.DB)
	}
	return fmt.Sprintf("redis://%s:%s@%s/db%d", r.Host, password, r.Host, r.DB)
}

// loadRedisConfig 从环境变量或配置文件加载Redis配置
func loadRedisConfig(envPath string) (*RedisConfig, error) {
	// 尝试加载配置文件以获取其中的Redis配置
	var envVars map[string]string

	// 先尝试加载配置文件
	_, err := syncbase.LoadConfig(envPath)
	if err == nil {
		// 如果syncbase.Config中包含FileVars，我们可以使用它
		envVars = make(map[string]string)

		// 尝试从配置文件路径读取更多变量
		absPath, _ := os.Getwd()
		if envPath != "" {
			absPath = envPath
		} else {
			// 查找.env文件
			searchPaths := []string{".env", "./.env", "../.env"}
			for _, path := range searchPaths {
				if _, err := os.Stat(path); err == nil {
					absPath = path
					break
				}
			}
		}

		// 重新读取包含Redis配置的文件
		if absPath != "" {
			if f, err := os.Open(absPath); err == nil {
				defer f.Close()
				scanner := bufio.NewScanner(f)
				for scanner.Scan() {
					line := strings.TrimSpace(scanner.Text())
					if line == "" || strings.HasPrefix(line, "#") {
						continue
					}
					if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
						key := strings.TrimSpace(parts[0])
						value := strings.TrimSpace(parts[1])
						if strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"") {
							value = strings.Trim(value, "\"")
						}
						envVars[key] = value
					}
				}
			}
		}
	} else {
		envVars = make(map[string]string)
	}

	// 从环境变量或配置文件获取Redis配置
	redisHost := os.Getenv("REDIS_HOST")
	if redisHost == "" {
		redisHost = envVars["REDIS_HOST"]
	}
	if redisHost == "" {
		redisHost = "localhost:6379" // 默认值
	}

	redisPass := os.Getenv("REDIS_PASS")
	if redisPass == "" {
		redisPass = envVars["REDIS_PASS"]
	}

	redisDB := os.Getenv("REDIS_DB")
	if redisDB == "" {
		redisDB = envVars["REDIS_DB"]
	}
	if redisDB == "" {
		redisDB = "0" // 默认值
	}

	dbNum := 0
	fmt.Sscanf(redisDB, "%d", &dbNum)

	return &RedisConfig{
		Host: redisHost,
		Pass: redisPass,
		DB:   dbNum,
	}, nil
}
