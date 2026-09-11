#!/usr/bin/env bash
# scripts/test.sh — 全量单测 + 集成测（不依赖 yt-dlp/ffmpeg/whisper-cli 真工具）
#
# -race:    竞态检测；对齐 docs/tasks.md 反复要求的 SOP
# -count=1: 绕过 test cache，强制重跑
#
# 追加参数会透传给 `go test`，例如：
#   ./scripts/test.sh -v -run TestSubtitleBranch ./test/integration/...

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

if [ "$#" -eq 0 ]; then
  set -- ./...
fi

exec go test "$@" -race -count=1
