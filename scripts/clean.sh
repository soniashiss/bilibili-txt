#!/usr/bin/env bash
# scripts/clean.sh — 清理构建产物与本地缓存
#
# 只清 build/cache 相关目录，不动 runtime 目录（logs/ / transcripts/）——
# 后者由用户自主管理，脚本不做隐式删除。

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

rm -rf bin .tmp
echo "cleaned bin/ .tmp/"
