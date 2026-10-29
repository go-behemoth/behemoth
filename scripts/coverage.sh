#!/usr/bin/env bash
#
# Measures the test coverage of the published Behemoth modules.
#
#   scripts/coverage.sh [--html FILE]
#   scripts/coverage.sh test DIR PROFILE [PACKAGE...]
#   scripts/coverage.sh report [-o FILE] [--markdown FILE] [--html FILE] PROFILE...
#
# Most of the root module is tested from tests/, a module of its own, and the
# storage adapters are tested there too, against real databases. A package's
# coverage therefore counts every statement that any test in the repository
# runs, whichever module the test is in.
#
# test runs the tests of the module in DIR, or only the PACKAGEs given, with
# coverage of every published package the tests link, and writes the profile
# to PROFILE. The tests of tests/ start database containers and need Docker.
# CI runs its tests through this command, so that a profile from CI and one
# from a local run measure the same thing. Extra go test flags, such as -race,
# go in GOFLAGS.
#
# report merges profiles: a block of statements counts as covered when a test
# in any of the profiles ran it. Packages of tests/ and examples/ are left
# out. It writes the merged profile to FILE (coverage.out by default), prints
# the coverage of each published module and package, and checks each package
# against its target in scripts/coverage_targets.txt. --markdown appends the
# same report in Markdown to a file, such as a CI job summary. --html writes
# go tool cover's annotated source view.
#
# The check fails when a package is below its target, when a package has no
# target, and when a target names a package that has no statements in the
# profiles. A new package therefore gets its target in the same change.
#
# Without a command, the script runs test for every module except examples/,
# which have no tests, and then report on the profiles.
#
# Profiles from different Go releases can't be merged: the blocks of a package
# can differ between them. Merge profiles from one release.
#
# docs/internal/testing/testing.md explains the measurement and the targets.
# scripts/coverage_test.sh tests the report on a scratch repository. Run it
# after a change here.

set -euo pipefail

# Directories whose go.mod files are published, as in scripts/release.sh. The
# root module is published too.
PUBLISHED_GLOBS=(storage/adapters/* plugins/adapters/* telemetry/adapters/*)

die() { echo "coverage: $*" >&2; exit 1; }

usage() {
  sed -n '3,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
  exit 2
}

REPO=$(cd "$(dirname "$0")/.." && pwd)
TARGETS=$REPO/scripts/coverage_targets.txt

# mod_path DIR prints the module path declared in DIR/go.mod.
mod_path() { awk '$1 == "module" { print $2; exit }' "$1/go.mod"; }

ROOT_PATH=$(mod_path "$REPO")

# matches_any DIR GLOB... succeeds when DIR matches one of the globs.
matches_any() {
  local dir=$1; shift
  local g
  for g in "$@"; do
    # shellcheck disable=SC2053
    [[ $dir == $g ]] && return 0
  done
  return 1
}

# MODULES holds the directory of every go.mod, PUBLISHED the published ones.
MODULES=()
PUBLISHED=()
while IFS= read -r f; do
  d=$(dirname "$f")
  MODULES+=("$d")
  if [ "$d" = . ] || matches_any "$d" "${PUBLISHED_GLOBS[@]}"; then
    PUBLISHED+=("$d")
  fi
done < <(cd "$REPO" && git ls-files -- go.mod ':(glob)**/go.mod' | sort)

# --- test ----------------------------------------------------------------------

cmd_test() {
  [ $# -ge 2 ] || usage
  local dir=$1 profile=$2
  shift 2
  [ -f "$dir/go.mod" ] || die "$dir has no go.mod"
  local packages=("$@")
  [ ${#packages[@]} -gt 0 ] || packages=(./...)
  mkdir -p "$(dirname "$profile")"
  profile=$(cd "$(dirname "$profile")" && pwd)/$(basename "$profile")

  # The pattern names packages of other modules too: those of the root module
  # in an adapter's tests, and those of every module in tests/. A test binary
  # only counts the packages it links. atomic is the mode -race needs, and
  # profiles merge only within one mode.
  (cd "$dir" && go test -count=1 -timeout 25m -covermode=atomic \
    -coverpkg="$ROOT_PATH/..." -coverprofile="$profile" "${packages[@]}")
}

# --- report --------------------------------------------------------------------

# merge PROFILE... writes the merged profile of the published packages to
# $OUT, and prints a line per package: its path, its module's directory, its
# statements, and the statements the tests ran.
merge() {
  local all="" published="" d
  for d in "${MODULES[@]}"; do all+="$(mod_path "$REPO/$d")=$d "; done
  for d in "${PUBLISHED[@]}"; do published+="$d "; done

  awk -v modules="$all" -v published="$published" -v out="$OUT" '
    BEGIN {
      n = split(modules, pairs, " ")
      for (i = 1; i <= n; i++) {
        eq = index(pairs[i], "=")
        mpath[i] = substr(pairs[i], 1, eq - 1)
        mdir[i] = substr(pairs[i], eq + 1)
      }
      split(published, p, " ")
      for (i in p) pub[p[i]] = 1
    }

    # module_dir PKG prints the directory of the module that holds PKG: the
    # one with the longest path that PKG is in.
    function module_dir(pkg,    i, best, len) {
      best = ""; len = 0
      for (i = 1; i <= n; i++) {
        if ((pkg == mpath[i] || index(pkg, mpath[i] "/") == 1) && length(mpath[i]) > len) {
          best = mdir[i]; len = length(mpath[i])
        }
      }
      return best
    }

    FNR == 1 {
      if ($1 != "mode:") { print FILENAME ": not a coverage profile" > "/dev/stderr"; failed = 1; exit 1 }
      if (mode == "") mode = $2
      else if ($2 != mode) { print FILENAME ": mode " $2 ", where the other profiles have " mode > "/dev/stderr"; failed = 1; exit 1 }
      next
    }

    {
      block = $1
      if (!(block in stmts)) {
        file = substr(block, 1, index(block, ":") - 1)
        if (!(file in fpkg)) {
          pkg = file
          sub(/\/[^\/]*$/, "", pkg)
          fpkg[file] = pkg
          fmod[file] = module_dir(pkg)
        }
        if (!(fmod[file] in pub)) next
        stmts[block] = $2
        bpkg[block] = fpkg[file]
        pmod[fpkg[file]] = fmod[file]
      } else if (stmts[block] != $2) {
        print FILENAME ": block " block " has " $2 " statements, where another profile has " stmts[block] ": were the profiles made with different Go releases?" > "/dev/stderr"
        failed = 1; exit 1
      }
      if (mode == "set") { if ($3 > 0) count[block] = 1; else count[block] += 0 }
      else count[block] += $3
    }

    END {
      if (failed) exit 1
      if (mode == "") { print "no profile has any block" > "/dev/stderr"; exit 1 }
      print "mode: " mode > out
      close(out)
      sort = "LC_ALL=C sort >> \"" out "\""
      for (b in stmts) {
        print b, stmts[b], count[b] | sort
        total[bpkg[b]] += stmts[b]
        if (count[b] > 0) covered[bpkg[b]] += stmts[b]
      }
      close(sort)
      for (pkg in total) print pkg, pmod[pkg], total[pkg], covered[pkg] + 0
    }
  ' "$@"
}

# report_tables FORMAT reads the lines of merge on stdin and prints the module
# and package tables, as text or markdown. It checks the targets and exits 1
# when one is missed.
report_tables() {
  awk -v format="$1" -v root="$ROOT_PATH" -v targets_file="$TARGETS" '
    # pct prints covered/total as a percentage, cut to one decimal place, so
    # that 79.96% does not show as 80.0% next to a target of 80.
    function pct(covered, total) {
      if (total == 0) return "-"
      return sprintf("%.1f%%", int(covered * 1000 / total) / 10)
    }
    function rel(pkg) {
      if (pkg == root) return "."
      return substr(pkg, length(root) + 2)
    }
    function row(a, b, c, d, e) {
      if (format == "markdown") {
        line = "| " a " | " b " | " c
        if (d != "" || e != "") line = line " | " d " | " e
        print line " |"
      } else {
        line = sprintf("%-36s %10s %9s %7s  %s", a, b, c, d, e)
        sub(/ +$/, "", line)
        print line
      }
    }

    BEGIN {
      while ((getline line < targets_file) > 0) {
        sub(/#.*/, "", line)
        if (split(line, f, " ") == 0) continue
        if (f[2] !~ /^[0-9]+$/ || f[2] > 100) { print targets_file ": " line ": the target is not a whole percentage" > "/dev/stderr"; bad = 1; exit 1 }
        target[f[1]] = f[2]
      }
      close(targets_file)
    }

    {
      pkg = rel($1)
      pkgs[++np] = pkg
      pmod[pkg] = $2; ptotal[pkg] = $3; pcov[pkg] = $4
      mtotal[$2] += $3; mcov[$2] += $4
      total += $3; cov += $4
    }

    END {
      if (bad) exit 1
      # Sort packages and modules by path: an insertion sort is enough for
      # a few dozen.
      for (i = 2; i <= np; i++) for (j = i; j > 1 && pkgs[j-1] > pkgs[j]; j--) { t = pkgs[j]; pkgs[j] = pkgs[j-1]; pkgs[j-1] = t }
      for (m in mtotal) mods[++nm] = m
      for (i = 2; i <= nm; i++) for (j = i; j > 1 && mods[j-1] > mods[j]; j--) { t = mods[j]; mods[j] = mods[j-1]; mods[j-1] = t }

      if (format == "markdown") { print "### Coverage by module"; print ""; print "| Module | Statements | Covered |"; print "| --- | ---: | ---: |" }
      else row("MODULE", "STATEMENTS", "COVERED")
      for (i = 1; i <= nm; i++) row(mods[i], mtotal[mods[i]], pct(mcov[mods[i]], mtotal[mods[i]]))
      row(format == "markdown" ? "**Total**" : "total", total, pct(cov, total))

      print ""
      if (format == "markdown") { print "### Coverage by package"; print ""; print "| Package | Statements | Covered | Target | Check |"; print "| --- | ---: | ---: | ---: | --- |" }
      else row("PACKAGE", "STATEMENTS", "COVERED", "TARGET")
      for (i = 1; i <= np; i++) {
        p = pkgs[i]
        check = ""
        if (!(p in target)) { check = "no target"; failures++ }
        else if (pcov[p] * 100 < target[p] * ptotal[p]) { check = "below target"; failures++ }
        else if (format == "markdown") check = "ok"
        row(p, ptotal[p], pct(pcov[p], ptotal[p]), (p in target) ? target[p] "%" : "-", check)
      }

      for (p in target) if (!(p in ptotal)) {
        print ""
        print targets_file ": " p " has a target but no statements in the profiles: remove its line, or check that the profiles cover every module"
        failures++
      }
      if (failures) {
        print ""
        print failures " package" (failures > 1 ? "s" : "") " failed the coverage check."
        exit 1
      }
    }
  '
}

cmd_report() {
  OUT=coverage.out
  local markdown="" html=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -o)         OUT=$2; shift 2 ;;
      --markdown) markdown=$2; shift 2 ;;
      --html)     html=$2; shift 2 ;;
      -h|--help)  usage ;;
      -*)         die "unknown flag $1 (see --help)" ;;
      *)          break ;;
    esac
  done
  [ $# -gt 0 ] || die "report needs at least one profile"
  [ -f "$TARGETS" ] || die "$TARGETS does not exist"

  local stats status=0
  stats=$(merge "$@") || die "could not merge the profiles"

  echo "Coverage of the published modules by every test in the profiles"
  echo
  report_tables text <<<"$stats" || status=1
  if [ -n "$markdown" ]; then
    {
      echo "## Test coverage"
      echo
      report_tables markdown <<<"$stats" || true
      echo
    } >>"$markdown"
  fi

  if [ -n "$html" ]; then
    # go tool cover finds a package's source through the go command, and no
    # single module requires all the others. A workspace of every module
    # does, kept outside the repository.
    local work
    work=$(mktemp -d)
    {
      echo "go $(awk '$1 == "go" { print $2; exit }' "$REPO/go.mod")"
      local d
      for d in "${MODULES[@]}"; do echo "use $REPO/$d"; done
    } >"$work/go.work"
    GOWORK=$work/go.work go tool cover -html="$OUT" -o "$html" || status=1
    rm -rf "$work"
  fi
  return $status
}

# --- everything ----------------------------------------------------------------

cmd_all() {
  local html=()
  while [ $# -gt 0 ]; do
    case "$1" in
      --html)    html=(--html "$2"); shift 2 ;;
      -h|--help) usage ;;
      *)         die "unknown argument $1 (see --help)" ;;
    esac
  done

  local profiles=() d
  # Global, so that the trap still finds it after the function returns.
  PROFILES_DIR=$(mktemp -d)
  trap 'rm -rf "$PROFILES_DIR"' EXIT
  for d in "${MODULES[@]}"; do
    [[ $d == examples/* ]] && continue
    echo "==> $d"
    cmd_test "$REPO/$d" "$PROFILES_DIR/${d//\//_}.out"
    profiles+=("$PROFILES_DIR/${d//\//_}.out")
  done
  echo
  cmd_report -o "$REPO/coverage.out" "${html[@]}" "${profiles[@]}"
}

case "${1:-}" in
  test)      shift; cmd_test "$@" ;;
  report)    shift; cmd_report "$@" ;;
  -h|--help) usage ;;
  *)         cmd_all "$@" ;;
esac
