#!/usr/bin/env bash
#
# The Endpoint Picker (EPP) test lanes, the same commands locally and in CI
# (.github/workflows/epp-sanity.yml), so a green run on a laptop means a
# green run on the runner.
#
#   unit      pkg/epp (the ext_proc client and its fake EPP) and the two
#             pure-Go commands. No root, no cgo, a few seconds.
#   datapath  the in-process data plane: the loxinet harness loads eBPF and
#             runs the real sockproxy workers, and the EPP tests drive HTTP
#             through a VIP to a fake EPP and fake backends. Needs root and a
#             built loxilb-ebpf tree (make). The remote-backend test also
#             needs ip, tc and the netem qdisc; it skips without them.
#   all       both (the default).
#
# Usage: scripts/epp-test.sh [unit|datapath|all]
#        sudo -E env "PATH=$PATH" scripts/epp-test.sh datapath
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 2
LANE="${1:-all}"
LOG="${EPP_TEST_LOG:-/tmp/epp-test-$$.log}"
FAILED=0

fail() { printf '  FAIL  %s\n' "$1"; FAILED=1; }
pass() { printf '  ok    %s\n' "$1"; }
run()  { # <label> <command...>: the command's output is kept on failure
  local label="$1"; shift
  if "$@" >"$LOG" 2>&1; then pass "$label"; else fail "$label"; tail -40 "$LOG"; fi
}

# Under sudo, reuse the invoking user's build cache (root's is empty and the
# datapath packages take minutes to compile) unless the caller set one.
if [ "$(id -u)" = 0 ] && [ -n "${SUDO_USER:-}" ] && [ -z "${GOCACHE:-}" ]; then
  sudo_home="$(getent passwd "$SUDO_USER" | cut -d: -f6)"
  export GOCACHE="$sudo_home/.cache/go-build"
  export GOMODCACHE="${GOMODCACHE:-$sudo_home/go/pkg/mod}"
fi

unit_lane() {
  echo "==== epp unit (no root, no cgo) ===="
  run "go vet pkg/epp, cmd/epp-fake, cmd/epp-probe" \
    env CGO_ENABLED=0 go vet ./pkg/epp/... ./cmd/epp-fake ./cmd/epp-probe
  run "go test -race pkg/epp" \
    go test -race -count=1 ./pkg/epp/...
  run "cmd/epp-fake builds without cgo" \
    env CGO_ENABLED=0 go build -o /dev/null ./cmd/epp-fake
  run "cmd/epp-probe builds without cgo" \
    env CGO_ENABLED=0 go build -o /dev/null ./cmd/epp-probe
}

datapath_lane() {
  echo "==== epp datapath (root, in-process eBPF + sockproxy) ===="
  if [ "$(id -u)" != 0 ]; then
    fail "the datapath lane needs root (sudo -E env \"PATH=\$PATH\" $0 datapath)"
    return
  fi
  local lib="$ROOT/loxilb-ebpf/kernel/libloxilbdp.a"
  if [ ! -f "$lib" ]; then
    fail "loxilb-ebpf is not built ($lib missing): run make first"
    return
  fi
  # The C side of the EPP stage: rule words reach the proxy pool.
  run "C unit test_epp" make -C loxilb-ebpf/common test_epp

  # Link against the submodule's libbpf, never a stale system one (R10), and
  # relink when the C library changed: Go's build cache does not see it.
  export CGO_LDFLAGS="-L$ROOT/loxilb-ebpf/libbpf/src/build/usr/lib64 ${CGO_LDFLAGS:-}"
  local bust; bust="$(stat -c %Y "$lib")"
  # TestMain is the harness (loxiNetInit); without it in -run every EPP test
  # skips, which is why the skip count is checked below.
  if go test -count=1 -v -ldflags="-X=main.eppbust=$bust" \
       -run 'TestMain|TestEpp' ./pkg/loxinet/ >"$LOG" 2>&1; then
    pass "go test pkg/loxinet -run 'TestMain|TestEpp'"
  else
    fail "go test pkg/loxinet -run 'TestMain|TestEpp'"
    grep -E '^(\s*--- FAIL|FAIL|panic:)' "$LOG" | head -20
    grep -B2 -A12 '^\s*--- FAIL' "$LOG" | head -80
  fi
  local passed skipped
  passed="$(grep -c '^--- PASS: TestEpp' "$LOG")"
  skipped="$(grep -c '^\s*--- SKIP: TestEpp' "$LOG")"
  printf '        %s EPP tests passed, %s skipped\n' "$passed" "$skipped"
  if [ "$passed" -lt 8 ]; then
    fail "only $passed EPP tests ran: the harness did not start (not root, or TestMain not in -run)"
  fi
  if [ "$skipped" -gt 1 ]; then
    fail "$skipped EPP tests skipped (only the netns remote-backend test may skip)"
    grep '^\s*--- SKIP: TestEpp' "$LOG"
  elif [ "$skipped" = 1 ]; then
    printf '        skipped: %s\n' "$(grep '^\s*--- SKIP: TestEpp' "$LOG" | tr -s ' ')"
  fi
}

case "$LANE" in
  unit) unit_lane ;;
  datapath) datapath_lane ;;
  all) unit_lane; datapath_lane ;;
  *) echo "usage: $0 [unit|datapath|all]" >&2; exit 2 ;;
esac

rm -f "$LOG"
if [ "$FAILED" != 0 ]; then echo "==== epp-test: FAILED ===="; exit 1; fi
echo "==== epp-test: ok ===="
