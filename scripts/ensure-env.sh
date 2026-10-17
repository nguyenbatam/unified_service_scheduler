# shellcheck shell=bash
# Ensure .env exists and load it (exported). Source it from bash (3.2+) or zsh:
#   source scripts/ensure-env.sh                      # interactive shell, either bash or zsh
#   ROOT="$(cd "$(dirname "$0")/.." && pwd)"          # from another script
#   # shellcheck source=ensure-env.sh
#   source "$ROOT/scripts/ensure-env.sh"
# Like docker compose, a variable the caller already set wins over .env: only the .env lines
# naming an unset variable are applied (so `GATEWAY_PORT=9000 ./scripts/check-all.sh` works).
# bash names the sourced file in BASH_SOURCE; zsh (FUNCTION_ARGZERO, on by default) puts it in $0.
_ENV_SRC="${BASH_SOURCE[0]:-$0}"
_ROOT="$(cd "$(dirname "$_ENV_SRC")/.." && pwd)"
if [[ ! -f "$_ROOT/.env" ]]; then
  cp "$_ROOT/.env.example" "$_ROOT/.env"
  echo "created .env from .env.example"
fi
# The example's JWT_SECRET is a placeholder (prefix dev-only-change-me) the server refuses: give
# this checkout its own random secret, in .env only, rewriting just that line (a missing line is
# appended). Temp file + mv, not `sed -i` (BSD and GNU sed disagree on it).
_ENV_RE="^[[:space:]]*(export[[:space:]]+)?JWT_SECRET=[\"']?dev-only-change-me"
if grep -Eq "$_ENV_RE" "$_ROOT/.env" || ! grep -Eq '^[[:space:]]*(export[[:space:]]+)?JWT_SECRET=' "$_ROOT/.env"; then
  _ENV_SECRET="$(openssl rand -hex 32 2>/dev/null)" || _ENV_SECRET=""
  if [[ ${#_ENV_SECRET} -ne 64 ]]; then
    _ENV_SECRET="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  fi
  if [[ ${#_ENV_SECRET} -ne 64 ]]; then
    echo "ensure-env: could not generate a JWT_SECRET (no openssl or /dev/urandom)" >&2
    unset _ROOT _ENV_SRC _ENV_RE _ENV_SECRET
    return 1
  fi
  _ENV_TMP="$(mktemp "$_ROOT/.env.XXXXXX")"
  if awk -v re="$_ENV_RE" -v secret="$_ENV_SECRET" '
    $0 ~ re { match($0, /JWT_SECRET=/); print substr($0, 1, RSTART + RLENGTH - 1) secret; done = 1; next }
    /^[[:space:]]*(export[[:space:]]+)?JWT_SECRET=/ { done = 1 }
    { print }
    END { if (!done) print "JWT_SECRET=" secret }' "$_ROOT/.env" >"$_ENV_TMP" && cat "$_ENV_TMP" >"$_ROOT/.env"; then
    # Written through (not mv'd over) so .env keeps its mode and stays a symlink if it is one.
    rm -f "$_ENV_TMP"
    echo "ensure-env: wrote a random JWT_SECRET to .env (it was the dev-only placeholder or missing)"
  else
    rm -f "$_ENV_TMP"
    echo "ensure-env: could not rewrite JWT_SECRET in .env" >&2
    unset _ROOT _ENV_SRC _ENV_RE _ENV_SECRET _ENV_TMP
    return 1
  fi
fi
unset _ENV_RE _ENV_SECRET _ENV_TMP
# Names .env assigns that the caller already set (even to ""): those lines are skipped, and the
# caller's value is exported so child processes (compose, go, python) see the same one. A value
# this script itself loaded earlier (remembered in _ENVLOADED_<NAME>) and still unchanged is not
# the caller's: it is reloaded, so an edit to .env applies when an interactive shell re-sources.
_ENV_KEEP=""
# shellcheck disable=SC2013 # one variable name per line, no spaces
for _ENV_NAME in $(sed -nE 's/^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)=.*/\2/p' "$_ROOT/.env"); do
  if eval "[ -n \"\${${_ENV_NAME}+x}\" ]"; then
    if eval "[ -n \"\${_ENVLOADED_${_ENV_NAME}+x}\" ] && [ \"\${_ENVLOADED_${_ENV_NAME}}\" = \"\${${_ENV_NAME}}\" ]"; then
      continue
    fi
    _ENV_KEEP="$_ENV_KEEP $_ENV_NAME"
    eval "export ${_ENV_NAME}"
  fi
done
set -a
eval "$(awk -v keep="$_ENV_KEEP" '
  BEGIN { n = split(keep, k, " "); for (i = 1; i <= n; i++) skip[k[i]] = 1 }
  {
    line = $0
    sub(/^[ \t]*export[ \t]+/, "", line)
    if (match(line, /^[A-Za-z_][A-Za-z0-9_]*=/) && (substr(line, 1, RLENGTH - 1) in skip)) next
    print
  }' "$_ROOT/.env")"
set +a
# Remember what was loaded from .env (not the caller's kept values) for the next source.
# shellcheck disable=SC2013
for _ENV_NAME in $(sed -nE 's/^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)=.*/\2/p' "$_ROOT/.env"); do
  case " $_ENV_KEEP " in *" $_ENV_NAME "*) continue ;; esac
  eval "export _ENVLOADED_${_ENV_NAME}=\"\${${_ENV_NAME}}\""
done
case "${JWT_SECRET:-}" in
  dev-only-change-me*) echo "ensure-env: warning: JWT_SECRET set in your environment is the dev-only placeholder; the server refuses it (unset it to use .env's)" >&2 ;;
esac
unset _ROOT _ENV_SRC _ENV_KEEP _ENV_NAME
