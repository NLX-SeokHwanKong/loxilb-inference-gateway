package user

// Stall tests for the management store: every double blocks until the
// context it was handed is cancelled, as a store that keeps TCP open and
// never answers does. The properties: an operation returns inside the
// operation budget as a whole -- retries included -- a readiness ping
// inside the ping budget, a statement the server may still apply is never
// re-sent, and the error says which of those it was.

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"

	cmn "github.com/loxilb-io/loxilb/common"
	"github.com/loxilb-io/loxilb/pkg/pgstore"
)

type stallHandle struct {
	rows      *sql.DB
	mu        sync.Mutex
	calls     int
	deadlines []time.Duration // remaining budget at each call; negative = already expired
	undated   int             // calls that carried no deadline at all
}

func newStallHandle(t *testing.T) *stallHandle {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://nobody:x@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("open the row minter: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &stallHandle{rows: db}
}

func (h *stallHandle) wait(ctx context.Context) error {
	h.mu.Lock()
	h.calls++
	if dl, ok := ctx.Deadline(); ok {
		h.deadlines = append(h.deadlines, time.Until(dl))
	} else {
		h.undated++
	}
	h.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

func (h *stallHandle) ExecContext(ctx context.Context, _ string, _ ...any) (sql.Result, error) {
	return nil, h.wait(ctx)
}
func (h *stallHandle) QueryContext(ctx context.Context, _ string, _ ...any) (*sql.Rows, error) {
	return nil, h.wait(ctx)
}
func (h *stallHandle) QueryRowContext(ctx context.Context, _ string, _ ...any) *sql.Row {
	_ = h.wait(ctx)
	return h.rows.QueryRowContext(ctx, "SELECT 1")
}
func (h *stallHandle) PingContext(ctx context.Context) error { return h.wait(ctx) }

func (h *stallHandle) stats() (calls, undated int, deadlines []time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, h.undated, append([]time.Duration(nil), h.deadlines...)
}

func stalledUserService(t *testing.T) (*UserService, *stallHandle) {
	t.Helper()
	h := newStallHandle(t)
	svc := &UserService{Cache: cache.New(time.Minute, time.Minute)}
	svc.Attach(h)
	return svc, h
}

func within(t *testing.T, what string, took, budget time.Duration) {
	t.Helper()
	if took > budget+500*time.Millisecond {
		t.Fatalf("%s took %v, budget %v", what, took, budget)
	}
	if took < budget/2 {
		t.Fatalf("%s returned after %v, before the %v budget could expire: the double did not block", what, took, budget)
	}
}

// The whole operation, retries and all, fits in one operation budget. The
// retry profile alone is three attempts with 300 ms doubling sleeps; a
// stall used to cost three full waits plus the sleeps.
func TestManagementOperationsAnswerInsideOneOperationBudget(t *testing.T) {
	const budget = 200 * time.Millisecond
	t.Setenv(pgstore.OpTimeoutEnv, "200")
	// AddUser hashes the password before its first statement, and bcrypt at
	// the production cost is CPU work the budget does not govern: under the
	// race detector on a small runner it alone takes longer than the budget.
	// Measure it here, in the same process, and credit that one operation
	// with it, so the wall-clock check still holds the store wait to one
	// budget without timing the hash.
	hashStart := time.Now()
	if _, err := hashUserPassword("Correct-Horse-9!"); err != nil {
		t.Fatalf("hash the probe password: %v", err)
	}
	hashCost := time.Since(hashStart)
	ops := []struct {
		name        string
		hashesFirst bool
		call        func(context.Context, *UserService) error
	}{
		{"AddUser", true, func(ctx context.Context, s *UserService) error {
			_, err := s.AddUser(ctx, cmn.User{Username: "alice", Password: "Correct-Horse-9!", Role: "admin", CreatedAt: time.Now()})
			return err
		}},
		{"BootstrapUser", false, func(ctx context.Context, s *UserService) error {
			_, err := s.BootstrapUser(ctx, cmn.User{Username: "alice", Password: "Correct-Horse-9!", Role: "admin", CreatedAt: time.Now()})
			return err
		}},
		{"GetUsers", false, func(ctx context.Context, s *UserService) error { _, err := s.GetUsers(ctx); return err }},
		{"DelegationAllowed", false, func(ctx context.Context, s *UserService) error {
			_, err := s.DelegationAllowed(ctx, "alice")
			return err
		}},
		{"DeleteUser", false, func(ctx context.Context, s *UserService) error { return s.DeleteUser(ctx, 7) }},
		{"UpdateUser", false, func(ctx context.Context, s *UserService) error {
			return s.UpdateUser(ctx, cmn.User{ID: 7, Username: "alice", Password: "Correct-Horse-9!"})
		}},
		{"Login", false, func(ctx context.Context, s *UserService) error { _, _, err := s.Login(ctx, "alice", "pw"); return err }},
		{"Logout", false, func(ctx context.Context, s *UserService) error { return s.Logout(ctx, "tok") }},
		{"ValidateToken", false, func(ctx context.Context, s *UserService) error { _, err := s.ValidateToken(ctx, "tok"); return err }},
	}
	for _, tc := range ops {
		t.Run(tc.name, func(t *testing.T) {
			svc, h := stalledUserService(t)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			allowed := budget
			if tc.hashesFirst {
				allowed += hashCost
			}
			start := time.Now()
			err := tc.call(ctx, svc)
			within(t, tc.name, time.Since(start), allowed)
			if err == nil {
				t.Fatal("a stalled store answered success")
			}
			if !errors.Is(err, cmn.ErrDBUnavailable) {
				t.Fatalf("stall with nothing sent reported as %v, want the store-unavailable sentinel", err)
			}
			if errors.Is(err, cmn.ErrStoreOutcomeUnknown) {
				t.Fatalf("nothing was sent, yet reported unknown: %v", err)
			}
			calls, undated, dls := h.stats()
			if calls == 0 {
				t.Fatal("the store was never asked")
			}
			if undated != 0 {
				t.Fatalf("%d statement(s) reached the store with no deadline", undated)
			}
			// A deadline already in the past is fine (the password hash
			// can spend the budget before the first statement under the
			// race detector); one beyond the budget is not.
			for _, dl := range dls {
				if dl > budget+50*time.Millisecond {
					t.Fatalf("a statement carried deadline %v, budget %v", dl, budget)
				}
			}
		})
	}
}

func TestReadyAnswersInsideThePingBudget(t *testing.T) {
	const budget = 100 * time.Millisecond
	t.Setenv(pgstore.PingTimeoutEnv, "100")
	t.Setenv(pgstore.OpTimeoutEnv, "3000")
	svc, h := stalledUserService(t)
	start := time.Now()
	err := svc.Ready(context.Background())
	within(t, "Ready", time.Since(start), budget)
	if !errors.Is(err, cmn.ErrDBUnavailable) {
		t.Fatalf("Ready against a stall = %v", err)
	}
	if _, undated, dls := h.stats(); undated != 0 || len(dls) != 1 || dls[0] > budget+50*time.Millisecond {
		t.Fatalf("ping deadline %v (undated %d), budget %v", dls, undated, budget)
	}
}

// RetryOperation stops at the deadline: no attempt and no sleep after it,
// and the answer is classified rather than the last transient error.
func TestRetryOperationStopsAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	attempts := 0
	start := time.Now()
	err := RetryOperation(ctx, func() error {
		attempts++
		return errors.New("transient")
	}, AuthMaxRetries, AuthRetryDelay, retryableDBError)
	took := time.Since(start)
	if took > 600*time.Millisecond {
		t.Fatalf("retry loop outlived the deadline: %v", took)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d; the first 300 ms sleep already crosses the 100 ms deadline", attempts)
	}
	if !errors.Is(err, cmn.ErrDBUnavailable) {
		t.Fatalf("deadline reported as %v", err)
	}
}

// A statement the server may still be applying is never sent again: an
// outcome-unknown error ends the retry loop at once and is passed through.
func TestRetryWriteNeverRepeatsAnUnknownOutcome(t *testing.T) {
	attempts := 0
	start := time.Now()
	err := retryWrite(context.Background(), func() error {
		attempts++
		return pgstore.ClassifyWriteErr(errors.New("socket went quiet after the send"))
	})
	if attempts != 1 {
		t.Fatalf("an outcome-unknown write was re-sent: %d attempts", attempts)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("retryWrite slept after an outcome-unknown error")
	}
	if !errors.Is(err, cmn.ErrStoreOutcomeUnknown) || errors.Is(err, cmn.ErrDBUnavailable) {
		t.Fatalf("outcome unknown reported as %v", err)
	}
}

// A retried write that keeps failing transiently is reported the way a
// write must be: through the write classifier, not as a generic outage
// wording that a caller could mistake for a verdict.
func TestRetryWriteClassifiesTheFinalError(t *testing.T) {
	err := retryWrite(context.Background(), func() error { return context.DeadlineExceeded })
	if !errors.Is(err, cmn.ErrDBUnavailable) {
		t.Fatalf("bare deadline (nothing sent) reported as %v", err)
	}
}
