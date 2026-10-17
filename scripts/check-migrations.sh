#!/usr/bin/env bash
# Applied migrations are immutable: the runner applies each file once by name, so an edit, delete
# or rename never reaches a database that already ran it. Fails when any migration that exists at
# the base was modified, deleted or renamed since; new files (the next number) are fine.
# Usage: ./scripts/check-migrations.sh [base]
#   base: a commit or ref to compare against (CI passes the PR base, or the push's previous
#   commit; lint.sh passes nothing and reads MIGRATIONS_BASE). The diff runs from
#   merge-base(base, HEAD) to the working tree, so uncommitted edits count too. With
#   MIGRATIONS_DIRECT=1 (CI on a push: base is the push's "before") base itself is diffed to the
#   working tree as well: after a force push the merge-base is older than "before", and a
#   migration pushed (and maybe applied) in between and since edited or dropped must still fail. Without one:
#   origin/main (or origin/master) — the branch's whole divergence from main, pushed or not —, else
#   the branch's upstream, else HEAD (uncommitted edits only). A base missing from the clone
#   (shallow fetch, force push) is fetched from origin first; on CI (CI set) a base still missing,
#   or without common history, fails — except with MIGRATIONS_DIRECT=1, where a push that rewrote
#   the whole history (a squash into a new root) is judged by the direct diff alone. Locally, and for a new branch's all-zero "before" or a
#   repository with no commits yet, it is a notice and a pass.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
DIR=backend/internal/migrate/migrations

skip() { echo "check-migrations: $* — skipped"; exit 0; }
# unusable: no base to diff against. CI must never pass on that (it would check nothing).
unusable() {
  if [[ -n "${CI:-}" ]]; then
    echo "FAIL check-migrations: $* (CI: the base must be diffable)"
    exit 1
  fi
  skip "$*"
}

git rev-parse --is-inside-work-tree >/dev/null 2>&1 || skip "not a git work tree"
git rev-parse -q --verify 'HEAD^{commit}' >/dev/null || skip "no commits yet"

is_commit() { [[ -n "$1" ]] && git rev-parse -q --verify "$1^{commit}" >/dev/null 2>&1; }

# fetch_base makes $1 and its history available: a shallow clone is deepened to full history
# (merge-base needs it), and a commit still absent is fetched by name.
fetch_base() {
  git remote get-url origin >/dev/null 2>&1 || return 0
  if [[ "$(git rev-parse --is-shallow-repository 2>/dev/null)" == true ]]; then
    GIT_TERMINAL_PROMPT=0 git fetch -q --no-tags --unshallow origin || true
  fi
  is_commit "$1" || GIT_TERMINAL_PROMPT=0 git fetch -q --no-tags origin "$1" || true
}

base="${1:-${MIGRATIONS_BASE:-}}"
if [[ -n "$base" ]]; then
  # A new branch's first push reports an all-zero "before".
  [[ "$base" =~ ^0+$ ]] && skip "no previous commit (new branch)"
  is_commit "$base" || fetch_base "$base"
  is_commit "$base" || unusable "base $base is not in this clone, nor fetchable from origin"
else
  for candidate in origin/main origin/master '@{upstream}' HEAD; do
    if is_commit "$candidate"; then base="$candidate"; break; fi
  done
fi

if ! mb="$(git merge-base "$base" HEAD 2>/dev/null)"; then
  # A shallow clone may hold both commits but not the history joining them.
  fetch_base "$base"
  if ! mb="$(git merge-base "$base" HEAD 2>/dev/null)"; then
    # A push whose "before" shares no history with it rewrote the branch from a new root: the
    # direct diff below still compares every migration "before" held (and maybe applied).
    [[ "${MIGRATIONS_DIRECT:-}" == 1 ]] || unusable "no common history between $base and HEAD"
    mb=""
  fi
fi
# -M: a rename is reported as R (old -> new), not as an add plus a delete the filter would miss.
changed=""
[[ -z "$mb" ]] || changed="$(git diff -M --diff-filter=MDR --name-status "$mb" -- "$DIR")"
if [[ "${MIGRATIONS_DIRECT:-}" == 1 && "$(git rev-parse "$base^{commit}")" != "$mb" ]]; then
  # Not for a PR base: diffed directly, migrations added on main since the branch point would show
  # as deleted here. A push's "before" is (or was, before a force push) this branch itself.
  direct="$(git diff -M --diff-filter=MDR --name-status "$base" -- "$DIR")"
  changed="$(printf '%s\n%s\n' "$changed" "$direct" | sed '/^$/d' | sort -u)"
fi
if [[ -n "$changed" ]]; then
  echo "$changed"
  echo "FAIL existing migrations under $DIR were modified, deleted or renamed (vs $base @ ${mb:0:12}${mb:-direct diff})."
  echo "     Never edit an applied migration: revert it and add the next NNN_name.sql instead."
  exit 1
fi
echo "migrations ok: no existing file under $DIR changed since $base (${mb:0:12}${mb:-no common history, direct diff})"
