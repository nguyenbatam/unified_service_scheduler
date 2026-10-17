#!/usr/bin/env bash
# Repo checks (OpenAPI copies, migrations, shellcheck) + Go format/vet/golangci-lint/tidy/vulns +
# web typecheck + file size. Used by local check-all and GitHub Actions CI — keep them identical.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

RED=$'\033[31m'
GRN=$'\033[32m'
YLW=$'\033[33m'
NC=$'\033[0m'
step() { echo; echo "${YLW}==>${NC} $*"; }
ok() { echo "${GRN}OK${NC} $*"; }
fail() { echo "${RED}FAIL${NC} $*"; exit 1; }

command -v go >/dev/null || fail "go missing"
[[ "$(go version)" == *go1.27* ]] || echo "${YLW}warn${NC}: expected Go 1.27, got $(go version)"

step "OpenAPI copies identical (docs/ and the embedded backend/internal/httpapi/docs/)"
cmp docs/openapi.yaml backend/internal/httpapi/docs/openapi.yaml \
  || fail "docs/openapi.yaml and backend/internal/httpapi/docs/openapi.yaml differ — edit both identically"
ok "openapi copies match"

step "migrations immutable (local: vs merge-base with origin/main + uncommitted edits; CI: MIGRATIONS_BASE)"
bash scripts/check-migrations.sh || fail "existing migration changed — add the next NNN_name.sql instead"
ok "migrations"

step "shellcheck scripts/*.sh scripts/lib/*.sh (config: scripts/.shellcheckrc)"
if command -v shellcheck >/dev/null; then
  shellcheck scripts/*.sh scripts/lib/*.sh || fail "shellcheck findings above"
  ok "shellcheck"
elif [[ -n "${CI:-}" ]]; then
  fail "shellcheck missing on CI (ubuntu runners ship it; apt-get install shellcheck)"
else
  echo "${YLW}warn${NC}: shellcheck not installed, skipped (brew install shellcheck); CI runs it"
fi

step "python helpers compile (scripts/lib/*.py)"
# Parse only (ast): py_compile would write __pycache__ into the repo.
python3 -I -c 'import ast, sys
for f in sys.argv[1:]:
    with open(f) as src:
        ast.parse(src.read(), f)' scripts/lib/*.py || fail "python syntax errors above"
ok "python helpers"

step "gofmt (Go 1.27 alignment)"
(
  cd backend
  bad="$(gofmt -l .)"
  if [[ -n "$bad" ]]; then
    echo "$bad"
    fail "gofmt needed — run: ./scripts/fmt.sh"
  fi
)
ok "gofmt clean"

step "go vet (includes stdversion on go test)"
(cd backend && go vet ./...)
ok "go vet"

step "golangci-lint (see backend/.golangci.yml: gosec, staticcheck, bodyclose, rowserrcheck, noctx, contextcheck, …)"
(cd backend && go tool golangci-lint run ./...)
ok "golangci-lint"

step "go mod tidy drift"
# -diff (Go >= 1.23) prints what tidy would change and exits non-zero, without touching the files.
(
  cd backend
  if ! go mod tidy -diff; then
    fail "go.mod/go.sum not tidy — run: cd backend && go mod tidy"
  fi
)
ok "go.mod tidy"

step "govulncheck (go tool, pinned in backend/go.mod)"
(cd backend && go tool govulncheck ./...)
ok "govulncheck"

step "web tsc"
(
  cd web
  if [[ ! -d node_modules ]]; then
    if [[ -f package-lock.json ]]; then npm ci; else npm install; fi
  fi
  npx tsc --noEmit
)
ok "web tsc"

step "file size (max 1000 lines)"
MAX_LINES=1000
oversized="$(
  find backend web/src -type f \( -name '*.go' -o -name '*.ts' -o -name '*.tsx' \) \
    ! -path '*/node_modules/*' ! -path '*/dist/*' -print0 \
    | xargs -0 wc -l \
    | awk -v max="$MAX_LINES" '$2 != "total" && $1+0 > max { print $1, $2 }'
)"
if [[ -n "$oversized" ]]; then
  echo "$oversized"
  fail "files over ${MAX_LINES} lines — split before merge"
fi
ok "all source files ≤${MAX_LINES} lines"

echo
echo "${GRN}lint passed${NC}"
