# 达梦 → Redis 用户数据缓存写入工具

将达梦数据库 `DLPDATA.did_system_sync_user` 表中的用户数据写入Redis缓存。

## 功能说明

- 从达梦数据库读取用户数据（uid, login_name, real_name, base_org_name）
- 将用户信息以JSON格式存储到Redis中
- 使用固定缓存键格式：`user:login_name:{login_name}`
- 不设置过期时间，数据永久存储

## 配置要求

在 `.env` 文件中添加Redis连接配置：

```ini
# Redis配置
REDIS_HOST=localhost:6379    # Redis服务器地址和端口
REDIS_PASS=                  # Redis密码（如果没有密码则留空）
REDIS_DB=0                   # Redis数据库编号

# 达梦数据库配置（已有）
DM_HOST=10.8.19.133
DM_PORT=5236
DM_USER=SYSDBA
DM_PASS=SYSDBA

# MongoDB配置（可选，如果需要的话）
MONGO_HOST=10.8.27.151
MONGO_PORT=32017
MONGO_DB=identity
MONGO_USERNAME=root
MONGO_PASSWORD=TopsecCdu_1130
```

## 使用方法

### 1. 直接运行（开发环境）

```bash
# 使用当前目录的 .env 文件
go run ./migrate_user_redis

# 指定配置文件路径
go run ./migrate_user_redis -env /path/to/your/.env

# 指定源表名（如果表名不是默认的 DLPDATA.did_system_sync_user）
go run ./migrate_user_redis -dm-table YOUR_SCHEMA.YOUR_TABLE_NAME
```

### 2. 编译后运行

```bash
# 编译（需要先安装Go环境）
go build -o migrate_user_redis ./migrate_user_redis/

# 运行
./migrate_user_redis
```

### 3. 使用构建脚本编译多平台

```bash
# 编译所有平台（需要先修改build.sh脚本）
bash build.sh linux/amd64 linux/arm64
```

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-env` | 自动查找 | 配置文件路径 |
| `-dm-table` | `DLPDATA.did_system_sync_user` | 达梦源表全限定名 |
| `-conn-timeout` | `15s` | 数据库连接超时时间 |

## 缓存结构

在Redis中，每个用户数据以如下格式存储：

```
Key: user:login_name:{login_name}
Value: {"login_name":"...","real_name":"...","base_org_name":"..."}
```

例如：
- Key: `user:login_name:zhangsan`
- Value: `{"login_name":"zhangsan","real_name":"张三","base_org_name":"技术部"}`

## 运行示例

```bash
$ go run ./migrate_user_redis
  用户表Redis缓存写入  DLPDATA.did_system_sync_user → redis://localhost:6379/db0
  配置文件    /path/to/.env
  达梦        dm://SYSDBA:***@10.8.19.133:5236
  Redis       redis://localhost:***@localhost:6379/db0

达梦连接成功
Redis连接成功
源表 DLPDATA.did_system_sync_user 共 400000 条记录，开始写入Redis缓存...
开始处理用户数据...
已处理 1000/400000 条，成功 998，失败 2
已处理 2000/400000 条，成功 1996，失败 4

Redis缓存写入完成！
总计处理: 400000 条
成功写入: 399800 条
写入失败: 200 条
```

## 注意事项

1. 确保Redis服务器可访问且配置正确
2. 确保达梦数据库连接配置正确
3. 如果某些记录的登录名为空，则会跳过缓存写入
4. 缓存写入失败的记录会在控制台输出错误信息
5. 不设置过期时间，如需过期策略请在Redis层面配置