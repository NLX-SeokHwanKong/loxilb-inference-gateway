#!/bin/bash
# Host capability probe for the CICD suite.
#
# preflight-deps.sh installs what every scenario needs and fails when it
# cannot. This script is its sibling for what only SOME scenarios need: a
# missing capability is not a reason to stop the suite, it is a reason to
# skip the scenarios that declare it (run_scenario --needs) and say why.
# Without that, a bed that lacks one of these fails the affected scenarios in
# ways that read like product defects:
#
#   sctp    cicd/common/sctp_client and sctp_server are committed, dynamically
#           linked ELFs. Without libsctp.so.1 they exit at load time, and the
#           scenario reports "All Servers are not UP".
#   ipv6    a kernel booted with ipv6.disable=1 has no IPv6 at all. The
#           NAT66/NAT64 and IPv6 health-check scenarios cannot reach their
#           servers, and gobgpd exits at start (it listens on [::]:179).
#   minica  the TLS scenarios take minica from PATH or $(go env GOPATH)/bin
#           and fall back to `go install`. This probe makes the same attempt
#           once, so an offline bed without it is told up front.
#
# One check is not a capability: every scenario sources common.sh, which runs
# the committed cicd/common/minica. If that binary cannot load, nothing can
# run, so it is reported as a failure (exit 1) rather than as a skip.
#
# Output, one line per capability, read by scenario_runner.sh:
#
#   cap <name> ok
#   cap <name> missing <reason>
set -u
cd "$(dirname "$0")" || exit 2

rc=0
cap() { printf 'cap %-7s %s\n' "$1" "$2"; }

# The shared libraries an ELF cannot resolve, space-separated. ldd is the
# check that matches how the binary actually fails; whether a package is
# installed is a proxy for it.
unresolved() {
  ldd "$@" 2>/dev/null | awk '/not found/ {print $1}' | sort -u | tr '\n' ' '
}

if ! command -v ldd >/dev/null 2>&1; then
  echo "[FAIL] ldd is unavailable, so the committed binaries cannot be checked"
  exit 1
fi

# ── the vendored minica every scenario runs ────────────────────────────────
miss=$(unresolved common/minica)
if [[ ! -x common/minica ]]; then
  echo "[FAIL] cicd/common/minica is missing or not executable (common.sh runs it for every scenario)"
  rc=1
elif [[ -n $miss ]]; then
  echo "[FAIL] cicd/common/minica cannot load: unresolved ${miss}(common.sh runs it for every scenario)"
  rc=1
fi

# ── sctp ────────────────────────────────────────────────────────────────────
miss=$(unresolved common/sctp_client common/sctp_server)
if [[ -z $miss ]]; then
  cap sctp ok
else
  cap sctp "missing cicd/common/sctp_client cannot load: unresolved ${miss}(install lksctp-tools)"
fi

# ── ipv6 ────────────────────────────────────────────────────────────────────
if [[ ! -d /proc/sys/net/ipv6 ]]; then
  if grep -qw 'ipv6.disable=1' /proc/cmdline 2>/dev/null; then
    cap ipv6 "missing the kernel was booted with ipv6.disable=1"
  else
    cap ipv6 "missing the kernel has no IPv6 support (/proc/sys/net/ipv6 is absent)"
  fi
elif [[ $(cat /proc/sys/net/ipv6/conf/all/disable_ipv6 2>/dev/null) != 0 ]]; then
  cap ipv6 "missing net.ipv6.conf.all.disable_ipv6 is set"
else
  cap ipv6 ok
fi

# ── minica on PATH ──────────────────────────────────────────────────────────
have_minica() {
  command -v minica >/dev/null 2>&1 && return 0
  command -v go >/dev/null 2>&1 && [[ -x "$(go env GOPATH)/bin/minica" ]]
}
if ! have_minica && command -v go >/dev/null 2>&1; then
  go install github.com/jsha/minica@latest >/dev/null 2>&1 || true
fi
if have_minica; then
  cap minica ok
elif command -v go >/dev/null 2>&1; then
  cap minica "missing not on PATH or in \$(go env GOPATH)/bin, and 'go install github.com/jsha/minica@latest' failed"
else
  cap minica "missing not on PATH, and no go toolchain to install it with"
fi

exit $rc
