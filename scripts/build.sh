#!/bin/sh
# 一键可复现构建：前端类型检查 + 构建 → 复制产物进 Go embed 目录 → Go 测试与编译。
#
# 顺序不能颠倒：Go 的 //go:embed all:dist 在编译期读取 internal/server/dist，
# 前端产物必须先生成，否则镜像里只有空占位页。
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

skip_tests=0
for arg in "$@"; do
  case "$arg" in
    --no-tests) skip_tests=1 ;;
    -h|--help)
      cat <<'EOF'
用法: ./scripts/build.sh [--no-tests]

  --no-tests   只构建，不运行 Go 测试（镜像构建中使用）
EOF
      exit 0
      ;;
    *)
      echo "未知参数: $arg" >&2
      exit 2
      ;;
  esac
done

need() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "缺少依赖: $1" >&2
    exit 1
  }
}

need node
need npm
need go

echo "==> 前端依赖"
if [ -f web/package-lock.json ]; then
  # CI 与可复现构建都走 npm ci：严格按 lockfile 安装。
  (cd web && npm ci --no-audit --no-fund)
else
  (cd web && npm install --no-audit --no-fund)
fi

echo "==> 前端类型检查与构建"
(cd web && npm run build)

echo "==> 同步前端产物到 internal/server/dist"
# 先清空再复制，避免上一轮的旧 hash 资源被一并内嵌。
rm -rf internal/server/dist
mkdir -p internal/server/dist
cp -R web/dist/. internal/server/dist/
# 保留占位文件：干净检出时 //go:embed all:dist 要求目录非空，否则编译失败。
: > internal/server/dist/.gitkeep
if [ ! -f internal/server/dist/index.html ]; then
  echo "前端构建产物缺少 index.html" >&2
  exit 1
fi

if [ "$skip_tests" -eq 0 ]; then
  echo "==> go vet"
  go vet ./...
  echo "==> go test"
  go test ./...
fi

echo "==> 编译 Go 二进制"
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/logview ./cmd/logview

echo "完成: dist/logview"
