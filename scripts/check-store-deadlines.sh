#!/usr/bin/env bash
# check-store-deadlines.sh - every key-store and management-store statement
# carries a context deadline.
#
# A PostgreSQL that stalls while keeping TCP open answers a statement that
# has no deadline never. The DBTX interfaces in pkg/aikey and pkg/user carry
# only the *Context methods, so a call through them cannot regress; this
# check covers what the interface cannot: a call on a concrete *sql.DB,
# *sql.Tx or *sql.Conn (the provisioning code, a transaction body, a
# helper that took the pool rather than the interface).
#
# Exit 1 and list the offenders when any non-test Go file in the store
# packages, or the data-path bridge, calls a database/sql method without a
# context. Run from the repository root; CI runs it in auth-plane-sanity.
set -euo pipefail

cd "$(dirname "$0")/.."

roots=(pkg/aikey pkg/pgstore pkg/user pkg/loxinet/ai_gateway_dp.go)
# Receiver names the store code uses for a pool, transaction or handle.
pattern='\b(db|tx|handle|pool|conn|h|d)\.(Exec|Query|QueryRow|Ping|Begin|Prepare)\('

offenders=$(grep -rnE --include='*.go' --exclude='*_test.go' "$pattern" "${roots[@]}" || true)
if [ -n "$offenders" ]; then
	echo "store calls without a context deadline (use the *Context method):" >&2
	echo "$offenders" >&2
	exit 1
fi
echo "check-store-deadlines: every store call in ${roots[*]} carries a context"
