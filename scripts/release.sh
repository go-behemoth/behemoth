#!/usr/bin/env bash
#
# Releases every published Behemoth module at one version.
#
#   scripts/release.sh VERSION [--prepare] [--no-push] [--skip-proxy]
#                              [--skip-tests] [--skip-vulncheck] [--yes]
#
# VERSION is a semantic version with a leading "v", such as v0.2.0.
#
# The published modules are the root module and every module under
# storage/adapters/, plugins/adapters/ and telemetry/adapters/. They are
# released in lockstep: each gets the same version. tests/ and examples/ are
# modules too, but they are never tagged. A go.mod anywhere else stops the
# script, so adding a module is a decision made here.
#
# A release takes two runs, because the release branch (dev while the major
# version is 0, main after) takes changes through pull requests only:
#
#   1. On a branch made from the release branch, with a CHANGELOG.md section
#      for VERSION committed, run the script with --prepare. It commits the
#      version change. Push the branch, open a pull request into the release
#      branch, and merge it once CI passes.
#   2. On the release branch, after pulling the merge, run the script without
#      --prepare. It checks the release, tags it, and pushes the tags.
#
# The script:
#
#   1. Checks the branch, a clean working tree, a branch that matches its
#      upstream, tags that do not exist yet, and a CHANGELOG.md section for
#      VERSION.
#   2. Sets the require line of every published module for another published
#      module to VERSION.
#   3. Runs go mod tidy in every module. A run that pushes stops here when
#      steps 2 and 3 changed a file: the version change has to be on the
#      release branch already.
#   4. Runs govulncheck on the published modules and the full test suite on
#      every module.
#   5. Commits the version change, if steps 2 and 3 made one.
#   6. Tags that commit: VERSION for the root module and DIR/VERSION for each
#      published submodule DIR.
#   7. Pushes the submodule tags in one atomic push, then the root tag on its
#      own. GitHub creates no push event when more than three tags arrive in
#      one push, and the root tag starts .github/workflows/release.yml.
#   8. Fetches every tagged module through proxy.golang.org, so that
#      pkg.go.dev indexes it.
#
# --prepare runs steps 1 to 3 and 5 on any branch but the release branch, and
# stops after the commit. It skips step 4: the pull request runs CI, and the
# second run checks again. --no-push stops after step 6 and leaves the commit
# and tags local. Use it to try the script on a scratch clone. --skip-tests
# and --skip-vulncheck skip step 4, and only work together with --no-push: a
# release is never pushed unchecked. --skip-proxy skips step 8. --yes skips
# the question before the push.
#
# scripts/release_test.sh tests this script on a scratch repository. Run it
# after a change here.
#
# Deferred: from the second release on, run gorelease (golang.org/x/exp/cmd/
# gorelease) on each published module in step 4 to report breaking changes.
# Deferred: print the advisory review checklist (docs/internal/security) before
# step 7 once the threat catalogue exists.

set -euo pipefail

# Directories whose go.mod files are published. The root module is published
# too and is handled apart, because its tag has no directory prefix.
PUBLISHED_GLOBS=(storage/adapters/* plugins/adapters/* telemetry/adapters/*)

# Directories whose go.mod files are never published or tagged.
UNPUBLISHED_GLOBS=(tests examples/*)

PROXY=https://proxy.golang.org

die() { echo "release: $*" >&2; exit 1; }
step() { printf '\n==> %s\n' "$*"; }

usage() {
  sed -n '3,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'
  exit 2
}

# --- arguments ---------------------------------------------------------------

VERSION=""
PREPARE=0
PUSH=1
PROXY_CHECK=1
RUN_TESTS=1
RUN_VULNCHECK=1
ASSUME_YES=0

for arg in "$@"; do
  case "$arg" in
    --prepare)        PREPARE=1 ;;
    --no-push)        PUSH=0 ;;
    --skip-proxy)     PROXY_CHECK=0 ;;
    --skip-tests)     RUN_TESTS=0 ;;
    --skip-vulncheck) RUN_VULNCHECK=0 ;;
    --yes)            ASSUME_YES=1 ;;
    -h|--help)        usage ;;
    -*)               die "unknown flag $arg (see --help)" ;;
    *)                [ -z "$VERSION" ] || die "more than one version given"
                      VERSION=$arg ;;
  esac
done

[ -n "$VERSION" ] || usage
[[ $VERSION =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]] \
  || die "$VERSION is not a version like v0.2.0 or v0.2.0-rc.1"
MAJOR=${BASH_REMATCH[1]}

# A --prepare run pushes nothing, and CI tests its pull request.
if [ "$PREPARE" -eq 1 ]; then
  PUSH=0
  RUN_TESTS=0
  RUN_VULNCHECK=0
fi

if [ "$PUSH" -eq 1 ]; then
  [ "$RUN_TESTS" -eq 1 ]      || die "--skip-tests needs --no-push: a release is never pushed untested"
  [ "$RUN_VULNCHECK" -eq 1 ]  || die "--skip-vulncheck needs --no-push: a release is never pushed unchecked"
fi

cd "$(git rev-parse --show-toplevel)"

# --- module list -------------------------------------------------------------

ROOT_PATH=$(cd . && go list -m)

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

# ALL holds every directory with a tracked go.mod. PUBLISHED holds the
# published ones, root first. A go.mod in neither group stops the script.
ALL=()
PUBLISHED=(.)
while IFS= read -r f; do
  d=$(dirname "$f")
  ALL+=("$d")
  if [ "$d" = . ]; then
    continue
  elif matches_any "$d" "${PUBLISHED_GLOBS[@]}"; then
    PUBLISHED+=("$d")
  elif matches_any "$d" "${UNPUBLISHED_GLOBS[@]}"; then
    :
  else
    die "$f is neither published nor unpublished: add its directory to PUBLISHED_GLOBS or UNPUBLISHED_GLOBS in $0"
  fi
done < <(git ls-files -- ':(glob)**/go.mod' | sort)

# mod_path DIR prints the module path declared in DIR/go.mod.
mod_path() { (cd "$1" && go list -m); }

# published_path DIR prints the module path a published directory must have.
published_path() {
  if [ "$1" = . ]; then echo "$ROOT_PATH"; else echo "$ROOT_PATH/$1"; fi
}

# A tag DIR/VERSION only resolves when the module path ends in DIR.
for d in "${PUBLISHED[@]}"; do
  got=$(mod_path "$d")
  want=$(published_path "$d")
  [ "$got" = "$want" ] || die "$d/go.mod declares module $got, but its tag only works for $want"
done

# --- step 1: checks ----------------------------------------------------------

step "Check the repository"

echo "Go toolchain: $(go version)"

BRANCH=$(git rev-parse --abbrev-ref HEAD)
if [ "$MAJOR" -eq 0 ]; then WANT_BRANCH=dev; else WANT_BRANCH=main; fi
if [ "$PREPARE" -eq 1 ]; then
  [ "$BRANCH" != "$WANT_BRANCH" ] \
    || die "--prepare makes a commit for a pull request into $WANT_BRANCH: run it on a branch made from $WANT_BRANCH"
else
  [ "$BRANCH" = "$WANT_BRANCH" ] || die "on branch $BRANCH: $VERSION is released from $WANT_BRANCH"
fi

[ -z "$(git status --porcelain)" ] || die "the working tree is not clean"

if [ "$PUSH" -eq 1 ]; then
  git fetch --quiet origin "$BRANCH"
  [ "$(git rev-parse HEAD)" = "$(git rev-parse "origin/$BRANCH")" ] \
    || die "$BRANCH differs from origin/$BRANCH: push or pull first"
fi

TAGS=("$VERSION")
for d in "${PUBLISHED[@]}"; do
  [ "$d" = . ] || TAGS+=("$d/$VERSION")
done

for t in "${TAGS[@]}"; do
  git rev-parse --quiet --verify "refs/tags/$t" >/dev/null && die "tag $t already exists"
done
if [ "$PUSH" -eq 1 ]; then
  existing=$(git ls-remote --tags origin "${TAGS[@]/#/refs/tags/}")
  [ -z "$existing" ] || die "tags already exist on origin:"$'\n'"$existing"
fi

grep -Eq "^## \[${VERSION#v}\]" CHANGELOG.md \
  || die "CHANGELOG.md has no \"## [${VERSION#v}]\" section: move the Unreleased notes under it"

# The require lines of a published module are bumped to VERSION, so a replace
# directive that names only v0.0.0 would stop applying. Every replace of a
# published module must therefore be unversioned.
for d in "${ALL[@]}"; do
  for p in "${PUBLISHED[@]}"; do
    path=$(published_path "$p")
    if grep -Eq "^[[:space:]]*(replace[[:space:]]+)?${path//./\\.}[[:space:]]+v[^[:space:]]*[[:space:]]+=>" "$d/go.mod"; then
      die "$d/go.mod replaces $path at a fixed version: drop the version from the replace directive"
    fi
  done
done

# From here on the script edits files. If it stops before the commit, put the
# tree back as it was: it was clean when the script started.
COMMITTED=0
cleanup() {
  status=$?
  if [ "$status" -ne 0 ] && [ "$COMMITTED" -eq 0 ] && [ -n "${EDITING:-}" ]; then
    echo "release: failed; restoring the working tree" >&2
    git checkout -- . || true
  fi
  exit "$status"
}
trap cleanup EXIT

# --- step 2: set the requires ------------------------------------------------

step "Set the Behemoth requires to $VERSION"
EDITING=1

for d in "${PUBLISHED[@]}"; do
  for p in "${PUBLISHED[@]}"; do
    [ "$d" != "$p" ] || continue
    path=$(published_path "$p")
    if grep -Eq "^[[:space:]]*(require[[:space:]]+)?${path//./\\.}[[:space:]]+v" "$d/go.mod"; then
      echo "$d: $path@$VERSION"
      (cd "$d" && go mod edit -require="$path@$VERSION")
    fi
  done
done

# --- step 3: tidy ------------------------------------------------------------

step "Run go mod tidy in every module"
for d in "${ALL[@]}"; do
  echo "$d"
  (cd "$d" && go mod tidy)
done

# The tree was clean at the start, so any change comes from steps 2 and 3. The
# release branch takes it through a pull request only, so a run that pushes
# stops before the tests rather than at a push the branch refuses.
CHANGED=$(git status --porcelain)
if [ -n "$CHANGED" ] && [ "$PUSH" -eq 1 ]; then
  die "$BRANCH does not have the version change yet. Steps 2 and 3 changed:"$'\n'"$CHANGED"$'\n'"Run the script with --prepare on a branch made from $BRANCH, merge its pull request, and run it here again"
fi
if [ -z "$CHANGED" ] && [ "$PREPARE" -eq 1 ]; then
  echo
  echo "Nothing to prepare: every require already names $VERSION, and every module is tidy."
  exit 0
fi

# --- step 4: check -----------------------------------------------------------

if [ "$RUN_VULNCHECK" -eq 1 ]; then
  step "Run govulncheck on the published modules"
  command -v govulncheck >/dev/null \
    || die "govulncheck is not installed: go install golang.org/x/vuln/cmd/govulncheck@latest"
  for d in "${PUBLISHED[@]}"; do
    echo "$d"
    (cd "$d" && govulncheck ./...)
  done
fi

if [ "$RUN_TESTS" -eq 1 ]; then
  step "Run the tests of every module"
  for d in "${ALL[@]}"; do
    echo "$d"
    # tests/ starts database containers and needs Docker.
    (cd "$d" && go build ./... && go vet ./... && go test -count=1 -timeout 25m ./...)
  done
fi

# --- step 5: commit ----------------------------------------------------------

step "Commit the version change"
git add -- ':(glob)**/go.mod' ':(glob)**/go.sum'
if git diff --cached --quiet; then
  echo "nothing to commit: the requires already name $VERSION"
else
  git commit --quiet -m "chore(release): $VERSION"
fi
COMMITTED=1

if [ "$PREPARE" -eq 1 ]; then
  echo
  echo "Stopped after the commit (--prepare). Push $BRANCH, open a pull request into $WANT_BRANCH,"
  echo "and once it is merged, run the script again on $WANT_BRANCH without --prepare."
  exit 0
fi

# --- step 6: tag -------------------------------------------------------------

step "Tag the commit"
for t in "${TAGS[@]}"; do
  git tag -a "$t" -m "$t"
  echo "$t"
done

if [ "$PUSH" -eq 0 ]; then
  echo
  echo "Stopped before the push (--no-push). The commit and ${#TAGS[@]} tags are local."
  exit 0
fi

# --- step 7: push ------------------------------------------------------------

if [ "$ASSUME_YES" -eq 0 ]; then
  echo
  echo "About to push ${#TAGS[@]} tags on $(git rev-parse --short HEAD), the head of origin/$BRANCH."
  echo "The Go proxy keeps a tag for good: a pushed tag cannot be taken back."
  read -r -p "Push? [y/N] " answer
  [ "$answer" = y ] || [ "$answer" = Y ] || die "not pushed; the tags stay local"
fi

# The commit is on origin already: step 3 stopped a run that would have made
# one. GitHub creates no push event for a push of more than three tags, so the
# root tag, which starts the release workflow, goes alone and last. The
# release then appears only once every submodule is tagged.
step "Push the tags"
refs=()
for t in "${TAGS[@]:1}"; do refs+=("refs/tags/$t"); done
if [ "${#refs[@]}" -gt 0 ]; then
  git push --atomic origin "${refs[@]}"
fi
git push origin "refs/tags/$VERSION" \
  || die "the submodule tags are pushed, but $VERSION is not: push it with: git push origin refs/tags/$VERSION"

# --- step 8: proxy -----------------------------------------------------------

if [ "$PROXY_CHECK" -eq 0 ]; then
  echo
  echo "Skipped the proxy fetch (--skip-proxy)."
  exit 0
fi

step "Fetch the tagged modules through $PROXY"
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

# The proxy fetches a new tag from the repository on the first request, which
# can fail for a moment after the push. Retry before giving up.
for d in "${PUBLISHED[@]}"; do
  path=$(published_path "$d")
  ok=0
  for attempt in 1 2 3 4 5 6; do
    if (cd "$scratch" && GOPROXY=$PROXY GOFLAGS= GOPRIVATE= GONOPROXY= \
          go list -m "$path@$VERSION" >/dev/null 2>&1); then
      ok=1
      break
    fi
    sleep 10
  done
  [ "$ok" -eq 1 ] || die "$PROXY did not serve $path@$VERSION: request it again later with: GOPROXY=$PROXY go list -m $path@$VERSION"
  echo "$path@$VERSION"
done

echo
echo "Released $VERSION: ${#TAGS[@]} modules."
