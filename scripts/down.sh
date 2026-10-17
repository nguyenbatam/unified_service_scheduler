#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
# shellcheck source=ensure-env.sh
source "$ROOT/scripts/ensure-env.sh"
docker compose down
