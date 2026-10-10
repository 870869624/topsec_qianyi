#!/bin/bash

# 交叉编译脚本
# 用法：
#   bash build.sh                           # 默认只出 linux/amd64
#   bash build.sh linux/amd64 linux/arm64   # 一次出多个架构
#   OUT_DIR=/tmp/out bash build.sh linux/amd64   # 自定义输出目录

set -e

readonly APPS=(
  migrate_user
  migrate_org
  dmcheck
  migrate_user_redis  # 新增的Redis缓存迁移工具
)

out_dir=${OUT_DIR:-./dist}
mkdir -p "$out_dir"

platforms=("${@:-linux/amd64}")

for platform in "${platforms[@]}"; do
  os_arch=(${platform/\// })
  os="${os_arch[0]}"
  arch="${os_arch[1]}"
  
  echo "Building for $platform..."
  
  for app in "${APPS[@]}"; do
    if [[ "$app" == "dmcheck" ]]; then
      # dmcheck 是单文件，不依赖 syncbase
      env CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
        go build -trimpath -ldflags "-s -w" -o "$out_dir/${app}_${os}_${arch}" "./${app}/"
    else
      # 其他应用依赖 syncbase
      env CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
        go build -trimpath -ldflags "-s -w" -o "$out_dir/${app}_${os}_${arch}" "./${app}/"
    fi
  done
done

echo "Build complete. Outputs in $out_dir:"
ls -lh "$out_dir/"