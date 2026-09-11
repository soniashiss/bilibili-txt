#!/usr/bin/env bash
# scripts/check.sh — 交付前一键体检：gofmt-clean + go vet + go test
#
# 任一步骤 fail 立即退出（依赖 set -e）。等价于原 Makefile 的 `make check`。

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

echo "==> gofmt -l ."
fmt_out="$(gofmt -l . 2>&1)"
if [ -n "${fmt_out}" ]; then
  echo "gofmt has diff:"
  echo "${fmt_out}"
  exit 1
fi

echo "==> go vet ./..."
go vet ./...

echo "==> go test ./... -race -count=1"
go test ./... -race -count=1

echo "check ok"
