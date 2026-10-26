/*
 * Copyright (c) 2026 LoxiLB Authors
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

package aikey

// The data-path lookup guard.
//
// Every store round trip the data path makes -- the key lookup on a cache
// miss and the quota-row reads behind a request -- runs on a proxy worker
// thread inside a cgo callback, bounded by the lookup budget. The budget
// alone still costs every miss one full wait while the store stalls, on
// every worker that asks. The guard in front of those reads adds two things:
//
//   - an in-flight bound, so that at most a few workers are parked on the
//     store at once and the rest learn inside the same budget that they will
//     not be served;
//   - a store-health circuit, so that once the store has failed a few
//     consecutive round trips nothing waits on it at all until a cool-down
//     has passed and one probe has succeeded.
//
// A refused read takes the fallback branch the read already has for an
// unreachable store: the last store-confirmed value for a quota row, the
// store-unavailable answer for a key. Cache hits never reach the guard. The
// management plane never consults it: its calls carry their own budget and
// must report real outcomes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Environment knobs. Each has a default and a floor; a value below the floor
// is raised to it, an unparsable value is ignored.
const (
	// LookupMaxInflightEnv bounds how many data-path round trips may be on
	// the store at once. A worker that finds no slot waits for one until its
	// lookup budget expires, so wait plus statement never exceed one budget.
	LookupMaxInflightEnv     = "LLB_AI_KEY_LOOKUP_MAX_INFLIGHT"
	lookupMaxInflightDefault = 2

	// LookupCircuitFailuresEnv is the number of consecutive store failures
	// (deadline, transport, not connected) that opens the circuit. 0
	// disables the circuit: every read is admitted.
	LookupCircuitFailuresEnv     = "LLB_AI_KEY_LOOKUP_CIRCUIT_FAILURES"
	lookupCircuitFailuresDefault = 3

	// LookupCircuitCooldownEnv is how long the circuit stays open before one
	// probe read is let through, in milliseconds.
	LookupCircuitCooldownEnv       = "LLB_AI_KEY_LOOKUP_CIRCUIT_COOLDOWN_MS"
	lookupCircuitCooldownDefaultMs = 5000
	lookupCircuitCooldownFloorMs   = 100

	// NegativeCacheEnv is how long a key hash the store answered "no such
	// key" for is refused without asking again, in milliseconds. 0 disables
	// the negative cache.
	NegativeCacheEnv       = "LLB_AI_KEY_NEGATIVE_CACHE_MS"
	negativeCacheDefaultMs = 2000

	// NegativeCacheCapEnv bounds the negative cache's entries; the oldest
	// entry is dropped when a new one arrives at the cap.
	NegativeCacheCapEnv     = "LLB_AI_KEY_NEGATIVE_CACHE_CAP"
	negativeCacheCapDefault = 4096
)

// lookupResult labels one data-path round trip for the lookup statistics.
// The values are the `result` label of loxilb_ai_key_store_lookup_seconds.
// A cache hit is not a round trip and is not counted here.
type lookupResult int

const (
	resultMissFound       lookupResult = iota // the store answered with a row
	resultMissInvalid                         // the store answered "no row"
	resultTimeout                             // the lookup budget expired
	resultRefusedCircuit                      // refused: the circuit is open
	resultRefusedInflight                     // refused: no slot inside the budget
	resultUnavailable                         // another store failure, or not connected
	resultCount
)

// LookupResultNames are the `result` label values, indexed by lookupResult.
var LookupResultNames = [resultCount]string{
	"miss_found", "miss_invalid", "timeout", "refused_circuit", "refused_inflight", "unavailable",
}

// LookupBucketBounds are the histogram's upper bounds in seconds. The default
// lookup budget (0.3 s) is a bound, so a stall shows as samples at that edge.
var LookupBucketBounds = [...]float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1}

// LookupStats is a snapshot of the guard's counters for the scrape-time
// collector in api/prometheus. pkg/aikey imports no Prometheus client; the
// collector turns this into the exposition.
type LookupStats struct {
	// Count, Sum and Buckets are indexed like LookupResultNames. Buckets
	// holds cumulative counts per LookupBucketBounds entry (the +Inf bucket
	// is Count).
	Count   [resultCount]uint64
	Sum     [resultCount]float64
	Buckets [resultCount][len(LookupBucketBounds)]uint64
	// CircuitOpen is true while the circuit is open or half-open: no
	// data-path read reaches the store except the half-open probe. It stays
	// true after the store recovers until a miss runs that probe.
	CircuitOpen bool
	// InFlight is the number of round trips on the store right now.
	InFlight int
}

type circuitState int32

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

// lookupGuardConfig is read once from the environment, at the first use.
type lookupGuardConfig struct {
	maxInflight int
	failures    int
	cooldown    time.Duration
	negativeTTL time.Duration
	negativeCap int
}

func intFromEnv(env string, def, floor int) int {
	v := def
	if raw := os.Getenv(env); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			v = n
		}
	}
	if v < floor {
		v = floor
	}
	return v
}

func readLookupGuardConfig() lookupGuardConfig {
	return lookupGuardConfig{
		maxInflight: intFromEnv(LookupMaxInflightEnv, lookupMaxInflightDefault, 1),
		failures:    intFromEnv(LookupCircuitFailuresEnv, lookupCircuitFailuresDefault, 0),
		cooldown:    time.Duration(intFromEnv(LookupCircuitCooldownEnv, lookupCircuitCooldownDefaultMs, lookupCircuitCooldownFloorMs)) * time.Millisecond,
		negativeTTL: time.Duration(intFromEnv(NegativeCacheEnv, negativeCacheDefaultMs, 0)) * time.Millisecond,
		negativeCap: intFromEnv(NegativeCacheCapEnv, negativeCacheCapDefault, 1),
	}
}

// lookupGuard is usable at its zero value: the configuration is read and the
// slots are made at the first admit, so a Service built without New (the
// tests do) carries a working guard.
type lookupGuard struct {
	once sync.Once
	cfg  lookupGuardConfig
	now  func() time.Time
	sem  chan struct{}

	// Circuit state. gen rises on every transition; a statement admitted
	// under an older generation reports into the statistics but never moves
	// the state (see report).
	mu         sync.Mutex
	state      circuitState
	gen        uint64
	failures   int
	openedAt   time.Time
	probeTaken bool

	// Statistics, atomics so that the scrape never takes mu.
	count   [resultCount]atomic.Uint64
	sumNs   [resultCount]atomic.Int64
	buckets [resultCount][len(LookupBucketBounds)]atomic.Uint64
}

// lookupToken is what admit hands an admitted read and report takes back.
type lookupToken struct {
	gen   uint64
	probe bool
	slot  bool
}

func (g *lookupGuard) init() {
	g.once.Do(func() {
		g.cfg = readLookupGuardConfig()
		if g.now == nil {
			g.now = time.Now
		}
		g.sem = make(chan struct{}, g.cfg.maxInflight)
	})
}

// errCircuitOpen and errLookupBusy are the guard's refusals. Both are
// ErrDBUnavailable to every errors.Is check, so a caller's fallback branch
// handles them exactly as it handles a store that did not answer.
var (
	errCircuitOpen = fmt.Errorf("aikey: key store circuit open: %w", ErrDBUnavailable)
	errLookupBusy  = fmt.Errorf("aikey: key store lookups at the in-flight bound: %w", ErrDBUnavailable)
)

// admit decides whether one data-path round trip may go to the store. It
// returns the token the read reports with, or the refusal and its result.
// The wait for a slot is bounded by ctx, which carries the lookup budget.
func (g *lookupGuard) admit(ctx context.Context) (lookupToken, lookupResult, error) {
	g.init()
	tok, ok := g.pass()
	if !ok {
		return lookupToken{}, resultRefusedCircuit, errCircuitOpen
	}
	select {
	case g.sem <- struct{}{}:
		tok.slot = true
		return tok, resultCount, nil
	case <-ctx.Done():
		// The probe, if this was it, did not run: give it back so the next
		// miss after the cool-down can take it.
		if tok.probe {
			g.mu.Lock()
			if g.state == circuitHalfOpen && g.gen == tok.gen {
				g.probeTaken = false
			}
			g.mu.Unlock()
		}
		return lookupToken{}, resultRefusedInflight, errLookupBusy
	}
}

// pass is the circuit's admission decision.
func (g *lookupGuard) pass() (lookupToken, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch g.state {
	case circuitClosed:
		return lookupToken{gen: g.gen}, true
	case circuitOpen:
		if g.now().Sub(g.openedAt) < g.cfg.cooldown {
			return lookupToken{}, false
		}
		g.state = circuitHalfOpen
		g.gen++
		g.probeTaken = true
		return lookupToken{gen: g.gen, probe: true}, true
	default: // half-open: exactly one probe is out
		if g.probeTaken {
			return lookupToken{}, false
		}
		g.probeTaken = true
		return lookupToken{gen: g.gen, probe: true}, true
	}
}

// report releases the read's slot, records its outcome, and moves the circuit
// when the outcome is one that may: a consecutive failure in the closed
// state, or the probe's outcome in the half-open state. A read admitted
// before the state last changed (its generation is stale) is counted and
// nothing more, so a statement that completes late cannot re-open a circuit
// that has since recovered, nor close one its peers have just opened.
func (g *lookupGuard) report(tok lookupToken, res lookupResult, took time.Duration) {
	if tok.slot {
		<-g.sem
	}
	g.observe(res, took)
	failure := res == resultTimeout || res == resultUnavailable
	g.mu.Lock()
	defer g.mu.Unlock()
	if tok.gen != g.gen {
		return
	}
	switch g.state {
	case circuitClosed:
		if !failure {
			g.failures = 0
			return
		}
		g.failures++
		if g.cfg.failures > 0 && g.failures >= g.cfg.failures {
			g.state = circuitOpen
			g.openedAt = g.now()
			g.gen++
		}
	case circuitHalfOpen:
		if !tok.probe {
			return
		}
		g.probeTaken = false
		g.gen++
		if failure {
			g.state = circuitOpen
			g.openedAt = g.now()
			return
		}
		g.state = circuitClosed
		g.failures = 0
	}
}

// observe records one outcome into the statistics.
func (g *lookupGuard) observe(res lookupResult, took time.Duration) {
	g.init()
	g.count[res].Add(1)
	g.sumNs[res].Add(int64(took))
	secs := took.Seconds()
	for i, bound := range LookupBucketBounds {
		if secs <= bound {
			g.buckets[res][i].Add(1)
		}
	}
}

// snapshot is LookupStats for the collector.
func (g *lookupGuard) snapshot() LookupStats {
	g.init()
	var st LookupStats
	for r := range st.Count {
		st.Count[r] = g.count[r].Load()
		st.Sum[r] = time.Duration(g.sumNs[r].Load()).Seconds()
		for i := range st.Buckets[r] {
			st.Buckets[r][i] = g.buckets[r][i].Load()
		}
	}
	g.mu.Lock()
	st.CircuitOpen = g.state != circuitClosed
	g.mu.Unlock()
	st.InFlight = len(g.sem)
	return st
}

// classifyLookup maps a statement's error to its result. nil and
// sql.ErrNoRows are answers; a deadline is a timeout; anything else the
// store could not be asked.
func classifyLookup(ctx context.Context, err error) lookupResult {
	switch {
	case err == nil:
		return resultMissFound
	case errors.Is(err, sql.ErrNoRows):
		return resultMissInvalid
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return resultTimeout
	default:
		return resultUnavailable
	}
}

// dataPathRead runs one guarded store round trip under the lookup budget.
// fn runs the statement and returns its error unchanged (nil, sql.ErrNoRows
// or a driver error); the caller then takes exactly the branch it took
// before the guard existed. A refusal, and a store that is not connected,
// come back as ErrDBUnavailable without the statement running.
func (s *Service) dataPathRead(fn func(ctx context.Context, db DBTX) error) error {
	db, err := s.store()
	if err != nil {
		s.guard.observe(resultUnavailable, 0)
		return err
	}
	ctx, cancel := lookupContext()
	defer cancel()
	start := s.guardNow()
	tok, refusal, err := s.guard.admit(ctx)
	if err != nil {
		s.guard.observe(refusal, s.guardNow().Sub(start))
		return err
	}
	err = fn(ctx, db)
	s.guard.report(tok, classifyLookup(ctx, err), s.guardNow().Sub(start))
	return err
}

func (s *Service) guardNow() time.Time {
	s.guard.init()
	return s.guard.now()
}

// LookupStats returns the data-path lookup statistics for the metrics
// collector. Safe on a nil Service: everything reads zero.
func (s *Service) LookupStats() LookupStats {
	if s == nil {
		return LookupStats{}
	}
	return s.guard.snapshot()
}

// negativeKeyCache remembers key hashes the store answered "no such key"
// for, so that a flood of unknown keys does not spend a round trip, a slot
// and a budget each. Bounded; the oldest entry goes first.
type negativeKeyCache struct {
	mu      sync.Mutex
	entries map[string]time.Time // hash -> expiry
	order   []string             // insertion order, for the cap
}

// negativeHit reports whether hash is remembered as invalid and unexpired.
func (s *Service) negativeHit(hash string) bool {
	s.guard.init()
	if s.guard.cfg.negativeTTL <= 0 {
		return false
	}
	n := &s.negative
	n.mu.Lock()
	defer n.mu.Unlock()
	exp, ok := n.entries[hash]
	if !ok {
		return false
	}
	if s.guard.now().After(exp) {
		delete(n.entries, hash)
		return false
	}
	return true
}

// rememberNegative records hash as invalid until the TTL passes.
func (s *Service) rememberNegative(hash string) {
	s.guard.init()
	ttl := s.guard.cfg.negativeTTL
	if ttl <= 0 {
		return
	}
	n := &s.negative
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.entries == nil {
		n.entries = make(map[string]time.Time)
	}
	if _, present := n.entries[hash]; !present {
		for len(n.order) >= s.guard.cfg.negativeCap {
			oldest := n.order[0]
			n.order = n.order[1:]
			delete(n.entries, oldest)
		}
		n.order = append(n.order, hash)
	}
	n.entries[hash] = s.guard.now().Add(ttl)
}

// forgetNegative drops hash: the key now exists, or a peer says so.
func (s *Service) forgetNegative(hash string) {
	n := &s.negative
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.entries[hash]; !ok {
		return
	}
	delete(n.entries, hash)
	for i, h := range n.order {
		if h == hash {
			n.order = append(n.order[:i], n.order[i+1:]...)
			break
		}
	}
}

// negativeLen is the entry count, for the tests.
func (s *Service) negativeLen() int {
	n := &s.negative
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.entries)
}
