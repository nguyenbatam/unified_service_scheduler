#!/usr/bin/env bash
# PostToolUse(Edit|Write): gofmt backend Go, keep both OpenAPI copies identical,
# and report files over the 1000-line cap back to Claude (exit 2 → stderr shown).
set -euo pipefail
command -v jq >/dev/null || exit 0

file="$(jq -r '.tool_input.file_path // empty')"
[[ -n "$file" && -f "$file" ]] || exit 0
root="${CLAUDE_PROJECT_DIR:-$PWD}"
rel="${file#"$root"/}"

case "$rel" in
  backend/*.go) gofmt -w "$file" 2>/dev/null || true ;;
esac

case "$rel" in
  docs/openapi.yaml) cp "$file" "$root/backend/internal/httpapi/docs/openapi.yaml" ;;
  backend/internal/httpapi/docs/openapi.yaml) cp "$file" "$root/docs/openapi.yaml" ;;
esac

case "$rel" in
  */node_modules/* | */dist/*) ;;
  *.go | *.ts | *.tsx)
    lines="$(wc -l <"$file" | tr -d ' ')"
    if ((lines > 1000)); then
      echo "$rel has $lines lines (max 1000, see AGENTS.md). Split it by responsibility now." >&2
      exit 2
    fi
    ;;
esac
exit 0
