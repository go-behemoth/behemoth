#!/usr/bin/env bash
#
# Manages worktrees: extra checkouts of this repository, one per task, so that
# several tasks can be worked on at the same time without sharing a working
# tree.
#
#   scripts/worktree.sh new NAME [BASE]
#   scripts/worktree.sh link [DIR]
#   scripts/worktree.sh rm NAME
#   scripts/worktree.sh list
#
# new creates the worktree WORKTREES/NAME on the branch NAME. A branch that
# doesn't exist yet is made from BASE, dev by default. An existing branch is
# checked out as it is, and BASE must then be left out. A "/" in NAME becomes
# "-" in the folder name.
#
# WORKTREES is the folder next to the main checkout, named after it with a
# "-wt" suffix, such as ../behemoth-wt. Set BEHEMOTH_WORKTREES to use another
# folder.
#
# The local-only files are the files in LOCAL_FILES below. The .gitignore keeps
# them out of the repository, so git worktree add doesn't copy them. new links
# each one into the worktree, pointing at the file in the main checkout, and
# every worktree then reads and writes the same copy. link does the same for
# the worktree that holds DIR, the current directory by default. Use it on a
# worktree made another way, such as by git worktree add or claude --worktree.
# A file is skipped when the main checkout doesn't have it, when the worktree
# has a file at its path already, or when the worktree's .gitignore doesn't
# ignore it, so that a link is never committed.
#
# rm removes the worktree of NAME. git refuses when the worktree has changes
# that aren't committed. The branch stays: delete it with git branch -d once
# it is merged.
#
# list lists every worktree with its branch.
#
# The script works from the main checkout and from any worktree.

set -euo pipefail

LOCAL_FILES=(
  CLAUDE.md
  docs/ongoing.md
  docs/plans.md
  release_plan.md
  examples/init/.env
)

usage() {
  cat >&2 <<'EOF'
usage: scripts/worktree.sh new NAME [BASE]
       scripts/worktree.sh link [DIR]
       scripts/worktree.sh rm NAME
       scripts/worktree.sh list
EOF
  exit 2
}

die() { echo "worktree: $*" >&2; exit 1; }

# The first worktree git lists is the main checkout.
MAIN=$(git -C "$(dirname "$0")" worktree list --porcelain | sed -n '1s/^worktree //p')
[ -n "$MAIN" ] || die "can't find the main checkout"
WORKTREES=${BEHEMOTH_WORKTREES:-$(dirname "$MAIN")/$(basename "$MAIN")-wt}

path_of() { echo "$WORKTREES/${1//\//-}"; }

link() {
  local top f src dst
  top=$(git -C "$1" rev-parse --show-toplevel) || exit 1
  [ "$top" != "$MAIN" ] || die "$top is the main checkout"
  for f in "${LOCAL_FILES[@]}"; do
    src=$MAIN/$f
    dst=$top/$f
    [ -e "$src" ] || continue
    if [ -L "$dst" ] && [ "$(readlink "$dst")" = "$src" ]; then
      continue
    fi
    if [ -e "$dst" ] || [ -L "$dst" ]; then
      echo "worktree: $f exists in $top, left as it is" >&2
      continue
    fi
    if ! git -C "$top" check-ignore -q "$f"; then
      echo "worktree: the .gitignore in $top doesn't ignore $f, not linked" >&2
      continue
    fi
    mkdir -p "$(dirname "$dst")"
    ln -s "$src" "$dst"
    echo "linked $f"
  done
}

new() {
  local name=$1 base=${2:-} dir
  git -C "$MAIN" check-ref-format --branch "$name" >/dev/null 2>&1 ||
    die "$name is not a valid branch name"
  dir=$(path_of "$name")
  [ ! -e "$dir" ] || die "$dir exists already"
  if git -C "$MAIN" show-ref --verify --quiet "refs/heads/$name"; then
    [ -z "$base" ] || die "branch $name exists already, leave out BASE to check it out"
    git -C "$MAIN" worktree add "$dir" "$name"
  else
    git -C "$MAIN" worktree add -b "$name" "$dir" "${base:-dev}"
  fi
  link "$dir"
  echo "worktree ready: $dir"
}

remove() {
  local dir
  dir=$(path_of "$1")
  git -C "$MAIN" worktree remove "$dir"
  echo "removed $dir, branch $1 kept (delete it with: git branch -d $1)"
}

cmd=${1:-}
[ $# -eq 0 ] || shift
case "$cmd" in
  new)  { [ $# -ge 1 ] && [ $# -le 2 ]; } || usage; new "$@" ;;
  link) [ $# -le 1 ] || usage; link "${1:-.}" ;;
  rm)   [ $# -eq 1 ] || usage; remove "$1" ;;
  list) [ $# -eq 0 ] || usage; git -C "$MAIN" worktree list ;;
  *)    usage ;;
esac
