#!/usr/bin/env bash
# =============================================================================
# 构建两个镜像并打成可离线搬运的 tar
# =============================================================================
#
# 为什么不用 registry:少一个依赖、少一份凭据、也不用担心把二开镜像推到公开仓库。
# 一条 scp 就能把镜像搬过去。
#
# 用法（在本机,两个仓库都在本地）:
#   RESIN_REPO=/path/to/Resin bash build.sh
#
# 产出:
#   images-<时间戳>.tar.gz   两个镜像
# =============================================================================
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUB2API_REPO="${SUB2API_REPO:-$(cd "$HERE/../.." && pwd)}"
RESIN_REPO="${RESIN_REPO:-$HOME/Desktop/project/Resin}"
TS="$(date +%Y%m%d-%H%M%S)"

SUB2API_IMAGE="${SUB2API_IMAGE:-sub2api-stack/sub2api:local}"
RESIN_IMAGE="${RESIN_IMAGE:-sub2api-stack/resin:local}"

[ -r "$SUB2API_REPO/deploy/Dockerfile" ] || { echo "找不到 $SUB2API_REPO/deploy/Dockerfile" >&2; exit 1; }
[ -r "$RESIN_REPO/Dockerfile" ] || { echo "找不到 $RESIN_REPO/Dockerfile（用 RESIN_REPO= 指定）" >&2; exit 1; }

# 目标机是 x86_64。在 Apple Silicon 上构建必须显式指定平台,
# 否则会产出 arm64 镜像,而它在目标机上**能 load、不能跑**,报 exec format error。
PLATFORM="${PLATFORM:-linux/amd64}"

echo "[1/3] 构建 sub2api（$PLATFORM）..."
docker build --platform "$PLATFORM" \
  -f "$SUB2API_REPO/deploy/Dockerfile" \
  -t "$SUB2API_IMAGE" \
  "$SUB2API_REPO"

echo "[2/3] 构建 resin（$PLATFORM）..."
# 记录 git 信息进镜像,这样线上能查出跑的是哪个版本 ——
# 二开分支尤其需要,否则分不清跑的是我们的版本还是上游的。
docker build --platform "$PLATFORM" \
  --build-arg VERSION="fork-$TS" \
  --build-arg GIT_COMMIT="$(git -C "$RESIN_REPO" rev-parse --short HEAD 2>/dev/null || echo unknown)" \
  --build-arg BUILD_TIME="$(date -Iseconds)" \
  -t "$RESIN_IMAGE" \
  "$RESIN_REPO"

echo "[3/3] 打包镜像 ..."
docker save "$SUB2API_IMAGE" "$RESIN_IMAGE" | gzip -1 > "$HERE/images-$TS.tar.gz"

echo
echo "完成: $HERE/images-$TS.tar.gz ($(du -h "$HERE/images-$TS.tar.gz" | cut -f1))"
echo
echo "搬到新机后:  docker load -i images-$TS.tar.gz"
