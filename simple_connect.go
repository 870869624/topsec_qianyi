package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"time"

	_ "github.com/godoes/gorm-dameng"
	// 注意：这行需要在安装驱动后取消注释
	// _ "github.com/gotomicro/dm-go-driver/dm"
)

func main() {
	host := "10.8.19.133"
	port := 5236
	user := "SYSDBA"
	password := "SYSDBA"

	fmt.Printf("正在连接达梦数据库服务器: %s:%d\n", host, port)

	// 首先测试TCP连接
	address := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		fmt.Printf("❌ TCP连接失败: %v\n", err)
		return
	}

	defer conn.Close()
	fmt.Printf("✅ TCP连接成功！服务器响应正常。\n")
	fmt.Printf("   本地地址: %s\n", conn.LocalAddr())
	fmt.Printf("   远程地址: %s\n", conn.RemoteAddr())
	fmt.Printf("   连接已建立，准备进行数据库操作。\n\n")

	// 尝试连接到数据库并读取库信息
	connectAndReadDatabaseInfo(host, port, user, password)
}

func connectAndReadDatabaseInfo(host string, port int, user, password string) {
	fmt.Println("🔍 尝试连接并读取数据库信息...")

	// 构建连接字符串
	dsn := fmt.Sprintf("dm://%s:%s@%s:%d", user, password, host, port)

	// 检查驱动是否可用
	db, err := sql.Open("dm", dsn)
	if err != nil {
		fmt.Printf("⚠️  无法连接到数据库: %v\n", err)
		fmt.Println("💡 提示：需要先安装达梦数据库Go驱动")
		fmt.Println("   安装命令: go get github.com/gotomicro/dm-go-driver/dm")
		fmt.Println("   或从达梦官网下载驱动")
		fmt.Println()
		showDatabaseInfo()
		return
	}
	defer db.Close()

	// 设置连接参数
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(10 * time.Minute)

	// 测试连接
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		fmt.Printf("⚠️  数据库Ping失败: %v\n", err)
		showDatabaseInfo()
		return
	}

	fmt.Println("✅ 数据库连接成功！")
	fmt.Println()

	// 读取数据库信息
	readSchemas(db)

	// 执行数据库测试
	performDatabaseTests(db)
}

func readSchemas(db *sql.DB) {
	fmt.Println("📋 数据库模式信息:")
	fmt.Println("==================")

	// 查询所有用户模式
	fmt.Println("1. 查询所有用户模式:")
	rows, err := db.Query("SELECT DISTINCT owner FROM all_tables ORDER BY owner")
	if err != nil {
		fmt.Printf("   ❌ 查询失败: %v\n", err)
	} else {
		defer rows.Close()
		count := 0
		for rows.Next() {
			var owner string
			if err := rows.Scan(&owner); err != nil {
				fmt.Printf("   ❌ 扫描失败: %v\n", err)
				break
			}
			fmt.Printf("   - %s\n", owner)
			count++
		}
		if count == 0 {
			fmt.Println("   (未找到任何模式)")
		}
	}

	fmt.Println()

	// 查询系统用户
	fmt.Println("2. 查询系统用户:")
	rows, err = db.Query("SELECT username FROM dba_users WHERE account_status = 'OPEN' ORDER BY username")
	if err != nil {
		fmt.Printf("   ❌ 查询失败: %v\n", err)
	} else {
		defer rows.Close()
		count := 0
		for rows.Next() {
			var username string
			if err := rows.Scan(&username); err != nil {
				fmt.Printf("   ❌ 扫描失败: %v\n", err)
				break
			}
			fmt.Printf("   - %s\n", username)
			count++
		}
		if count == 0 {
			fmt.Println("   (未找到任何用户)")
		}
	}

	fmt.Println()

	// 查询数据库名称
	fmt.Println("3. 数据库基本信息:")
	var dbName string
	err = db.QueryRow("SELECT name FROM v$database").Scan(&dbName)
	if err != nil {
		fmt.Printf("   ❌ 查询数据库名称失败: %v\n", err)
	} else {
		fmt.Printf("   - 数据库名称: %s\n", dbName)
	}

	var version string
	err = db.QueryRow("SELECT version() FROM dual").Scan(&version)
	if err != nil {
		fmt.Printf("   ❌ 查询数据库版本失败: %v\n", err)
	} else {
		fmt.Printf("   - 数据库版本: %s\n", version)
	}
}

func performDatabaseTests(db *sql.DB) {
	fmt.Println()
	fmt.Println("🧪 开始数据库功能测试:")
	fmt.Println("=====================")

	// 创建测试表
	createTableSQL := `
CREATE TABLE IF NOT EXISTS test_users (
    id INT PRIMARY KEY IDENTITY,
    username VARCHAR(50) NOT NULL,
    email VARCHAR(100),
    age INT,
    created_time TIMESTAMP DEFAULT CURRENT_TIMESTAMP
)`

	fmt.Println("1. 创建测试表 test_users...")
	_, err := db.Exec(createTableSQL)
	if err != nil {
		fmt.Printf("   ❌ 创建表失败: %v\n", err)
	} else {
		fmt.Println("   ✅ 表创建成功")
	}

	// 插入测试数据
	fmt.Println("2. 插入测试数据...")
	insertSQL := "INSERT INTO test_users (username, email, age) VALUES (?, ?, ?)"
	testUsers := []struct {
		username string
		email    string
		age      int
	}{
		{"张三", "zhangsan@example.com", 25},
		{"李四", "lisi@example.com", 30},
		{"王五", "wangwu@example.com", 28},
	}

	for i, user := range testUsers {
		_, err := db.Exec(insertSQL, user.username, user.email, user.age)
		if err != nil {
			fmt.Printf("   ❌ 插入第%d条数据失败: %v\n", i+1, err)
		} else {
			fmt.Printf("   ✅ 插入第%d条数据成功: %s (%d岁)\n", i+1, user.username, user.age)
		}
	}

	// 查询所有数据
	fmt.Println("3. 查询所有用户...")
	rows, err := db.Query("SELECT id, username, email, age, created_time FROM test_users ORDER BY id")
	if err != nil {
		fmt.Printf("   ❌ 查询失败: %v\n", err)
	} else {
		defer rows.Close()
		fmt.Println("   ID | 用户名 | 邮箱 | 年龄 | 创建时间")
		fmt.Println("   ----------------------------------------")
		for rows.Next() {
			var id int
			var username, email string
			var age int
			var createdTime time.Time
			if err := rows.Scan(&id, &username, &email, &age, &createdTime); err != nil {
				fmt.Printf("   ❌ 扫描数据失败: %v\n", err)
				break
			}
			fmt.Printf("   %d  | %s | %s | %d | %s\n", id, username, email, age, createdTime.Format("2006-01-02 15:04:05"))
		}
	}

	// 查询特定用户
	fmt.Println("4. 查询年龄大于27的用户...")
	rows, err = db.Query("SELECT username, age FROM test_users WHERE age > ?", 27)
	if err != nil {
		fmt.Printf("   ❌ 查询条件数据失败: %v\n", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var username string
			var age int
			if err := rows.Scan(&username, &age); err != nil {
				fmt.Printf("   ❌ 扫描条件数据失败: %v\n", err)
				break
			}
			fmt.Printf("   ✅ 找到用户: %s (%d岁)\n", username, age)
		}
	}

	// 统计总数
	fmt.Println("5. 统计用户总数...")
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM test_users").Scan(&count)
	if err != nil {
		fmt.Printf("   ❌ 统计失败: %v\n", err)
	} else {
		fmt.Printf("   ✅ 总共 %d 个用户\n", count)
	}

	fmt.Println()
	fmt.Println("🎉 数据库功能测试完成！")
}

func showDatabaseInfo() {
	fmt.Println()
	fmt.Println("📋 达梦数据库信息:")
	fmt.Println("==================")
	fmt.Println("在达梦数据库中，库的概念对应的是'模式'(Schema)")
	fmt.Println("可以通过以下SQL语句查询数据库中的模式信息：")
	fmt.Println()
	fmt.Println("1. 查询所有用户模式：")
	fmt.Println("   SELECT owner FROM all_tables GROUP BY owner;")
	fmt.Println()
	fmt.Println("2. 查询当前用户的模式对象：")
	fmt.Println("   SELECT owner, object_type, COUNT(*) FROM all_objects GROUP BY owner, object_type;")
	fmt.Println()
	fmt.Println()
	fmt.Println("3. 查询系统模式：")
	fmt.Println("   SELECT username FROM dba_users WHERE account_status = 'OPEN';")
	fmt.Println()
	fmt.Println("💡 注意：要执行这些查询，需要使用达梦官方Go驱动连接后执行SQL语句")
	fmt.Println("   目前已确认服务器网络可达，下一步可以安装驱动进行数据库查询")
}
