# 达梦→Redis用户数据缓存写入工具使用说明

## 概述

此工具用于将达梦数据库 `DLPDATA.did_system_sync_user` 表中的用户数据写入Redis缓存，适用于大规模数据处理场景（如120万用户数据）。

## 功能特性

- 从达梦数据库读取用户数据（uid, login_name, real_name, base_org_name）
- 将用户信息以JSON格式存储到Redis中
- 使用固定缓存键格式：`user:login_name:{login_name}`
- 不设置过期时间，数据永久存储
- 支持试运行模式，可在不影响生产数据的情况下验证功能
- 支持分批处理，降低大批量数据处理的风险
- 支持限制处理数量，便于测试和验证

## 环境要求

- Go 1.26.0 或更高版本（编译时需要）
- 达梦数据库访问权限
- Redis服务器访问权限

## 配置文件

在项目根目录创建 `.env` 文件，包含以下配置：

```ini
# 达梦数据库配置
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

# Redis配置
REDIS_HOST=10.8.27.151:30379    # Redis服务器地址和端口
REDIS_PASS=TopsecCdu_1130        # Redis密码
REDIS_DB=7                       # Redis数据库编号
```

## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-env` | 自动查找 | 配置文件路径 |
| `-dm-table` | `DLPDATA.did_system_sync_user` | 达梦源表全限定名 |
| `-conn-timeout` | `15s` | 数据库连接超时时间 |
| `-dry-run` | `false` | 试运行模式，不实际写入Redis |
| `-batch-size` | `1000` | 每批次处理的记录数量 |
| `-limit` | `0` | 限制处理的最大记录数量（0表示处理全部）|

## 使用步骤

### 1. 试运行验证（推荐首次执行）

```bash
# 验证连接和基本功能（处理前100条记录）
go run ./migrate_user_redis -dry-run -limit 100
```

### 2. 小规模测试

```bash
# 处理少量数据，验证数据完整性和性能
go run ./migrate_user_redis -limit 1000
```

### 3. 分批处理验证

```bash
# 使用较小批次处理，监控系统资源使用
go run ./migrate_user_redis -batch-size 500 -limit 5000
```

### 4. 全量数据处理

```bash
# 确认无误后，进行全量数据处理（使用1000条每批）
go run ./migrate_user_redis -batch-size 1000
```

### 5. 大批量处理（适用于120万数据）

```bash
# 使用较大批次以提高效率，同时监控资源使用
go run ./migrate_user_redis -batch-size 2000 -limit 0
```

## 安全建议（针对120万数据场景）

对于大规模数据处理，建议按以下步骤进行：

1. **连接验证**：
   ```bash
   # 先进行试运行，验证连接和基本功能
   go run ./migrate_user_redis -dry-run -limit 50
   ```

2. **小规模测试**：
   ```bash
   # 处理1000条记录，验证数据完整性和性能
   go run ./migrate_user_redis -limit 1000
   ```

3. **中等规模测试**：
   ```bash
   # 处理10000条记录，验证系统稳定性
   go run ./migrate_user_redis -batch-size 1000 -limit 10000
   ```

4. **分阶段全量处理**：
   ```bash
   # 分多次处理，每次处理10万条
   go run ./migrate_user_redis -batch-size 2000 -limit 100000
   
   # 或者全量处理，使用适当批次大小
   go run ./migrate_user_redis -batch-size 2000
   ```

## 输出示例

### 试运行模式输出
```bash
  用户表Redis缓存写入  DLPDATA.did_system_sync_user → redis://10.8.27.151:30379/db7
  配置文件    .env
  达梦        dm://SYSDBA:***@10.8.19.133:5236
  Redis       redis://10.8.27.151:***@10.8.27.151:30379/db7
  模式        试运行 dry-run，不会写入任何Redis数据
  批次大小    1000 条
  限制数量    1000 条

达梦连接成功
Redis连接成功
源表 DLPDATA.did_system_sync_user 共 1200000 条记录，开始写入Redis缓存...
试运行模式: 开始测试Redis写入能力...
试运行测试通过，模拟写入Redis缓存...
开始处理用户数据...
已处理 1000/1000 条，成功 998，失败 2

Redis缓存写入完成！
  模式        试运行结束（未写入数据）
总计处理: 1000 条
成功写入: 998 条
写入失败: 2 条
```

### 正常运行模式输出
```bash
  用户表Redis缓存写入  DLPDATA.did_system_sync_user → redis://10.8.27.151:30379/db7
  配置文件    .env
  达梦        dm://SYSDBA:***@10.8.19.133:5236
  Redis       redis://10.8.27.151:***@10.8.27.151:30379/db7
  批次大小    2000 条

达梦连接成功
Redis连接成功
源表 DLPDATA.did_system_sync_user 共 1200000 条记录，开始写入Redis缓存...
开始处理用户数据...
已处理 2000/1200000 条，成功 1998，失败 2
已处理 4000/1200000 条，成功 3996，失败 4
已处理 6000/1200000 条，成功 5994，失败 6
...

Redis缓存写入完成！
总计处理: 1200000 条
成功写入: 1198800 条
写入失败: 1200 条
```

## 缓存结构

在Redis中，每个用户信息存储为JSON字符串，键值格式为：
```
Key: user:login_name:{login_name}
Value: {"login_name":"...","real_name":"...","base_org_name":"..."}
```

例如：
- Key: `user:login_name:zhangsan`
- Value: `{"login_name":"zhangsan","real_name":"张三","base_org_name":"技术部"}`

## 注意事项

1. 确保Redis服务器有足够的内存空间存储所有用户数据
2. 确保达梦数据库连接配置正确
3. 如果某些记录的登录名为空，则会跳过缓存写入
4. 缓存写入失败的记录会在控制台输出错误信息
5. 不设置过期时间，如需过期策略请在Redis层面配置
6. 对于大量数据处理，建议在业务低峰期执行
7. 监控Redis服务器的CPU、内存和网络使用情况