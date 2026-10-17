#!/usr/bin/env bash
# PreToolUse(Edit|Write): migrations are applied once by filename, so editing an
# existing one never reaches databases that already ran it. Ask before allowing.
set -euo pipefail
command -v jq >/dev/null || exit 0

file="$(jq -r '.tool_input.file_path // empty')"
root="${CLAUDE_PROJECT_DIR:-$PWD}"
rel="${file#"$root"/}"

case "$rel" in
  backend/internal/migrate/migrations/*.sql)
    if [[ -f "$file" ]]; then
      jq -n --arg r "$rel already exists; applied migrations are never re-run. Add the next NNN_*.sql instead unless this migration is unreleased." \
        '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "ask", permissionDecisionReason: $r}}'
    fi
    ;;
esac
exit 0
