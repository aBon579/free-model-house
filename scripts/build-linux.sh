#!/usr/bin/env bash
# 交叉编译一个 Linux 静态二进制（不需要 Docker）。
#
# 用法：
#   ./scripts/build-linux.sh              → ./fmh（当前架构）
#   ./scripts/build-linux.sh amd64        → ./fmh-linux-amd64
#   ./scripts/build-linux.sh arm64        → ./fmh-linux-arm64
#
# ⚠️ 为什么要这个脚本：
#   NAS 上没有 Docker、或者 Docker 拉不了基础镜像时，
#   可以**在这台机器上编译好**，把二进制传过去直接跑。
#
# ⚠️ 前端要**先**构建 —— 它会被 go:embed 打进二进制：
#   cd web && npm ci && npm run build
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ARCH="${1:-amd64}"

case "$ARCH" in
  amd64) GOARCH=amd64 ;;
  arm64) GOARCH=arm64 ;;
  *) echo "用法: $0 [amd64|arm64]" >&2; exit 1 ;;
esac

cd "$ROOT"

if [ ! -f internal/server/web/dist/index.html ]; then
  echo "前端还没构建 —— 先跑：cd web && npm ci && npm run build" >&2
  exit 1
fi

OUT="$ROOT/fmh-linux-$GOARCH"
echo "编译 $GOARCH → $OUT"

CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "$OUT" ./cmd/fmh

echo
echo "✅ $(du -h "$OUT" | cut -f1)  $OUT"
echo
echo "传到 NAS："
echo "  scp \"$OUT\" user@<NAS-IP>:~/"
echo
echo "在 NAS 上跑（不需要 Docker）："
echo "  mkdir -p ~/fmh-data"
echo "  FMH_ADDR=\":8678\" FMH_DATA=\"\$HOME/fmh-data\" ~/fmh-linux-$GOARCH"
