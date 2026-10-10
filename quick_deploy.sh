#!/bin/bash

# 达梦→Redis用户数据缓存写入工具 - 服务器快速部署脚本

set -e

echo "==========================================="
echo "达梦→Redis用户数据缓存写入工具 - 服务器部署"
echo "==========================================="

# 检查是否在项目目录中
if [ ! -f "migrate_user_redis/main.go" ]; then
    echo "错误: 未在项目根目录中，请切换到项目根目录后再运行此脚本"
    exit 1
fi

echo "1. 检查Go版本..."
GO_VERSION=$(go version | grep -o "go[0-9.]*" | sed 's/go//')
echo "检测到Go版本: $GO_VERSION"

# 检查Go版本是否满足要求
MIN_VERSION="1.26.0"
VERSION_CHECK=$(printf '%s\n%s' "$MIN_VERSION" "$GO_VERSION" | sort -V | head -n1)
if [ "$VERSION_CHECK" = "$MIN_VERSION" ]; then
    echo "✓ Go版本检查通过"
else
    echo "× Go版本过低，需要至少 $MIN_VERSION"
    echo "  当前版本: $GO_VERSION"
    echo "  请升级Go版本后再运行此脚本"
    exit 1
fi

echo ""
echo "2. 创建输出目录..."
OUT_DIR="./dist"
mkdir -p "$OUT_DIR"

echo ""
echo "3. 编译 Linux amd64 版本..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$OUT_DIR/migrate_user_redis_linux_amd64" ./migrate_user_redis/

echo ""
echo "4. 编译完成!"
BINARY_PATH="$OUT_DIR/migrate_user_redis_linux_amd64"
if [ -f "$BINARY_PATH" ]; then
    echo "✓ 二进制文件已生成: $BINARY_PATH"
    echo "文件大小:"
    ls -lh "$BINARY_PATH"
else
    echo "× 编译失败，未生成二进制文件"
    exit 1
fi

echo ""
echo "5. 生成服务器部署脚本..."

# 创建服务器部署脚本
cat > deploy_to_server.sh << 'SCRIPT_EOF'
#!/bin/bash

# 服务器部署脚本模板
# 请根据实际服务器信息修改以下变量

SERVER_HOST=""  # 服务器IP地址
SERVER_USER=""  # 服务器用户名
REMOTE_DIR="/opt/migrate-tools"  # 服务器上的目标目录

if [ -z "$SERVER_HOST" ] || [ -z "$SERVER_USER" ]; then
    echo "请先编辑此脚本，设置 SERVER_HOST 和 SERVER_USER 变量"
    exit 1
fi

echo "开始部署到服务器 $SERVER_USER@$SERVER_HOST..."

# 创建远程目录
ssh "$SERVER_USER@$SERVER_HOST" "mkdir -p $REMOTE_DIR && chmod 755 $REMOTE_DIR"

# 上传二进制文件
scp migrate_user_redis_linux_amd64 "$SERVER_USER@$SERVER_HOST:$REMOTE_DIR/"

# 上传部署指南
scp ../../SERVER_DEPLOY.md "$SERVER_USER@$SERVER_HOST:$REMOTE_DIR/"

echo "上传完成!"
echo "请在服务器上执行以下操作："
echo "1. ssh $SERVER_USER@$SERVER_HOST"
echo "2. cd $REMOTE_DIR"
echo "3. chmod +x migrate_user_redis_linux_amd64"
echo "4. 参考 SERVER_DEPLOY.md 进行后续部署"
SCRIPT_EOF

chmod +x deploy_to_server.sh

echo "✓ 服务器部署脚本已生成: deploy_to_server.sh"

echo ""
echo "6. 生成使用说明..."

# 创建使用说明
cat > quick_start.md << 'USAGE_EOF'
# 达梦→Redis用户数据缓存写入工具 - 快速开始

## 本地测试

### 1. 试运行验证
```bash
# 验证连接和基本功能（处理前100条记录）
go run ./migrate_user_redis -dry-run -limit 100
```

### 2. 小规模测试
```bash
# 处理少量数据，验证数据完整性和性能
go run ./migrate_user_redis -limit 1000
```

## 服务器部署

### 1. 编译Linux版本
```bash
# 使用编译脚本
bash build_linux_amd64.sh
```

### 2. 上传到服务器
```bash
# 修改 deploy_to_server.sh 中的服务器信息后运行
bash deploy_to_server.sh
```

### 3. 服务器上运行

#### 试运行验证
```bash
# 验证连接（处理前100条记录）
./migrate_user_redis_linux_amd64 -dry-run -limit 100
```

#### 正式运行
```bash
# 处理全部数据，使用2000条每批
./migrate_user_redis_linux_amd64 -batch-size 2000
```

#### 大数据量分批处理
```bash
# 分批处理120万数据
nohup ./migrate_user_redis_linux_amd64 -batch-size 2000 > migrate.log 2>&1 &
```

## 主要参数说明

- `-dry-run`: 试运行模式，不实际写入Redis
- `-batch-size`: 每批次处理的记录数量（默认1000）
- `-limit`: 限制处理的最大记录数量（0表示全部）
- `-env`: 指定配置文件路径
- `-conn-timeout`: 数据库连接超时时间

## 重要提醒

1. **数据安全**：在生产环境运行前，务必先在测试环境验证
2. **资源监控**：处理120万数据时，监控服务器资源使用情况
3. **备份准备**：在运行前确保Redis有备份方案
4. **网络稳定**：确保达梦数据库连接稳定
</>
USAGE_EOF

echo "✓ 快速开始说明已生成: quick_start.md"

echo ""
echo "==========================================="
echo "编译和打包完成!"
echo "==========================================="
echo ""
echo "生成的文件："
echo "- $BINARY_PATH (Linux amd64 二进制文件)"
echo "- deploy_to_server.sh (服务器部署脚本)"
echo "- quick_start.md (快速开始说明)"
echo "- USAGE.md (详细使用说明)"
echo "- SERVER_DEPLOY.md (服务器部署指南)"
echo ""
echo "接下来的步骤："
echo "1. 检查编译的二进制文件: $BINARY_PATH"
echo "2. 如需部署到服务器，编辑 deploy_to_server.sh 并运行"
echo "3. 详细使用说明请查看 quick_start.md 和 USAGE.md"
echo ""
echo "对于120万数据的大规模处理，建议："
echo "- 先在测试环境验证功能"
echo "- 使用 -dry-run 参数验证连接"
echo "- 选择合适的 -batch-size (推荐2000)"
echo "- 使用 nohup 后台运行长时间任务"