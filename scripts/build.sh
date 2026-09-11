#!/usr/bin/env bash
# scripts/build.sh — bilibili-txt 正式构建入口
#
# 只负责一件事：编出可发布的 bin/bilibili-txt，并把版本号注入到
# `main.version`。开发期日常构建可直接 `go build ./cmd/bilibili-txt`，
# 无需走这里；本脚本是**制品契约**层。
#
# 用法：
#   ./scripts/build.sh                 # VERSION 走 git describe，回落 0.0.0-dev
#   VERSION=v1.2.3 ./scripts/build.sh  # 手动覆盖（发布 / 无 git 元数据场景）
#   EXTRA_LDFLAGS='-w -s' ./scripts/build.sh   # 追加链接器旗标，opt-in

set -euo pipefail

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)}"
EXTRA_LDFLAGS="${EXTRA_LDFLAGS:-}"

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="${REPO_ROOT}/bin"
BIN="${BIN_DIR}/bilibili-txt"
CMD_PKG="./cmd/bilibili-txt"

mkdir -p "${BIN_DIR}"

cd "${REPO_ROOT}"
go build \
  -trimpath \
  -mod=readonly \
  -ldflags "-X 'main.version=${VERSION}' ${EXTRA_LDFLAGS}" \
  -o "${BIN}" \
  "${CMD_PKG}"

echo "built ${BIN} (version=${VERSION})"
