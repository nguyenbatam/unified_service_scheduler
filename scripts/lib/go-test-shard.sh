#!/usr/bin/env bash
# Prints the `go test -run` pattern of shard INDEX of TOTAL for the Go package in DIR: its
# top-level tests (`func TestXxx(t *testing.T) {` in DIR's _test.go files), sorted by name and dealt
# round-robin, so every test runs in exactly one shard and the slow families (TestSession…,
# TestHTTP…) spread over all of them. Subtests follow their parent. Fails when a `func Test…` line
# has another shape (it would silently run in no shard) or the shard comes out empty.
# Usage: scripts/lib/go-test-shard.sh DIR INDEX TOTAL   (1 ≤ INDEX ≤ TOTAL)
set -euo pipefail

fail() { echo "go-test-shard: $*" >&2; exit 1; }

[[ $# -eq 3 ]] || fail "usage: $0 DIR INDEX TOTAL"
dir=$1 index=$2 total=$3
if ! [[ $index =~ ^[1-9][0-9]*$ && $total =~ ^[1-9][0-9]*$ ]] || ((index > total)); then
  fail "want 1 ≤ INDEX ≤ TOTAL, got INDEX=$index TOTAL=$total"
fi
compgen -G "$dir/*_test.go" >/dev/null || fail "no _test.go files in $dir"

# TestMain runs the binary; it is not a test to deal out.
declared=$(grep -h '^func Test' "$dir"/*_test.go | grep -v '^func TestMain(' || true)
# Byte order (LC_ALL=C): every shard runs on a runner of its own, and they must all deal the same list.
names=$(sed -nE 's/^func (Test[A-Z0-9_][A-Za-z0-9_]*)\(t \*testing\.T\) \{$/\1/p' <<<"$declared" | LC_ALL=C sort)
count() {
  if [[ -z $1 ]]; then echo 0; else wc -l <<<"$1" | tr -d ' '; fi
}
if [[ $(count "$declared") -ne $(count "$names") ]]; then
  echo "$declared" | grep -vE '^func Test[A-Z0-9_][A-Za-z0-9_]*\(t \*testing\.T\) \{$' >&2 || true
  fail "the lines above declare tests in another shape: write them as func TestXxx(t *testing.T) {"
fi

shard=$(awk -v i="$index" -v n="$total" '(NR - 1) % n == i - 1' <<<"$names")
[[ -n $shard ]] || fail "shard $index of $total is empty ($(count "$names") tests in $dir)"
printf '^(%s)$\n' "$(paste -sd'|' - <<<"$shard")"
