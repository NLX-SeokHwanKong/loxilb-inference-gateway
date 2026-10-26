package aikey

// Stall tests: the store accepts every statement and never answers. Every
// double here BLOCKS until the context it was handed is cancelled -- a
// double that returned an error at once would prove nothing about the
// budget, which is the property under test: that a management call, a
// readiness probe, a snapshot refresh and a data-path lookup each return
// inside their own budget, and classify what happened, while the store
// keeps every connection open and says nothing.

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"

	cmn "github.com/loxilb-io/loxilb/common"
	"github.com/loxilb-io/loxilb/pkg/pgstore"
)

// stallDB is a DBTX whose every method waits for its context to end and
// then answers the way database/sql answers a caller whose context expired
// while it waited for a connection: the bare context error, nothing sent.
// It records the deadline each call carried, so a test can assert the
// budget the service applied rather than only the time it took.
type stallDB struct {
	// rows mints *sql.Row values: it is a pool that is never dialled (an
	// expired context never reaches the driver), opened against nothing.
	rows *sql.DB

	mu        sync.Mutex
	calls     int
	deadlines []time.Duration // remaining budget as seen at each call
	cancelled atomic.Int32
}

func newStallDB(t *testing.T) *stallDB {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://nobody:x@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("open the row minter: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &stallDB{rows: db}
}

func (d *stallDB) note(ctx context.Context) {
	d.mu.Lock()
	d.calls++
	if dl, ok := ctx.Deadline(); ok {
		d.deadlines = append(d.deadlines, time.Until(dl))
	} else {
		d.deadlines = append(d.deadlines, -1)
	}
	d.mu.Unlock()
}

func (d *stallDB) wait(ctx context.Context) error {
	d.note(ctx)
	<-ctx.Done()
	d.cancelled.Add(1)
	return ctx.Err()
}

func (d *stallDB) ExecContext(ctx context.Context, _ string, _ ...any) (sql.Result, error) {
	return nil, d.wait(ctx)
}
func (d *stallDB) QueryContext(ctx context.Context, _ string, _ ...any) (*sql.Rows, error) {
	return nil, d.wait(ctx)
}
func (d *stallDB) QueryRowContext(ctx context.Context, _ string, _ ...any) *sql.Row {
	_ = d.wait(ctx)
	// ctx is done: database/sql hands back a row carrying ctx.Err() without
	// touching the driver.
	return d.rows.QueryRowContext(ctx, "SELECT 1")
}
func (d *stallDB) PingContext(ctx context.Context) error { return d.wait(ctx) }

func (d *stallDB) snapshot() (calls int, deadlines []time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls, append([]time.Duration(nil), d.deadlines...)
}

func stalledService(t *testing.T) (*Service, *stallDB) {
	t.Helper()
	db := newStallDB(t)
	svc := &Service{db: db, Cache: cache.New(CacheExpirationTime*time.Minute, CacheCleanupInterval*time.Minute)}
	return svc, db
}

// elapsedWithin fails when a call took longer than budget plus a margin for
// the scheduler, or returned before the budget could have expired (which
// would mean the double did not block).
func elapsedWithin(t *testing.T, what string, took, budget time.Duration) {
	t.Helper()
	if took > budget+400*time.Millisecond {
		t.Fatalf("%s took %v, budget %v: the store stall was not bounded", what, took, budget)
	}
	if took < budget/2 {
		t.Fatalf("%s returned after %v, before the %v budget: the double did not block", what, took, budget)
	}
}

func assertDeadlineAtMost(t *testing.T, db *stallDB, budget time.Duration) {
	t.Helper()
	calls, dls := db.snapshot()
	if calls == 0 {
		t.Fatal("the store was never asked")
	}
	for _, dl := range dls {
		if dl < 0 {
			t.Fatal("a statement reached the store with no deadline")
		}
		if dl > budget+50*time.Millisecond {
			t.Fatalf("a statement carried a %v deadline, budget is %v", dl, budget)
		}
	}
}

// Every management write and read answers inside the operation budget, and
// a stall the pool never got a statement out for is reported as the store
// being unavailable -- never as a server verdict, never as success.
func TestManagementCallsAnswerInsideTheOperationBudget(t *testing.T) {
	const budget = 150 * time.Millisecond
	t.Setenv(pgstore.OpTimeoutEnv, "150")
	t.Setenv(pgstore.RefreshTimeoutEnv, "150")
	one := 1
	enabled := true
	calls := []struct {
		name string
		call func(context.Context, *Service) error
	}{
		{"CreateAPIKey", func(ctx context.Context, s *Service) error {
			_, _, err := s.CreateAPIKey(ctx, cmn.ApiKeyEntry{TenantID: "t", Name: "k", Enabled: true})
			return err
		}},
		{"RevokeAPIKey", func(ctx context.Context, s *Service) error { return s.RevokeAPIKey(ctx, "k") }},
		{"DeleteAPIKey", func(ctx context.Context, s *Service) error { return s.DeleteAPIKey(ctx, "k") }},
		{"PatchAPIKey", func(ctx context.Context, s *Service) error { return s.PatchAPIKey(ctx, "k", nil, &enabled) }},
		{"PatchAPIKeyRateLimits", func(ctx context.Context, s *Service) error {
			return s.PatchAPIKeyRateLimits(ctx, "k", &one, nil, nil)
		}},
		{"SetTenantRateLimit", func(ctx context.Context, s *Service) error { return s.SetTenantRateLimit(ctx, "t", 1, 1, 0) }},
		{"SetTenantModelRateLimit", func(ctx context.Context, s *Service) error {
			return s.SetTenantModelRateLimit(ctx, "t", "m", 1)
		}},
		{"SetObjective", func(ctx context.Context, s *Service) error {
			return s.SetObjective(ctx, cmn.AIObjectiveEntry{Name: "o", Priority: 1})
		}},
		{"DeleteObjective", func(ctx context.Context, s *Service) error { return s.DeleteObjective(ctx, "o") }},
		{"SetTenantClass", func(ctx context.Context, s *Service) error {
			return s.SetTenantClass(ctx, "t", cmn.AIClassPolicy{PriorityClass: cmn.AIClassStandard})
		}},
		{"SetAPIKeyClass", func(ctx context.Context, s *Service) error {
			return s.SetAPIKeyClass(ctx, "k", cmn.AIClassPolicy{PriorityClass: cmn.AIClassStandard})
		}},
		{"PatchTenantClass", func(ctx context.Context, s *Service) error {
			pc := cmn.AIClassStandard
			return s.PatchTenantClass(ctx, "t", cmn.AIClassPatch{PriorityClass: &pc})
		}},
		{"PatchAPIKeyClass", func(ctx context.Context, s *Service) error {
			pc := cmn.AIClassStandard
			return s.PatchAPIKeyClass(ctx, "k", cmn.AIClassPatch{PriorityClass: &pc})
		}},
		{"SetUserRateLimit", func(ctx context.Context, s *Service) error {
			return s.SetUserRateLimit(ctx, cmn.UserRateLimitEntry{TenantID: "t", UserID: "u", RPS: 1})
		}},
		{"DeleteUserRateLimit", func(ctx context.Context, s *Service) error { return s.DeleteUserRateLimit(ctx, "t", "u") }},
		{"SetRateLimitDefaults", func(ctx context.Context, s *Service) error {
			return s.SetRateLimitDefaults(ctx, cmn.RateLimitDefaultsEntry{Scope: cmn.RateLimitScopeGlobal, DefaultUserRPS: 1})
		}},
		{"DeleteRateLimitDefaults", func(ctx context.Context, s *Service) error {
			return s.DeleteRateLimitDefaults(ctx, cmn.RateLimitScopeGlobal, "")
		}},
		{"ListAPIKeys", func(ctx context.Context, s *Service) error { _, err := s.ListAPIKeys(ctx, ""); return err }},
		{"GetTenantRateLimitEntry", func(ctx context.Context, s *Service) error {
			_, err := s.GetTenantRateLimitEntry(ctx, "t")
			return err
		}},
		{"ListObjectives", func(ctx context.Context, s *Service) error { _, err := s.ListObjectives(ctx); return err }},
		{"GetTenantClass", func(ctx context.Context, s *Service) error { _, err := s.GetTenantClass(ctx, "t"); return err }},
		{"ListAPIKeyClasses", func(ctx context.Context, s *Service) error { _, err := s.ListAPIKeyClasses(ctx); return err }},
		{"GetUserRateLimitEntry", func(ctx context.Context, s *Service) error {
			_, err := s.GetUserRateLimitEntry(ctx, "t", "u")
			return err
		}},
		{"ListUserRateLimits", func(ctx context.Context, s *Service) error { _, err := s.ListUserRateLimits(ctx, "t"); return err }},
		{"GetRateLimitDefaultsEntry", func(ctx context.Context, s *Service) error {
			_, err := s.GetRateLimitDefaultsEntry(ctx, cmn.RateLimitScopeGlobal, "")
			return err
		}},
		{"KnownTenantCount", func(ctx context.Context, s *Service) error { _, err := s.KnownTenantCount(ctx); return err }},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			svc, db := stalledService(t)
			// The caller's own deadline is far away: the budget must come
			// from the service, not from the test.
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			start := time.Now()
			err := tc.call(ctx, svc)
			took := time.Since(start)
			elapsedWithin(t, tc.name, took, budget)
			if err == nil {
				t.Fatal("a stalled store answered success")
			}
			if !errors.Is(err, cmn.ErrDBUnavailable) {
				t.Fatalf("stall with nothing sent reported as %v, want the store-unavailable sentinel", err)
			}
			if errors.Is(err, cmn.ErrStoreOutcomeUnknown) {
				t.Fatalf("nothing was sent, yet the outcome is reported unknown: %v", err)
			}
			assertDeadlineAtMost(t, db, budget)
		})
	}
}

// The client's own context cancels the statement: a client that hangs up
// does not leave the handler waiting out the whole budget.
func TestCallerContextCancelsAStalledWrite(t *testing.T) {
	t.Setenv(pgstore.OpTimeoutEnv, "3000")
	svc, db := stalledService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := svc.SetObjective(ctx, cmn.AIObjectiveEntry{Name: "o", Priority: 1})
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("write outlived the caller's context by %v", took)
	}
	if err == nil || db.cancelled.Load() == 0 {
		t.Fatalf("write = %v, cancellations %d", err, db.cancelled.Load())
	}
}

// Readiness answers inside the ping budget, not the operation budget, and
// reports the stall as not ready.
func TestReadyAnswersInsideThePingBudget(t *testing.T) {
	const budget = 100 * time.Millisecond
	t.Setenv(pgstore.PingTimeoutEnv, "100")
	t.Setenv(pgstore.OpTimeoutEnv, "3000")
	svc, db := stalledService(t)
	start := time.Now()
	err := svc.Ready(context.Background())
	elapsedWithin(t, "Ready", time.Since(start), budget)
	if !errors.Is(err, cmn.ErrDBUnavailable) {
		t.Fatalf("Ready against a stall = %v", err)
	}
	assertDeadlineAtMost(t, db, budget)
}

// A refresh that stalls fails inside the refresh budget, leaves the
// snapshot in force, and flips the store's health to not reachable -- so
// the objective list reports the applied snapshot with store_reachable
// false instead of hanging.
func TestRefreshStallFlipsStoreHealthAndKeepsTheSnapshot(t *testing.T) {
	const budget = 120 * time.Millisecond
	t.Setenv(pgstore.RefreshTimeoutEnv, "120")
	t.Setenv(pgstore.OpTimeoutEnv, "120")
	t.Setenv(classRefreshEnv, "10")
	svc, _ := stalledService(t)
	// A snapshot the store answered with earlier.
	applied := &ClassSnapshot{Objectives: map[string]cmn.AIObjectiveEntry{"gold": {Name: "gold", Priority: 7}},
		Tenants: map[string]cmn.AIClassPolicy{}, Keys: map[string]cmn.AIClassPolicy{},
		ReadAt: time.Now().Add(-time.Minute).UTC(), Source: cmn.AIClassSourceStore, Gen: 1}
	svc.class.readGen.Store(1)
	svc.publishClasses(applied)
	svc.noteStoreRead(1, nil)
	if ok, _ := svc.StoreHealth(); !ok {
		t.Fatal("precondition: store reachable")
	}

	start := time.Now()
	err := svc.RefreshClasses(context.Background())
	elapsedWithin(t, "RefreshClasses", time.Since(start), budget)
	if err == nil {
		t.Fatal("refresh against a stall succeeded")
	}
	if ok, _ := svc.StoreHealth(); ok {
		t.Fatal("store still reported reachable after a stalled refresh")
	}
	if svc.Classes() != applied {
		t.Fatal("the snapshot in force was replaced by a failed refresh")
	}

	start = time.Now()
	list, err := svc.ObjectiveList(context.Background())
	elapsedWithin(t, "ObjectiveList", time.Since(start), budget)
	if err != nil {
		t.Fatalf("ObjectiveList with an applied snapshot: %v", err)
	}
	if list.StoreReachable || list.Source != cmn.AIClassSourceStore || len(list.Objectives) != 1 || list.Objectives[0].Name != "gold" {
		t.Fatalf("ObjectiveList during a stall = %+v", list)
	}
}

// The refresh budget never exceeds half the refresh interval: a stalled
// read is over before the next tick.
func TestRefreshBudgetIsClampedBelowTheInterval(t *testing.T) {
	t.Setenv(pgstore.RefreshTimeoutEnv, "3000")
	t.Setenv(classRefreshEnv, "2")
	svc := &Service{}
	if got := svc.refreshBudget(); got != time.Second {
		t.Fatalf("refresh budget = %v, want 1s (half of a 2 s interval)", got)
	}
	t.Setenv(classRefreshEnv, "10")
	if got := svc.refreshBudget(); got != 3*time.Second {
		t.Fatalf("refresh budget = %v, want the configured 3s", got)
	}
}

// Data path: a cache hit never touches the stalled store; a miss is
// answered "could not ask" inside the lookup budget, with the deadline the
// store saw being that budget and not the operation budget.
func TestValidateAPIKeyMissIsBoundedByTheLookupBudgetAndHitsNeverWait(t *testing.T) {
	const budget = 80 * time.Millisecond
	t.Setenv(pgstore.LookupTimeoutEnv, "80")
	t.Setenv(pgstore.OpTimeoutEnv, "3000")
	svc, db := stalledService(t)

	cached := &cmn.ApiKeyEntry{KeyID: "cached-id", TenantID: "t", Enabled: true}
	svc.Cache.Set(cacheKeyForHash(hashKey("cached-key")), cached, time.Minute)

	start := time.Now()
	entry, err := svc.ValidateAPIKey("cached-key")
	if err != nil || entry != cached || time.Since(start) > 20*time.Millisecond {
		t.Fatalf("cache hit during a stall: entry=%v err=%v took=%v", entry, err, time.Since(start))
	}
	if calls, _ := db.snapshot(); calls != 0 {
		t.Fatal("a cache hit reached the stalled store")
	}

	start = time.Now()
	_, err = svc.ValidateAPIKey("unknown-key")
	elapsedWithin(t, "ValidateAPIKey miss", time.Since(start), budget)
	if !errors.Is(err, cmn.ErrDBUnavailable) || errors.Is(err, ErrInvalidKey) {
		t.Fatalf("miss during a stall = %v: must be the store-unavailable answer, never a verdict on the key", err)
	}
	assertDeadlineAtMost(t, db, budget)
}

// The quota reads behind a request carry the same lookup budget, and keep
// the last store-confirmed value through the stall.
func TestDataPathQuotaReadsUseTheLookupBudget(t *testing.T) {
	const budget = 80 * time.Millisecond
	t.Setenv(pgstore.LookupTimeoutEnv, "80")
	svc, db := stalledService(t)

	// Never answered for: fail closed, inside the budget.
	start := time.Now()
	_, _, _, err := svc.GetTenantRateLimit("t-never")
	elapsedWithin(t, "GetTenantRateLimit", time.Since(start), budget)
	if !errors.Is(err, cmn.ErrDBUnavailable) {
		t.Fatalf("unanswered tenant during a stall = %v", err)
	}
	assertDeadlineAtMost(t, db, budget)

	// Answered before: the last-known value, still inside the budget.
	svc.rlLastKnown.Store(cacheKeyForTenant("t-known"), &rateLimitCacheEntry{rps: 5})
	start = time.Now()
	rps, _, _, err := svc.GetTenantRateLimit("t-known")
	elapsedWithin(t, "GetTenantRateLimit (last known)", time.Since(start), budget)
	if err != nil || rps != 5 {
		t.Fatalf("last-known value during a stall: rps=%d err=%v", rps, err)
	}

	for _, read := range []struct {
		name string
		call func() error
	}{
		{"GetTenantModelRateLimit", func() error { _, err := svc.GetTenantModelRateLimit("t", "m"); return err }},
		{"GetUserRateLimit", func() error { _, _, _, err := svc.GetUserRateLimit("t", "u"); return err }},
		{"GetUserModelRateLimit", func() error { _, err := svc.GetUserModelRateLimit("t", "u", "m"); return err }},
		{"GetRateLimitDefaults", func() error { _, _, err := svc.GetRateLimitDefaults(cmn.RateLimitScopeGlobal, ""); return err }},
		{"GetAPIKeyByID", func() error { _, err := svc.GetAPIKeyByID("k"); return err }},
	} {
		start := time.Now()
		err := read.call()
		elapsedWithin(t, read.name, time.Since(start), budget)
		if !errors.Is(err, cmn.ErrDBUnavailable) {
			t.Fatalf("%s during a stall = %v", read.name, err)
		}
	}
	assertDeadlineAtMost(t, db, budget)
}
