#!/usr/bin/env bash
# Auto-format Go (gofmt Go 1.27) + golangci formatters.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT/backend"
gofmt -w .
go tool golangci-lint fmt ./...
echo "formatted backend"
