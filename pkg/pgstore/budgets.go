/*
 * Copyright (c) 2025 LoxiLB Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package pgstore

import (
	"context"
	"os"
	"strconv"
	"time"
)

// Time budgets for store access. Every statement either plane runs carries
// one of these as a context deadline; none runs without one.
//
// They exist because a PostgreSQL that stalls while keeping its TCP
// connections open -- a hung or overloaded server, a stalled volume, a path
// that black-holes packets, a paused container -- returns neither an error
// nor a reply. connect_timeout bounds only the dial; a statement sent on a
// pooled connection that was healthy a moment ago waits for as long as the
// caller lets it, and before these budgets the caller let it wait forever: a
// management write committed after its client had given up, a readiness
// probe hung, and a cache-miss key lookup parked the proxy worker thread that
// asked, with every connection that worker owned.
//
// Each budget is an environment variable in milliseconds with a default that
// sits well below any client timeout, so the gateway answers first and says
// what it knows. The floor keeps a misconfiguration from turning every store
// call into an instant failure.
const (
	// OpTimeoutEnv bounds one management operation: a configuration read or
	// write, including a retrying operation's retries and the snapshot
	// refresh a write performs before it returns.
	OpTimeoutEnv       = "LLB_AI_STORE_OP_TIMEOUT_MS"
	opTimeoutDefaultMs = 3000

	// PingTimeoutEnv bounds the readiness ping and the housekeeping ping.
	PingTimeoutEnv       = "LLB_AI_STORE_PING_TIMEOUT_MS"
	pingTimeoutDefaultMs = 1000

	// RefreshTimeoutEnv bounds one read of the class and limit snapshot. It
	// is clamped below the refresh interval by the refresher, so a stalled
	// read never overlaps the next tick.
	RefreshTimeoutEnv       = "LLB_AI_STORE_REFRESH_TIMEOUT_MS"
	refreshTimeoutDefaultMs = 3000

	// LookupTimeoutEnv bounds one data-path store round trip: the key
	// lookup on a cache miss and the quota-row reads behind it. These run
	// on a proxy worker thread, so the budget is the longest that thread is
	// ever parked by the store.
	LookupTimeoutEnv       = "LLB_AI_KEY_LOOKUP_TIMEOUT_MS"
	lookupTimeoutDefaultMs = 300

	// minBudget is the floor on any budget. Below it a healthy store on a
	// loaded host fails its statements for no reason.
	minBudget = 10 * time.Millisecond

	// ConnMaxIdleTime is how long a pooled connection may sit idle before the
	// pool drops it. A connection that was healthy and then idled through a
	// stall is the one a later statement is handed; bounding the idle time
	// bounds how long after a stall such connections keep being handed out.
	ConnMaxIdleTime = 60 * time.Second
)

// budgetFromEnv reads a millisecond budget from env, with def when the
// variable is unset or not a positive integer, and never below minBudget.
func budgetFromEnv(env string, defMs int) time.Duration {
	ms := defMs
	if v := os.Getenv(env); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ms = n
		}
	}
	d := time.Duration(ms) * time.Millisecond
	if d < minBudget {
		d = minBudget
	}
	return d
}

// OpTimeout is the management operation budget.
func OpTimeout() time.Duration { return budgetFromEnv(OpTimeoutEnv, opTimeoutDefaultMs) }

// PingTimeout is the readiness and housekeeping ping budget.
func PingTimeout() time.Duration { return budgetFromEnv(PingTimeoutEnv, pingTimeoutDefaultMs) }

// RefreshTimeout is the snapshot refresh budget, before the refresher's
// clamp against its interval.
func RefreshTimeout() time.Duration {
	return budgetFromEnv(RefreshTimeoutEnv, refreshTimeoutDefaultMs)
}

// LookupTimeout is the data-path lookup budget.
func LookupTimeout() time.Duration { return budgetFromEnv(LookupTimeoutEnv, lookupTimeoutDefaultMs) }

// WithBudget derives a context that expires after budget, or sooner if
// parent does. A nil parent is the background context: a caller with no
// request to inherit from (a ticker, the data path) still gets the deadline.
func WithBudget(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, budget)
}
