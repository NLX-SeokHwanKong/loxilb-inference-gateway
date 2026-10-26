package aikey

// Guard tests. The circuit's state machine is driven through dataPathRead
// with closures that return the statement outcome directly, under a fake
// clock; the reads themselves are driven through the scripted store so that
// the fallbacks (last-known values, the negative cache, the by-ID view a
// validation writes) are exercised on the real code path.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"

	cmn "github.com/loxilb-io/loxilb/common"
	"github.com/loxilb-io/loxilb/pkg/pgstore"
)

// fakeClock is the guard's clock in these tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// guardedService returns a service over the scripted store with the guard
// on a fake clock. Knobs are set through the environment before the first
// use, which is when the guard reads them.
func guardedService(t *testing.T, db DBTX, maxInflight, failures int, cooldownMs int) (*Service, *fakeClock) {
	t.Helper()
	t.Setenv(LookupMaxInflightEnv, itoa(maxInflight))
	t.Setenv(LookupCircuitFailuresEnv, itoa(failures))
	t.Setenv(LookupCircuitCooldownEnv, itoa(cooldownMs))
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	svc := &Service{db: db, Cache: cache.New(CacheExpirationTime*time.Minute, CacheCleanupInterval*time.Minute)}
	svc.guard.now = clk.now
	return svc, clk
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func outcome(err error) func(context.Context, DBTX) error {
	return func(context.Context, DBTX) error { return err }
}

func stateOf(svc *Service) circuitState {
	svc.guard.mu.Lock()
	defer svc.guard.mu.Unlock()
	return svc.guard.state
}

// 1. In-flight bound: of N concurrent misses against a stalled store, at
// most MAX_INFLIGHT reach it; every caller returns inside one budget, the
// refused ones as refused_inflight; contention does not open the circuit.
func TestInflightBoundParksAtMostMaxInflightWorkers(t *testing.T) {
	const budget = 100 * time.Millisecond
	t.Setenv(pgstore.LookupTimeoutEnv, "100")
	t.Setenv(LookupMaxInflightEnv, "2")
	t.Setenv(LookupCircuitFailuresEnv, "0") // the circuit is not under test here
	svc, db := stalledService(t)

	const n = 8
	var wg sync.WaitGroup
	var tooSlow atomic.Int32
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s0 := time.Now()
			_, err := svc.ValidateAPIKey("unknown-key")
			if !errors.Is(err, cmn.ErrDBUnavailable) {
				t.Errorf("miss during a stall = %v", err)
			}
			if time.Since(s0) > budget+400*time.Millisecond {
				tooSlow.Add(1)
			}
		}()
	}
	wg.Wait()
	if tooSlow.Load() != 0 {
		t.Fatalf("%d of %d callers were parked beyond one budget", tooSlow.Load(), n)
	}
	if time.Since(start) > 2*budget+400*time.Millisecond {
		t.Fatalf("the burst took %v: callers queued behind each other instead of being refused", time.Since(start))
	}
	// The bound is on concurrency, not on the number of statements: a
	// waiter whose budget has a few microseconds left when a slot frees up
	// is granted it and times out on the store at once (the accepted late
	// grant), so the call count can exceed the bound by such near-expired
	// statements while no more than MAX_INFLIGHT ever wait at the same time.
	if peak := db.peakInflight(); peak > 2 {
		t.Fatalf("%d statements waited on the stalled store at once, bound is 2", peak)
	}
	calls, _ := db.snapshot()
	st := svc.LookupStats()
	if st.Count[resultRefusedInflight]+st.Count[resultTimeout] != n || st.Count[resultTimeout] != uint64(calls) {
		t.Fatalf("results: timeout=%d refused_inflight=%d, calls=%d, n=%d", st.Count[resultTimeout], st.Count[resultRefusedInflight], calls, n)
	}
	if st.Count[resultRefusedInflight] < n-4 {
		t.Fatalf("only %d of %d callers were refused at the bound; the rest queued", st.Count[resultRefusedInflight], n)
	}
	if st.CircuitOpen {
		t.Fatal("contention on the in-flight bound opened the circuit")
	}
}

// 2. The circuit opens after `failures` consecutive store failures; the
// next read is refused at once without touching the store.
func TestCircuitOpensAfterConsecutiveFailuresAndRefusesWithoutTheStore(t *testing.T) {
	db := newScriptDB(t)
	svc, _ := guardedService(t, db, 4, 3, 1000)
	for i := 0; i < 3; i++ {
		if err := svc.dataPathRead(outcome(context.DeadlineExceeded)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("failure %d = %v", i, err)
		}
	}
	if stateOf(svc) != circuitOpen {
		t.Fatalf("state after 3 failures = %v, want open", stateOf(svc))
	}
	ran := false
	start := time.Now()
	err := svc.dataPathRead(func(context.Context, DBTX) error { ran = true; return nil })
	if !errors.Is(err, ErrDBUnavailable) || ran || time.Since(start) > 20*time.Millisecond {
		t.Fatalf("read under an open circuit: err=%v ran=%v took=%v", err, ran, time.Since(start))
	}
	st := svc.LookupStats()
	if st.Count[resultTimeout] != 3 || st.Count[resultRefusedCircuit] != 1 || !st.CircuitOpen {
		t.Fatalf("stats = %+v", st)
	}
}

// 3. ErrNoRows is an answer, not a failure: it resets the counter.
func TestNoRowsIsAnAnswerAndResetsTheFailureCount(t *testing.T) {
	db := newScriptDB(t)
	svc, _ := guardedService(t, db, 4, 3, 1000)
	_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	_ = svc.dataPathRead(outcome(sql.ErrNoRows))
	_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	if stateOf(svc) != circuitClosed {
		t.Fatalf("a no-row answer between failures did not reset the count: state %v", stateOf(svc))
	}
	_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	if stateOf(svc) != circuitOpen {
		t.Fatal("three consecutive failures after the reset did not open the circuit")
	}
	if st := svc.LookupStats(); st.Count[resultMissInvalid] != 1 || st.Count[resultTimeout] != 5 {
		t.Fatalf("stats = %+v", st)
	}
}

// 4. After the cool-down exactly one probe is admitted; its success closes
// the circuit, its failure re-opens it for a fresh cool-down.
func TestHalfOpenAdmitsOneProbe(t *testing.T) {
	db := newScriptDB(t)
	svc, clk := guardedService(t, db, 8, 3, 1000)
	for i := 0; i < 3; i++ {
		_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	}
	clk.advance(999 * time.Millisecond)
	if err := svc.dataPathRead(outcome(nil)); !errors.Is(err, ErrDBUnavailable) {
		t.Fatalf("admitted before the cool-down: %v", err)
	}
	clk.advance(2 * time.Millisecond)

	// Four concurrent reads: one probe runs, three are refused.
	var ran atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- svc.dataPathRead(func(context.Context, DBTX) error {
				ran.Add(1)
				<-release
				return nil
			})
		}()
	}
	// Wait until the probe is inside the statement, then let it finish.
	deadline := time.Now().Add(2 * time.Second)
	for ran.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // the three others must be refused meanwhile
	close(release)
	wg.Wait()
	close(results)
	refused := 0
	for err := range results {
		if errors.Is(err, ErrDBUnavailable) {
			refused++
		}
	}
	if ran.Load() != 1 || refused != 3 {
		t.Fatalf("half-open: %d probes ran, %d refused (want 1 and 3)", ran.Load(), refused)
	}
	if stateOf(svc) != circuitClosed {
		t.Fatalf("probe success left the circuit %v", stateOf(svc))
	}

	// Open again, then a failing probe re-opens with a fresh cool-down.
	for i := 0; i < 3; i++ {
		_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	}
	clk.advance(1001 * time.Millisecond)
	_ = svc.dataPathRead(outcome(context.DeadlineExceeded)) // the probe fails
	if stateOf(svc) != circuitOpen {
		t.Fatalf("a failed probe left the circuit %v", stateOf(svc))
	}
	clk.advance(500 * time.Millisecond)
	if err := svc.dataPathRead(outcome(nil)); !errors.Is(err, ErrDBUnavailable) {
		t.Fatal("the failed probe did not start a fresh cool-down")
	}
	clk.advance(501 * time.Millisecond)
	if err := svc.dataPathRead(outcome(nil)); err != nil {
		t.Fatalf("the probe after the fresh cool-down was refused: %v", err)
	}
	if stateOf(svc) != circuitClosed {
		t.Fatal("the second probe's success did not close the circuit")
	}
}

// A statement admitted before a transition is counted and moves nothing:
// a late success cannot close a circuit its peers opened, a stale failure
// cannot re-open a half-open one or consume its probe.
func TestLateOutcomesFromBeforeATransitionDoNotMoveTheCircuit(t *testing.T) {
	db := newScriptDB(t)
	svc, clk := guardedService(t, db, 8, 3, 1000)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	svc.guard.init()
	early, _, err := svc.guard.admit(ctx) // admitted while closed
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	}
	if stateOf(svc) != circuitOpen {
		t.Fatal("precondition: the circuit should be open")
	}
	svc.guard.report(early, resultMissFound, time.Millisecond) // the late success
	if stateOf(svc) != circuitOpen {
		t.Fatal("a success admitted before the circuit opened closed it")
	}
	st := svc.LookupStats()
	if st.Count[resultMissFound] != 1 {
		t.Fatalf("the late success was not counted: %+v", st.Count)
	}

	// Half-open with the probe out; a stale failure must not touch it.
	clk.advance(1001 * time.Millisecond)
	probe, _, err := svc.guard.admit(ctx)
	if err != nil || !probe.probe {
		t.Fatalf("probe not admitted after the cool-down: err=%v tok=%+v", err, probe)
	}
	stale := lookupToken{gen: early.gen}
	svc.guard.report(stale, resultTimeout, time.Millisecond)
	if stateOf(svc) != circuitHalfOpen {
		t.Fatalf("a stale failure moved a half-open circuit to %v", stateOf(svc))
	}
	if _, _, err := svc.guard.admit(ctx); !errors.Is(err, ErrDBUnavailable) {
		t.Fatal("a stale failure gave the probe back while it was still out")
	}
	svc.guard.report(probe, resultMissFound, time.Millisecond)
	if stateOf(svc) != circuitClosed {
		t.Fatal("the probe's success did not close the circuit")
	}
}

// 5. Cache hits are unaffected by an open circuit.
func TestCacheHitsNeverReachAnOpenCircuit(t *testing.T) {
	db := newScriptDB(t)
	svc, _ := guardedService(t, db, 4, 3, 1000)
	cached := &cmn.ApiKeyEntry{KeyID: "k", TenantID: "t", Enabled: true}
	svc.Cache.Set(cacheKeyForHash(hashKey("cached-key")), cached, time.Minute)
	svc.Cache.Set(cacheKeyForTenant("t"), &rateLimitCacheEntry{rps: 9}, time.Minute)
	for i := 0; i < 3; i++ {
		_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	}
	before := svc.LookupStats()
	if e, err := svc.ValidateAPIKey("cached-key"); err != nil || e != cached {
		t.Fatalf("cached key under an open circuit: %v %v", e, err)
	}
	if rps, _, _, err := svc.GetTenantRateLimit("t"); err != nil || rps != 9 {
		t.Fatalf("cached tenant under an open circuit: rps=%d err=%v", rps, err)
	}
	after := svc.LookupStats()
	if after.Count != before.Count || db.callCount() != 0 {
		t.Fatalf("a cache hit touched the guard or the store: before=%v after=%v calls=%d", before.Count, after.Count, db.callCount())
	}
}

// 6. Under an open circuit every quota read serves its last-known value and
// refuses only what the store never described; the by-ID key view written by
// a validation is served the same way and goes with a revocation.
func TestLastKnownValuesServeUnderAnOpenCircuit(t *testing.T) {
	db := newScriptDB(t)
	svc, _ := guardedService(t, db, 4, 3, 1000)

	// A validated key: the store answers the by-hash row once.
	created := time.Unix(1_700_000_000, 0).UTC()
	db.push(scriptAnswer{values: []driver.Value{"kid-1", hashKey("live-key"), "t-1", "name", "m1,m2", int64(7), int64(3), int64(600), created, nil, true}})
	entry, err := svc.ValidateAPIKey("live-key")
	if err != nil || entry.KeyID != "kid-1" {
		t.Fatalf("validate: %v %v", entry, err)
	}
	if k, ok := svc.lastKnownKeySummary("kid-1"); !ok || k.RateLimitRPS != 7 || k.TokensPerMin != 600 || len(k.AllowedModels) != 2 {
		t.Fatalf("validation did not write the by-ID view: %+v %v", k, ok)
	}
	svc.Cache.Delete(cacheKeyForID("kid-1")) // aged out of the TTL cache

	// Quota rows the store once confirmed.
	svc.rlLastKnown.Store(cacheKeyForTenant("t-1"), &rateLimitCacheEntry{rps: 5, tokensPerMin: 500})
	svc.rlLastKnown.Store(cacheKeyForModel("t-1", "m1"), &rateLimitCacheEntry{tokensPerMin: 50})
	svc.rlLastKnown.Store(cacheKeyForUser("t-1", "u-1"), &userRateLimitCacheEntry{rps: 2, burstSize: 4, tokensPerMin: 20})
	svc.rlLastKnown.Store(cacheKeyForUserModel("t-1", "u-1", "m1"), &userRateLimitCacheEntry{tokensPerMin: 10})
	svc.rlLastKnown.Store(cacheKeyForDefaults(cmn.RateLimitScopeGlobal, ""), &defaultsCacheEntry{exists: true, userRPS: 1})

	for i := 0; i < 3; i++ {
		_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	}
	calls := db.callCount()

	if rps, tpm, _, err := svc.GetTenantRateLimit("t-1"); err != nil || rps != 5 || tpm != 500 {
		t.Fatalf("tenant last-known: %d %d %v", rps, tpm, err)
	}
	if tpm, err := svc.GetTenantModelRateLimit("t-1", "m1"); err != nil || tpm != 50 {
		t.Fatalf("tenant-model last-known: %d %v", tpm, err)
	}
	if rps, b, tpm, err := svc.GetUserRateLimit("t-1", "u-1"); err != nil || rps != 2 || b != 4 || tpm != 20 {
		t.Fatalf("user last-known: %d %d %d %v", rps, b, tpm, err)
	}
	if tpm, err := svc.GetUserModelRateLimit("t-1", "u-1", "m1"); err != nil || tpm != 10 {
		t.Fatalf("user-model last-known: %d %v", tpm, err)
	}
	if d, ok, err := svc.GetRateLimitDefaults(cmn.RateLimitScopeGlobal, ""); err != nil || !ok || d.DefaultUserRPS != 1 {
		t.Fatalf("defaults last-known: %+v %v %v", d, ok, err)
	}
	if k, err := svc.GetAPIKeyByID("kid-1"); err != nil || k.RateLimitRPS != 7 {
		t.Fatalf("key last-known: %+v %v", k, err)
	}
	// Never described: refused.
	if _, _, _, err := svc.GetTenantRateLimit("t-never"); !errors.Is(err, ErrDBUnavailable) {
		t.Fatalf("never-seen tenant under an open circuit = %v", err)
	}
	if _, err := svc.GetAPIKeyByID("kid-never"); !errors.Is(err, ErrDBUnavailable) {
		t.Fatalf("never-seen key under an open circuit = %v", err)
	}
	if _, err := svc.ValidateAPIKey("other-key"); !errors.Is(err, ErrDBUnavailable) {
		t.Fatalf("miss under an open circuit = %v", err)
	}
	if db.callCount() != calls {
		t.Fatalf("%d statements reached the store under an open circuit", db.callCount()-calls)
	}

	// A revocation takes the by-ID view with it.
	svc.evict(KeyInvalidation{KeyHash: hashKey("live-key"), KeyID: "kid-1"})
	if _, ok := svc.lastKnownKeySummary("kid-1"); ok {
		t.Fatal("the last-known key view outlived the key's eviction")
	}
	if _, err := svc.GetAPIKeyByID("kid-1"); !errors.Is(err, ErrDBUnavailable) {
		t.Fatalf("a revoked key was served from last-known: %v", err)
	}
}

// A store verdict that the row is gone removes the last-known view too.
func TestKeyNotFoundForgetsTheLastKnownView(t *testing.T) {
	db := newScriptDB(t)
	svc, _ := guardedService(t, db, 4, 3, 1000)
	svc.rlLastKnown.Store(cacheKeyForID("kid-9"), &cmn.ApiKeySummary{KeyID: "kid-9", TokensPerMin: 1})
	db.push(scriptAnswer{noRows: true})
	if _, err := svc.GetAPIKeyByID("kid-9"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("no-row answer = %v", err)
	}
	if _, ok := svc.lastKnownKeySummary("kid-9"); ok {
		t.Fatal("the last-known view outlived the store's no-row verdict")
	}
}

// 7. Negative cache: a hash the store said does not exist is refused without
// asking again until the TTL; creating the key clears it; the cap holds.
func TestNegativeCacheBoundsRepeatedUnknownKeys(t *testing.T) {
	db := newScriptDB(t)
	t.Setenv(NegativeCacheEnv, "2000")
	t.Setenv(NegativeCacheCapEnv, "3")
	svc, clk := guardedService(t, db, 4, 3, 1000)
	db.always(scriptAnswer{noRows: true})

	for i := 0; i < 5; i++ {
		if _, err := svc.ValidateAPIKey("ghost-key"); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("unknown key = %v", err)
		}
	}
	if db.callCount() != 1 {
		t.Fatalf("the store was asked %d times for one unknown hash", db.callCount())
	}
	clk.advance(2001 * time.Millisecond)
	_, _ = svc.ValidateAPIKey("ghost-key")
	if db.callCount() != 2 {
		t.Fatalf("the negative entry did not expire: calls=%d", db.callCount())
	}

	// The key comes into existence here or on a peer.
	svc.forgetNegative(hashKey("ghost-key"))
	_, _ = svc.ValidateAPIKey("ghost-key")
	if db.callCount() != 3 {
		t.Fatal("a cleared negative entry still refused the key")
	}
	svc.evict(KeyInvalidation{KeyHash: hashKey("ghost-key")})
	_, _ = svc.ValidateAPIKey("ghost-key")
	if db.callCount() != 4 {
		t.Fatal("a peer invalidation did not clear the negative entry")
	}

	for _, k := range []string{"g1", "g2", "g3", "g4", "g5"} {
		_, _ = svc.ValidateAPIKey(k)
	}
	if n := svc.negativeLen(); n != 3 {
		t.Fatalf("negative cache holds %d entries, cap is 3", n)
	}
	if st := svc.LookupStats(); st.Count[resultMissInvalid] != uint64(db.callCount()) {
		t.Fatalf("miss_invalid=%d, store calls=%d", st.Count[resultMissInvalid], db.callCount())
	}
}

// The negative cache is off at TTL 0.
func TestNegativeCacheDisabledAtZero(t *testing.T) {
	db := newScriptDB(t)
	t.Setenv(NegativeCacheEnv, "0")
	svc, _ := guardedService(t, db, 4, 3, 1000)
	db.always(scriptAnswer{noRows: true})
	_, _ = svc.ValidateAPIKey("ghost-key")
	_, _ = svc.ValidateAPIKey("ghost-key")
	if db.callCount() != 2 {
		t.Fatalf("negative cache active at TTL 0: calls=%d", db.callCount())
	}
}

// 8. Statistics: counts per result, cumulative monotonic buckets ending at
// the count, gauges tracking the state.
func TestLookupStatsAreConsistent(t *testing.T) {
	db := newScriptDB(t)
	svc, _ := guardedService(t, db, 4, 3, 1000)
	_ = svc.dataPathRead(outcome(nil))
	_ = svc.dataPathRead(outcome(errors.New("connection reset")))
	_ = svc.dataPathRead(outcome(sql.ErrNoRows)) // an answer: resets the count
	_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	st := svc.LookupStats()
	want := map[lookupResult]uint64{resultMissFound: 1, resultMissInvalid: 1, resultUnavailable: 1, resultTimeout: 2}
	for r, n := range want {
		if st.Count[r] != n {
			t.Fatalf("%s = %d, want %d", LookupResultNames[r], st.Count[r], n)
		}
	}
	if st.CircuitOpen {
		t.Fatal("two failures after a reset opened the circuit")
	}
	for r := range st.Buckets {
		prev := uint64(0)
		for i, c := range st.Buckets[r] {
			if c < prev {
				t.Fatalf("%s bucket %d not cumulative: %v", LookupResultNames[r], i, st.Buckets[r])
			}
			prev = c
		}
		if prev > st.Count[r] {
			t.Fatalf("%s buckets exceed the count: %v > %d", LookupResultNames[r], prev, st.Count[r])
		}
	}
	if st.InFlight != 0 {
		t.Fatalf("in-flight = %d after every read returned", st.InFlight)
	}
	_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	if st := svc.LookupStats(); !st.CircuitOpen {
		t.Fatal("CircuitOpen did not follow the state")
	}
}

// 9. The management plane bypasses the circuit: its calls reach the store
// while the data path is refused.
func TestManagementCallsBypassTheCircuit(t *testing.T) {
	db := newScriptDB(t)
	svc, _ := guardedService(t, db, 4, 3, 1000)
	for i := 0; i < 3; i++ {
		_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	}
	calls := db.callCount()
	db.push(scriptAnswer{values: []driver.Value{int64(3)}})
	n, err := svc.KnownTenantCount(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("management read under an open data-path circuit: n=%d err=%v", n, err)
	}
	if db.callCount() != calls+1 {
		t.Fatal("the management read did not reach the store")
	}
}

// With the circuit disabled (0 failures) every read is admitted.
func TestCircuitDisabledAtZeroFailures(t *testing.T) {
	db := newScriptDB(t)
	svc, _ := guardedService(t, db, 4, 0, 1000)
	for i := 0; i < 10; i++ {
		_ = svc.dataPathRead(outcome(context.DeadlineExceeded))
	}
	if stateOf(svc) != circuitClosed {
		t.Fatal("a disabled circuit opened")
	}
}
