#!/usr/bin/env bash
#
# Tests the report of scripts/coverage.sh on a scratch repository.
#
#   scripts/coverage_test.sh [--keep] [COVERAGE_SH]
#
# COVERAGE_SH is the script to test, scripts/coverage.sh by default.
#
# The scratch repository has the module layout of Behemoth: a root module, a
# published adapter module, and an unpublished tests/ module. The profiles
# are written by hand, so the test needs no Go: the report reads them with
# awk. It checks how profiles merge, which packages count, how modules and
# packages are reported, and each way the check of the targets fails.
#
# It works in a temporary directory, which it removes at the end unless the
# test failed or --keep is given.

set -euo pipefail

KEEP=0
SCRIPT=""
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    -*)     echo "coverage_test: unknown flag $arg" >&2; exit 2 ;;
    *)      SCRIPT=$arg ;;
  esac
done
SCRIPT=$(realpath "${SCRIPT:-$(dirname "$0")/coverage.sh}")
[ -f "$SCRIPT" ] || { echo "coverage_test: $SCRIPT does not exist" >&2; exit 2; }

T=$(mktemp -d)
FAIL=0
PASS=0

cleanup() {
  local status=$?
  if [ "$status" -eq 0 ] && [ "$KEEP" -eq 0 ]; then
    rm -rf "$T"
  else
    echo "The scratch repository and the output of each run are in $T"
  fi
}
trap cleanup EXIT

export GIT_CONFIG_GLOBAL=$T/gitconfig GIT_CONFIG_NOSYSTEM=1

# check DESCRIPTION CONDITION reports whether the shell condition holds.
check() {
  if eval "$2"; then
    echo "  ok   $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL $1"
    FAIL=$((FAIL + 1))
  fi
}

# run NAME ARG... runs report in the scratch repository with ARGs, and keeps
# its output in NAME.log and its exit code in NAME.code.
run() {
  local name=$1 status=0
  shift
  (cd "$W" && scripts/coverage.sh report "$@") > "$T/$name.log" 2>&1 || status=$?
  echo "$status" > "$T/$name.code"
}

# code NAME prints the exit code of run NAME.
code() { cat "$T/$1.code"; }

# says NAME TEXT succeeds when the output of run NAME contains TEXT.
says() { grep -qF -- "$2" "$T/$1.log"; }

# row NAME FIELD... succeeds when a line of the output of run NAME has
# exactly FIELDs, separated by any space.
row() {
  local name=$1
  shift
  grep -qE "^$*\$" <(sed -E 's/[[:space:]]+/ /g; s/ $//' "$T/$name.log")
}

# targets LINE... writes the targets file of the scratch repository.
targets() { printf '%s\n' "# targets" "$@" > "$W/scripts/coverage_targets.txt"; }

W=$T/repo
mkdir -p "$W/scripts" "$W/storage/adapters/db" "$W/tests"
cp "$SCRIPT" "$W/scripts/coverage.sh"
printf 'module example.com/m\n\ngo 1.26.0\n' > "$W/go.mod"
printf 'module example.com/m/storage/adapters/db\n\ngo 1.26.0\n' > "$W/storage/adapters/db/go.mod"
printf 'module example.com/m/tests\n\ngo 1.26.0\n' > "$W/tests/go.mod"
git -C "$W" init -q
git -C "$W" add .

# Two runs of the tests. The second covers the block of a that the first
# missed, and both miss b. tests/ is not published, so its package is left
# out.
cat > "$T/first.out" <<'EOF'
mode: atomic
example.com/m/a/a.go:1.1,2.1 3 1
example.com/m/a/a.go:3.1,4.1 1 0
example.com/m/b/b.go:1.1,2.1 2 0
example.com/m/storage/adapters/db/db.go:1.1,2.1 4 2
example.com/m/tests/helper/helper.go:1.1,2.1 5 1
EOF
cat > "$T/second.out" <<'EOF'
mode: atomic
example.com/m/a/a.go:3.1,4.1 1 7
example.com/m/b/b.go:1.1,2.1 2 0
example.com/m/storage/adapters/db/db.go:1.1,2.1 4 0
EOF

echo "== two profiles merged"
targets "a 100" "b 0" "storage/adapters/db 90"
run merge -o "$T/merged.out" --markdown "$T/summary.md" "$T/first.out" "$T/second.out"
check "exits 0" '[ "$(code merge)" = 0 ]'
check "a block either run covered counts as covered" 'row merge "a 4 100.0% 100%"'
check "a block no run covered counts as not covered" 'row merge "b 2 0.0% 0%"'
check "a package belongs to the module with the longest path" 'row merge "storage/adapters/db 4 100.0% 90%"'
check "the root module totals its packages" 'row merge ". 6 66.6%"'
check "the total covers every published package" 'row merge "total 10 80.0%"'
check "leaves out the packages of tests/" '! says merge "helper"'
check "the merged profile sums the counts" 'grep -qx "example.com/m/a/a.go:3.1,4.1 1 7" "$T/merged.out"'
check "the merged profile has each block once" '[ "$(grep -c "b/b.go" "$T/merged.out")" = 1 ]'
check "the merged profile leaves out tests/" '! grep -q "tests/helper" "$T/merged.out"'
check "the merged profile starts with its mode" '[ "$(head -1 "$T/merged.out")" = "mode: atomic" ]'
check "the summary has the package table in Markdown" 'grep -qF "| a | 4 | 100.0% | 100% | ok |" "$T/summary.md"'

echo "== a package below its target"
targets "a 100" "b 50" "storage/adapters/db 90"
run below "$T/first.out" "$T/second.out"
check "exits 1" '[ "$(code below)" = 1 ]'
check "names the package" 'row below "b 2 0.0% 50% below target"'
check "counts the failures" 'says below "1 package failed the coverage check."'

echo "== a package without a target"
targets "a 100" "storage/adapters/db 90"
run untargeted "$T/first.out" "$T/second.out"
check "exits 1" '[ "$(code untargeted)" = 1 ]'
check "names the package" 'row untargeted "b 2 0.0% - no target"'

echo "== a target without a package"
targets "a 100" "b 0" "c 80" "storage/adapters/db 90"
run stale "$T/first.out" "$T/second.out"
check "exits 1" '[ "$(code stale)" = 1 ]'
check "names the target" 'says stale "c has a target but no statements in the profiles"'

echo "== a coverage just under a target"
# 2000 of 2501 statements is 79.96%, which shows as 79.9%, not as 80.0%.
printf 'mode: atomic\nexample.com/m/c/c.go:1.1,2.1 2000 1\nexample.com/m/c/c.go:3.1,4.1 501 0\n' > "$T/close.out"
targets "c 80"
run close "$T/close.out"
check "exits 1" '[ "$(code close)" = 1 ]'
check "cuts the share instead of rounding it" 'row close "c 2501 79.9% 80% below target"'

echo "== a target that is not a whole percentage"
targets "a 99.5"
run fraction "$T/first.out"
check "exits 1" '[ "$(code fraction)" = 1 ]'
check "names the line" 'says fraction "a 99.5: the target is not a whole percentage"'

echo "== profiles that can't be merged"
targets "a 100" "b 0" "storage/adapters/db 90"
printf 'mode: set\nexample.com/m/a/a.go:1.1,2.1 3 1\n' > "$T/set.out"
run modes "$T/first.out" "$T/set.out"
check "two modes: exits 1" '[ "$(code modes)" = 1 ]'
check "two modes: names them" 'says modes "mode set, where the other profiles have atomic"'
printf 'mode: atomic\nexample.com/m/a/a.go:1.1,2.1 4 1\n' > "$T/other-release.out"
run blocks "$T/first.out" "$T/other-release.out"
check "a block of two sizes: exits 1" '[ "$(code blocks)" = 1 ]'
check "a block of two sizes: suspects two Go releases" 'says blocks "were the profiles made with different Go releases?"'
printf 'not a profile\n' > "$T/garbage.out"
run garbage "$T/garbage.out"
check "not a profile: exits 1" '[ "$(code garbage)" = 1 ]'

echo
echo "passed $PASS, failed $FAIL"
[ "$FAIL" -eq 0 ]
