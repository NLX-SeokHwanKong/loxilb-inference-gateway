#!/usr/bin/env bash
#
# check-ebpf-pin.sh — the loxilb-ebpf submodule is pinned to a commit on the
# ebpf fork's main.
#
# A gateway change that needs a data-plane change is tested by pinning the
# submodule to the open ebpf PR's head first, then re-pinned to that PR's
# merge commit once it lands. Nothing else stops the first pin from being
# merged here by mistake: CI passes on it, because the code it names exists.
# But a commit that only lived on a topic branch, or was squashed or rebased
# away, is not part of the published ebpf history, so nobody could rebuild
# this tree from it.
#
# Usage:
#   scripts/check-ebpf-pin.sh [--ref <rev>] [--repo <url-or-path>]
#                             [--branch <name>] [--allow-unmerged]
#
#   --ref <rev>        gateway revision whose pin is checked (default: HEAD)
#   --repo <url-or-path>
#                      ebpf repository to check reachability against
#                      (default: the submodule's URL in .gitmodules)
#   --branch <name>    branch the pin must be reachable from (default: main)
#   --allow-unmerged   a deliberate temporary pin: report an unreachable pin
#                      as a warning, not a finding (CI passes this when the
#                      pull request carries the ebpf-temp-pin label)
#
# Exit: 0 = the pin is sound (or allowed), 1 = a finding, 2 = usage.
set -u

SUBMODULE=loxilb-ebpf
REF=HEAD
REPO=""
BRANCH=main
ALLOW=0
while [ $# -gt 0 ]; do
  case "$1" in
    --ref) REF="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    --branch) BRANCH="$2"; shift 2 ;;
    --allow-unmerged) ALLOW=1; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

cd "$(dirname "$0")/.." || exit 2

fail() { printf 'FAIL: %s\n' "$1"; exit 1; }
pass() { printf 'ok:   %s\n' "$1"; }

# The pin is the gitlink recorded in the tree, not whatever the submodule
# checkout happens to be: that is what a clone of this revision builds.
ENTRY=$(git ls-tree "$REF" "$SUBMODULE" 2>/dev/null)
MODE=$(printf '%s' "$ENTRY" | awk '{print $1}')
PIN=$(printf '%s' "$ENTRY" | awk '{print $3}')
if [ -z "$ENTRY" ]; then
  fail "$REF has no $SUBMODULE entry"
fi
if [ "$MODE" != 160000 ]; then
  fail "$SUBMODULE in $REF is not a submodule (mode $MODE)"
fi
pass "$REF pins $SUBMODULE at $PIN"

if [ -z "$REPO" ]; then
  REPO=$(git config -f .gitmodules "submodule.$SUBMODULE.url")
  [ -n "$REPO" ] || fail "no URL for $SUBMODULE in .gitmodules"
fi

# A bare, single-branch clone carries the whole history of the branch and
# nothing else, which is exactly what an ancestry test needs. Blobs are left
# out where the transport supports it; only commits matter here.
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
CLONE_OPTS=(--quiet --bare --single-branch --branch "$BRANCH")
case "$REPO" in
  https://*|http://*|ssh://*|git@*) CLONE_OPTS+=(--filter=blob:none) ;;
esac
if ! git clone "${CLONE_OPTS[@]}" "$REPO" "$TMP/ebpf" 2>"$TMP/err"; then
  fail "cannot fetch $BRANCH of $REPO: $(head -1 "$TMP/err")"
fi

if git -C "$TMP/ebpf" merge-base --is-ancestor "$PIN" "$BRANCH" 2>/dev/null; then
  BEHIND=$(git -C "$TMP/ebpf" rev-list --count "$PIN..$BRANCH")
  pass "pin $PIN is reachable from $BRANCH of $REPO ($BEHIND commit(s) behind its tip)"
  exit 0
fi

MSG="pin $PIN is not reachable from $BRANCH of $REPO: pin the ebpf change's merge commit before this merges"
if [ "$ALLOW" = 1 ]; then
  printf 'WARN: %s (allowed: deliberate temporary pin)\n' "$MSG"
  exit 0
fi
fail "$MSG"
