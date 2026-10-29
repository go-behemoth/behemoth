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
# Replace directives: a module that requires another published module
# replaces it with its directory in this repository, without a version. The
# replace is what makes a release one commit. A submodule tagged VERSION
# requires the core module at VERSION, which doesn't exist until the tags are
# pushed, and without the replace go mod tidy and go build fail on it, in a
# go.work workspace too. It also lets one pull request change the core module
# and an adapter together, with CI building the adapter against the core of
# the same commit. Go ignores replace directives in a dependency's go.mod, so
# users get the versions the modules require. One constraint follows: go
# install PKG@VERSION refuses a module whose go.mod has replace directives, so
# a command belongs in the root module, which has none.
#
# Keep the replaces while the published modules are released in lockstep from
# this repository. A module that gets its own release cycle, or its own
# repository, drops them: it requires a released core version, so that CI
# tests what users get, and work across modules moves to a go.work file. A
# change to the core and that module is then released in two steps, and this
# script needs a version per module.
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
#      VERSION. Then compares the API of every published module with the
#      previous release (see "API changes" below).
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
# API changes: the previous release is the highest vX.Y.Z tag below VERSION.
# Step 1 checks it out in a worktree and compares the exported API of each
# published module there with the API in the working tree, using apidiff
# (golang.org/x/exp/cmd/apidiff). Internal packages are left out. The script
# follows the versioning policy in CHANGELOG.md: it stops when a patch release
# changes the API, and from v1.0.0 on, when a minor release changes it
# incompatibly. Other changes are listed, to be checked against CHANGELOG.md.
# The check runs with --prepare too, so a wrong version is found before the
# pull request.
#
# gorelease (golang.org/x/exp/cmd/gorelease) makes this comparison for one
# module, but it loads the module the way a dependency is loaded, without its
# replace directives. A submodule then needs the core module at VERSION from
# the proxy, which has it only after the release. apidiff is the comparison
# gorelease is built on, and both of its sides load here with the replaces.
#
# scripts/release_test.sh tests this script on a scratch repository. Run it
# after a change here.
#
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
MINOR=${BASH_REMATCH[2]}
CORE=v$MAJOR.$MINOR.${BASH_REMATCH[3]} # VERSION without a pre-release suffix

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

# --- cleanup -----------------------------------------------------------------

# TMP holds the worktree of the previous release and other scratch files.
TMP=$(mktemp -d)

# If the script stops after it has edited files and before the commit, put the
# tree back as it was: it was clean when the script started. EDITING is set
# when the edits start.
COMMITTED=0
cleanup() {
  status=$?
  if [ "$status" -ne 0 ] && [ "$COMMITTED" -eq 0 ] && [ -n "${EDITING:-}" ]; then
    echo "release: failed; restoring the working tree" >&2
    git checkout -- . || true
  fi
  git worktree remove --force "$TMP/base" 2>/dev/null || true
  rm -rf "$TMP"
  exit "$status"
}
trap cleanup EXIT

# api_changes reads a report of apidiff and prints one line per change, as
# "incompatible: CHANGE" or "compatible: CHANGE". A change names its package
# as "package PATH" or as a path relative to the module, such as
# "./utils.Name". Changes in internal packages are left out: no other module
# can import them.
api_changes() {
  awk '
    /^Incompatible changes:$/ { kind = "incompatible"; next }
    /^Compatible changes:$/   { kind = "compatible"; next }
    /^- / {
      what = ($2 == "package") ? $3 : $2
      if (what ~ /(^|\/)internal([\/.:]|$)/) next
      print kind ": " substr($0, 3)
    }'
}

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
# published module must therefore be unversioned. See "Replace directives" at
# the top for why the replaces exist.
for d in "${ALL[@]}"; do
  for p in "${PUBLISHED[@]}"; do
    path=$(published_path "$p")
    if grep -Eq "^[[:space:]]*(replace[[:space:]]+)?${path//./\\.}[[:space:]]+v[^[:space:]]*[[:space:]]+=>" "$d/go.mod"; then
      die "$d/go.mod replaces $path at a fixed version: drop the version from the replace directive"
    fi
  done
done

# Release candidates are left out of the previous release: a release is
# compared with the release before it, whatever candidates came between.
PREV=$({ git tag -l 'v*'; echo "$CORE"; } \
  | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -uV \
  | grep -x -B1 -- "$CORE" | grep -vx -- "$CORE" || true)

if [ -z "$PREV" ]; then
  API_SUMMARY="first release, no API to compare with"
  echo "No release before $VERSION: no API to compare with."
else
  step "Compare the API with $PREV"
  command -v apidiff >/dev/null \
    || die "apidiff is not installed: go install golang.org/x/exp/cmd/apidiff@latest"
  git worktree add --quiet --detach "$TMP/base" "$PREV"

  API_CHANGES=0
  API_BREAKS=0
  for d in "${PUBLISHED[@]}"; do
    path=$(published_path "$d")
    if [ ! -f "$TMP/base/$d/go.mod" ] || [ "$(mod_path "$TMP/base/$d")" != "$path" ]; then
      echo "$d: not in $PREV as $path, nothing to compare"
      continue
    fi
    (cd "$TMP/base/$d" && apidiff -m -w "$TMP/old.export" "$path")
    (cd "$d" && apidiff -m -w "$TMP/new.export" "$path")
    changes=$(apidiff -m "$TMP/old.export" "$TMP/new.export" | api_changes)
    if [ -z "$changes" ]; then
      echo "$d: no change"
      continue
    fi
    echo "$d:"
    echo "$changes" | sed 's/^/  /'
    API_CHANGES=$((API_CHANGES + $(echo "$changes" | wc -l)))
    API_BREAKS=$((API_BREAKS + $(echo "$changes" | grep -c '^incompatible: ' || true)))
  done
  git worktree remove --force "$TMP/base"

  # The versioning policy in CHANGELOG.md. From v1.0.0 on, an incompatible
  # change needs a new major version, which needs a new module path: that is
  # more than a version number, so the script stops.
  IFS=. read -r prev_major prev_minor _ <<< "${PREV#v}"
  if [ "$API_CHANGES" -gt 0 ] && [ "$MAJOR" = "$prev_major" ] && [ "$MINOR" = "$prev_minor" ]; then
    die "$VERSION is a patch release of $PREV, but the API changed (above). CHANGELOG.md allows API changes in a minor release only: release v$prev_major.$((prev_minor + 1)).0 instead"
  fi
  if [ "$API_BREAKS" -gt 0 ] && [ "$MAJOR" = "$prev_major" ] && [ "$MAJOR" -ge 1 ]; then
    die "$API_BREAKS incompatible API change(s) since $PREV (above): from v1.0.0 on, they need a new major version and module path"
  fi
  if [ "$API_CHANGES" -eq 0 ]; then
    API_SUMMARY="no API change since $PREV"
  else
    API_SUMMARY="$API_CHANGES API change(s) since $PREV, $API_BREAKS of them incompatible"
    echo
    echo "Check that CHANGELOG.md lists these changes, the incompatible ones under Changed or Removed."
  fi
fi

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
  echo "API: $API_SUMMARY."
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
# go list runs outside any module, so the repository's go.mod files play no part.
mkdir "$TMP/proxy"

# The proxy fetches a new tag from the repository on the first request, which
# can fail for a moment after the push. Retry before giving up.
for d in "${PUBLISHED[@]}"; do
  path=$(published_path "$d")
  ok=0
  for attempt in 1 2 3 4 5 6; do
    if (cd "$TMP/proxy" && GOPROXY=$PROXY GOFLAGS= GOPRIVATE= GONOPROXY= \
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
