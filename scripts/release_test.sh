#!/usr/bin/env bash
#
# Tests scripts/release.sh on a scratch repository.
#
#   scripts/release_test.sh [--keep] [RELEASE_SH]
#
# RELEASE_SH is the script to test, scripts/release.sh by default.
#
# The scratch repository has the module layout of Behemoth: a root module,
# published adapter modules under storage/adapters/, plugins/adapters/ and
# telemetry/adapters/, and an unpublished tests/ module. Its remote is a bare
# repository on disk whose hook logs the refs of every push, so the test sees
# exactly what a release pushes. The modules depend on a module that the test
# serves from a proxy directory, so that they have go.sum files as Behemoth's
# do, and nothing is fetched from the network. Stubs stand in for govulncheck
# and apidiff. The apidiff stub prints the report a scenario sets in
# APIDIFF_REPORT, and logs the directory each side of a comparison loads in.
#
# The test walks through a release: the refusals before the version change is
# merged, the --prepare run on a branch, a rebase merge of its pull request,
# and the release run on dev. Then it checks the API comparison of later
# releases against the versioning policy, with --no-push and --prepare runs.
#
# It needs go and git on PATH. It works in a temporary directory, which it
# removes at the end unless the test failed or --keep is given.

set -euo pipefail

KEEP=0
SCRIPT=""
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    -*)     echo "release_test: unknown flag $arg" >&2; exit 2 ;;
    *)      SCRIPT=$arg ;;
  esac
done
SCRIPT=$(realpath "${SCRIPT:-$(dirname "$0")/release.sh}")
[ -f "$SCRIPT" ] || { echo "release_test: $SCRIPT does not exist" >&2; exit 2; }
command -v go >/dev/null || { echo "release_test: go is not on PATH" >&2; exit 2; }

T=$(mktemp -d)
FAIL=0
PASS=0

# The module cache is read-only, so it needs write permission back before it
# can be removed.
cleanup() {
  local status=$?
  if [ "$status" -eq 0 ] && [ "$KEEP" -eq 0 ]; then
    chmod -R u+w "$T" 2>/dev/null || true
    rm -rf "$T"
  else
    echo "The scratch repository and the logs of each run are in $T"
  fi
}
trap cleanup EXIT

# Nothing outside $T is read or written: not the module cache, not the
# network, and not the git configuration of the machine, whose signing or
# hooks would change what the script does.
mkdir -p "$T/bin"
export PATH=$T/bin:$PATH
export GOWORK=off GOFLAGS= GOTOOLCHAIN=local GOSUMDB=off GOPROXY=file://$T/proxy
export GOMODCACHE=$T/modcache GOCACHE=$T/gocache GOPATH=$T/gopath
export GIT_CONFIG_GLOBAL=$T/gitconfig GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
git config --global init.defaultBranch dev
git config --global advice.detachedHead false

printf '#!/bin/sh\necho "No vulnerabilities found."\n' > "$T/bin/govulncheck"
chmod +x "$T/bin/govulncheck"

# apidiff -m -w FILE PATH logs its directory and PATH, and apidiff -m OLD NEW
# prints APIDIFF_REPORT.
cat > "$T/bin/apidiff" <<EOF
#!/bin/sh
if [ "\$2" = -w ]; then
  echo "\$(pwd) \$4" >> "$T/apidiff.log"
  echo "\$4" > "\$3"
  exit 0
fi
printf '%s' "\$APIDIFF_REPORT"
EOF
chmod +x "$T/bin/apidiff"
export APIDIFF_REPORT=""
: > "$T/apidiff.log"

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

# run NAME ARG... runs the script in the scratch repository with ARGs, and
# keeps its output in NAME.log and its exit code in NAME.code. Its input is
# the file INPUT names, or nothing, so that a question nobody expects fails
# rather than waits.
run() {
  local name=$1 status=0
  shift
  (cd "$W" && scripts/release.sh "$@") < "${INPUT:-/dev/null}" > "$T/$name.log" 2>&1 || status=$?
  echo "$status" > "$T/$name.code"
}

# code NAME prints the exit code of run NAME.
code() { cat "$T/$1.code"; }

# says NAME TEXT succeeds when the output of run NAME contains TEXT.
says() { grep -qF -- "$2" "$T/$1.log"; }

# pushes prints the number of pushes the remote received.
pushes() { wc -l < "$T/pushes.log"; }

# --- the dependency ----------------------------------------------------------

mkdir -p "$T/dep"
printf 'module example.com/dep\n\ngo 1.26.0\n' > "$T/dep/go.mod"
printf 'package dep\n\nfunc Name() string { return "dep" }\n' > "$T/dep/dep.go"
git -C "$T/dep" init -q
git -C "$T/dep" add -A
git -C "$T/dep" commit -qm dep
v=$T/proxy/example.com/dep/@v
mkdir -p "$v"
echo v1.0.0 > "$v/list"
echo '{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}' > "$v/v1.0.0.info"
cp "$T/dep/go.mod" "$v/v1.0.0.mod"
git -C "$T/dep" archive --format=zip --prefix=example.com/dep@v1.0.0/ HEAD > "$v/v1.0.0.zip"

# --- the remote --------------------------------------------------------------

git init -q --bare "$T/remote.git"
cat > "$T/remote.git/hooks/post-receive" <<EOF
#!/bin/sh
echo "push: \$(awk '{print \$3}' | tr '\n' ' ')" >> "$T/pushes.log"
EOF
chmod +x "$T/remote.git/hooks/post-receive"

# --- the repository ----------------------------------------------------------

W=$T/work
SUBMODULES=(storage/adapters/a storage/adapters/b plugins/adapters/c telemetry/adapters/d)

mkdir -p "$W/scripts"
cp "$SCRIPT" "$W/scripts/release.sh"
cat > "$W/go.mod" <<'EOF'
module example.com/rel

go 1.26.0

require example.com/dep v1.0.0
EOF
printf 'package rel\n\nimport "example.com/dep"\n\nfunc Name() string { return dep.Name() }\n' > "$W/rel.go"
printf 'package rel\n\nimport "testing"\n\nfunc TestName(t *testing.T) {\n\tif Name() != "dep" {\n\t\tt.Fatal(Name())\n\t}\n}\n' > "$W/rel_test.go"
for m in "${SUBMODULES[@]}" tests; do
  mkdir -p "$W/$m"
  up=$(echo "$m" | sed 's#[^/]*#..#g')
  cat > "$W/$m/go.mod" <<EOF
module example.com/rel/$m

go 1.26.0

require example.com/rel v0.0.0

replace example.com/rel => $up
EOF
  printf 'package %s\n\nimport "example.com/rel"\n\nvar Name = rel.Name\n' "$(basename "$m")" > "$W/$m/$(basename "$m").go"
done
for m in . "${SUBMODULES[@]}" tests; do
  (cd "$W/$m" && go mod tidy)
done
printf '# Changelog\n\n## Unreleased\n\n## [0.2.0] - 2026-10-28\n\n### Added\n\n- Something.\n' > "$W/CHANGELOG.md"
git -C "$W" init -q
git -C "$W" add -A
git -C "$W" commit -qm initial
git -C "$W" remote add origin "$T/remote.git"
git -C "$W" push -q origin dev
: > "$T/pushes.log"

# --- the release -------------------------------------------------------------

echo "== --help"
run help --help
check "prints the usage with --prepare" 'says help "scripts/release.sh VERSION [--prepare]"'

echo "== a release run on dev before the version change is merged"
run early v0.2.0 --yes --skip-proxy
check "exits 1" '[ "$(code early)" = 1 ]'
check "names the missing version change" 'says early "dev does not have the version change yet"'
check "lists a changed go.mod" 'says early "storage/adapters/a/go.mod"'
check "stops before the tests" '! says early "Run the tests"'
check "restores the tree" '[ -z "$(git -C "$W" status --porcelain)" ]'
check "makes no tag" '[ -z "$(git -C "$W" tag -l)" ]'
check "pushes nothing" '[ "$(pushes)" = 0 ]'

echo "== --prepare on dev"
run prep-dev v0.2.0 --prepare
check "exits 1" '[ "$(code prep-dev)" = 1 ]'
check "asks for another branch" 'says prep-dev "run it on a branch made from dev"'

echo "== --prepare for a version without a changelog section"
git -C "$W" switch -q -c release-v0.2.0
run prep-nolog v0.9.0 --prepare
check "exits 1" '[ "$(code prep-nolog)" = 1 ]'
check "asks for the changelog section" 'says prep-nolog "has no \"## [0.9.0]\" section"'

echo "== --prepare on a branch"
base=$(git -C "$W" rev-parse HEAD)
run prep v0.2.0 --prepare
check "exits 0" '[ "$(code prep)" = 0 ]'
check "commits the version change on the branch" '[ "$(git -C "$W" log -1 --format=%s)" = "chore(release): v0.2.0" ] && [ "$(git -C "$W" rev-parse HEAD~1)" = "$base" ]'
check "sets every published submodule to v0.2.0" '[ "$(git -C "$W" grep -l "example.com/rel v0.2.0" -- "*/go.mod" | grep -vc "^tests/")" = ${#SUBMODULES[@]} ]'
check "skips the tests and govulncheck" '! says prep "Run the tests" && ! says prep "govulncheck"'
check "has no earlier release to compare the API with" 'says prep "No release before v0.2.0" && [ ! -s "$T/apidiff.log" ]'
check "makes no tag" '[ -z "$(git -C "$W" tag -l)" ]'
check "leaves a clean tree" '[ -z "$(git -C "$W" status --porcelain)" ]'
check "says what to do next" 'says prep "open a pull request into dev"'
check "pushes nothing" '[ "$(pushes)" = 0 ]'

echo "== --prepare again"
head=$(git -C "$W" rev-parse HEAD)
run prep-again v0.2.0 --prepare
check "exits 0" '[ "$(code prep-again)" = 0 ]'
check "says there is nothing to prepare" 'says prep-again "Nothing to prepare"'
check "makes no commit" '[ "$(git -C "$W" rev-parse HEAD)" = "$head" ]'

# A rebase merge, as GitHub makes it: the commits land on dev with new hashes.
git -C "$W" push -q origin release-v0.2.0
git clone -q "$T/remote.git" "$T/merger"
git -C "$T/merger" cherry-pick origin/dev..origin/release-v0.2.0 > /dev/null
git -C "$T/merger" push -q origin dev
git -C "$W" switch -q dev
git -C "$W" pull -q --ff-only origin dev
: > "$T/pushes.log"

echo "== the release run on dev"
head=$(git -C "$W" rev-parse HEAD)
echo y > "$T/yes"
INPUT=$T/yes run release v0.2.0 --skip-proxy
check "exits 0" '[ "$(code release)" = 0 ]'
check "asks before the push, with what the API comparison found" 'says release "About to push" && says release "API: first release, no API to compare with."'
check "runs govulncheck and the tests" 'says release "Run govulncheck" && says release "Run the tests of every module"'
check "makes no commit" '[ "$(git -C "$W" rev-parse HEAD)" = "$head" ] && says release "nothing to commit"'
check "tags HEAD for every published module" '[ "$(git -C "$W" tag --points-at HEAD | wc -l)" = $((${#SUBMODULES[@]} + 1)) ]'
check "makes two pushes" '[ "$(pushes)" = 2 ]'
check "pushes the submodule tags first, and no branch" '[ "$(sed -n 1p "$T/pushes.log")" = "push: refs/tags/plugins/adapters/c/v0.2.0 refs/tags/storage/adapters/a/v0.2.0 refs/tags/storage/adapters/b/v0.2.0 refs/tags/telemetry/adapters/d/v0.2.0 " ]'
check "pushes the root tag alone, last" '[ "$(sed -n 2p "$T/pushes.log")" = "push: refs/tags/v0.2.0 " ]'
check "leaves every tag on the remote, on dev" '[ "$(git -C "$T/remote.git" tag --points-at dev | wc -l)" = $((${#SUBMODULES[@]} + 1)) ]'

echo "== the release run again"
run again v0.2.0 --yes --skip-proxy
check "exits 1 on the existing tag" '[ "$(code again)" = 1 ] && says again "tag v0.2.0 already exists"'

echo "== --skip-tests on a run that pushes"
run skip v0.3.0 --skip-tests
check "is refused" '[ "$(code skip)" = 1 ] && says skip "--skip-tests needs --no-push"'

# --- API changes -------------------------------------------------------------

MODULES=$((${#SUBMODULES[@]} + 1))
sed -i 's/^## Unreleased$/## Unreleased\n\n## [0.2.1] - 2026-10-29/' "$W/CHANGELOG.md"
git -C "$W" commit -qam "docs(changelog): v0.2.1"

echo "== a patch release that changes the API"
export APIDIFF_REPORT=$'Compatible changes:\n- ./utils.Added: added\n'
head=$(git -C "$W" rev-parse HEAD)
run patch v0.2.1 --no-push --skip-tests --skip-vulncheck
check "exits 1" '[ "$(code patch)" = 1 ]'
check "compares with v0.2.0" 'says patch "Compare the API with v0.2.0"'
check "lists the change" 'says patch "compatible: ./utils.Added: added"'
check "asks for a minor release" 'says patch "is a patch release of v0.2.0, but the API changed" && says patch "release v0.3.0 instead"'
check "loads the old API in a worktree of v0.2.0, the new one in the repository" '[ "$(grep -c "/base" "$T/apidiff.log")" = $MODULES ] && [ "$(grep -c "^$W" "$T/apidiff.log")" = $MODULES ]'
check "removes the worktree" '[ "$(git -C "$W" worktree list | wc -l)" = 1 ]'
check "makes no commit and no tag" '[ "$(git -C "$W" rev-parse HEAD)" = "$head" ] && [ -z "$(git -C "$W" tag -l "*v0.2.1")" ]'

echo "== a patch release whose changes are all in internal packages"
export APIDIFF_REPORT=$'Incompatible changes:\n- package example.com/rel/internal/x: removed\nCompatible changes:\n- ./internal/y.Added: added\n'
run patch-internal v0.2.1 --no-push --skip-tests --skip-vulncheck
check "exits 0" '[ "$(code patch-internal)" = 0 ]'
check "finds no change in any module" '[ "$(grep -c ": no change$" "$T/patch-internal.log")" = $MODULES ]'
check "tags every published module" '[ "$(git -C "$W" tag -l "*v0.2.1" | wc -l)" = $MODULES ]'

echo "== a minor release before v1.0.0 with an incompatible change"
sed -i 's/^## Unreleased$/## Unreleased\n\n## [0.3.0] - 2026-10-29/' "$W/CHANGELOG.md"
git -C "$W" commit -qam "docs(changelog): v0.3.0"
export APIDIFF_REPORT=$'Incompatible changes:\n- Name: removed\n'
: > "$T/pushes.log"
run trial v0.3.0 --no-push --skip-tests --skip-vulncheck
check "exits 0" '[ "$(code trial)" = 0 ]'
check "compares with v0.2.1" 'says trial "Compare the API with v0.2.1"'
check "lists the change" 'says trial "incompatible: Name: removed"'
check "asks to check the changelog" 'says trial "Check that CHANGELOG.md lists these changes"'
check "commits the version change" '[ "$(git -C "$W" log -1 --format=%s)" = "chore(release): v0.3.0" ]'
check "tags every published module" '[ "$(git -C "$W" tag -l "*v0.3.0" | wc -l)" = $MODULES ]'
check "pushes nothing" '[ "$(pushes)" = 0 ]'

echo "== a minor release after v1.0.0 with an incompatible change"
git -C "$W" tag v1.0.0
git -C "$W" switch -q -c release-v1.1.0
sed -i 's/^## Unreleased$/## Unreleased\n\n## [1.1.0] - 2026-10-30/' "$W/CHANGELOG.md"
git -C "$W" commit -qam "docs(changelog): v1.1.0"
head=$(git -C "$W" rev-parse HEAD)
run v1-break v1.1.0 --prepare
check "exits 1" '[ "$(code v1-break)" = 1 ]'
check "compares with v1.0.0 in a --prepare run" 'says v1-break "Compare the API with v1.0.0"'
check "asks for a new major version" 'says v1-break "they need a new major version and module path"'
check "makes no commit" '[ "$(git -C "$W" rev-parse HEAD)" = "$head" ]'

echo "== a minor release after v1.0.0 with a compatible change"
export APIDIFF_REPORT=$'Compatible changes:\n- Added: added\n'
run v1-add v1.1.0 --prepare
check "exits 0" '[ "$(code v1-add)" = 0 ]'
check "commits the version change" '[ "$(git -C "$W" log -1 --format=%s)" = "chore(release): v1.1.0" ]'

echo
echo "passed $PASS, failed $FAIL"
[ "$FAIL" -eq 0 ]
