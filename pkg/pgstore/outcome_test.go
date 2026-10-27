package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pgxconn "github.com/jackc/pgx/v5/pgconn"

	cmn "github.com/loxilb-io/loxilb/common"
)

// The classification contract, on the error shapes that can be built
// without a driver. The shapes only the driver produces are covered below
// against the wire-protocol fake.
func TestClassifyWriteErrShapes(t *testing.T) {
	pgErr := &pgxconn.PgError{Code: "23505"}
	cases := []struct {
		name        string
		err         error
		wantUnknown bool
		wantUnavail bool
		passThrough bool
	}{
		{"nil", nil, false, false, true},
		{"server verdict", pgErr, false, false, true},
		{"wrapped server verdict", fmt.Errorf("insert: %w", pgErr), false, false, true},
		{"no rows", sql.ErrNoRows, false, false, true},
		{"already unavailable", fmt.Errorf("%w: x", cmn.ErrDBUnavailable), false, true, true},
		{"already unknown", fmt.Errorf("%w: x", cmn.ErrStoreOutcomeUnknown), true, false, true},

		// Nothing was sent: safe to retry.
		{"bad conn after the pool's retries", driver.ErrBadConn, false, true, false},
		{"conn done", sql.ErrConnDone, false, true, false},
		{"tx done", sql.ErrTxDone, false, true, false},
		{"dial failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, false, true, false},
		{"pool wait deadline (bare)", context.DeadlineExceeded, false, true, false},
		{"wrapped pool wait deadline", fmt.Errorf("exec: %w", context.DeadlineExceeded), false, true, false},

		// Sent, or not known not to have been: outcome unknown.
		{"cancelled (pgconn returns it bare after a send too)", context.Canceled, true, false, false},
		{"read failure", &net.OpError{Op: "read", Err: errors.New("i/o timeout")}, true, false, false},
		{"connection dropped mid-exchange", io.ErrUnexpectedEOF, true, false, false},
		{"plain error of unknown shape", errors.New("something"), true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyWriteErr(tc.err)
			if tc.passThrough && !errors.Is(got, tc.err) && !(tc.err == nil && got == nil) {
				t.Fatalf("passthrough lost: got %v", got)
			}
			if tc.err != nil && !errors.Is(got, tc.err) {
				t.Fatalf("cause dropped from the chain: got %v", got)
			}
			if errors.Is(got, cmn.ErrStoreOutcomeUnknown) != tc.wantUnknown {
				t.Fatalf("outcome unknown = %v, want %v (got %v)", !tc.wantUnknown, tc.wantUnknown, got)
			}
			if errors.Is(got, cmn.ErrDBUnavailable) != tc.wantUnavail {
				t.Fatalf("unavailable = %v, want %v (got %v)", !tc.wantUnavail, tc.wantUnavail, got)
			}
			// A write error is never both.
			if errors.Is(got, cmn.ErrStoreOutcomeUnknown) && errors.Is(got, cmn.ErrDBUnavailable) {
				t.Fatalf("classified as both: %v", got)
			}
		})
	}
}

// A read has no outcome to be unsure about: every transport failure,
// including the post-send timeout, is "unavailable".
func TestClassifyReadErrNeverReportsUnknown(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, context.Canceled, io.ErrUnexpectedEOF,
		driver.ErrBadConn, &net.OpError{Op: "read", Err: errors.New("x")}} {
		got := ClassifyReadErr(err)
		if !errors.Is(got, cmn.ErrDBUnavailable) || errors.Is(got, cmn.ErrStoreOutcomeUnknown) {
			t.Errorf("ClassifyReadErr(%v) = %v", err, got)
		}
	}
	pgErr := &pgxconn.PgError{Code: "42P01"}
	if got := ClassifyReadErr(pgErr); got != pgErr {
		t.Errorf("server verdict reclassified: %v", got)
	}
	if got := ClassifyReadErr(sql.ErrNoRows); got != sql.ErrNoRows {
		t.Errorf("no-rows reclassified: %v", got)
	}
}

func openFake(t *testing.T, f *pgfake) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", f.DSN())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func execWithBudget(db *sql.DB, budget time.Duration, stmt string) (error, time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	start := time.Now()
	_, err := db.ExecContext(ctx, stmt)
	return err, time.Since(start)
}

// Shape (b) of the request: the statement is in flight when the server
// stops answering. The driver reports its timeout wrapper, the classifier
// says outcome unknown, the call returns inside the budget, and the stalled
// connection is dropped from the pool rather than handed to the next caller.
func TestStallMidStatementIsOutcomeUnknownAndEvictsTheConnection(t *testing.T) {
	f := newPgfake(t)
	db := openFake(t, f)
	// Two warm-ups, not one: database/sql resets a pooled connection before
	// every reuse, and the driver's reset pings a connection it has not
	// reset within the last second -- which a brand-new connection never
	// was. The first reuse therefore always pings first, and against a
	// stalled server that ping, not the statement, is what times out
	// (the shape the next test covers). The second warm-up is that first
	// reuse; the connection it leaves behind was reset just now, so the
	// statement under test goes straight out.
	for i := 1; i <= 2; i++ {
		if err, _ := execWithBudget(db, 2*time.Second, fmt.Sprintf("INSERT INTO t VALUES (%d)", i)); err != nil {
			t.Fatalf("warm-up exec %d: %v", i, err)
		}
	}
	if db.Stats().Idle != 1 {
		t.Fatalf("want one warm idle connection, have %+v", db.Stats())
	}

	f.stalled.Store(true)
	const budget = 300 * time.Millisecond
	err, took := execWithBudget(db, budget, "INSERT INTO t VALUES (3)")
	if err == nil {
		t.Fatal("exec against a stalled server succeeded")
	}
	if took > budget+500*time.Millisecond {
		t.Fatalf("exec took %v, budget %v", took, budget)
	}
	if !pgxconn.Timeout(err) {
		for e := err; e != nil; e = errors.Unwrap(e) {
			t.Logf("chain: %T: %v", e, e)
		}
		t.Logf("server saw: %q", f.Received())
		t.Fatalf("driver error is not pgconn's timeout shape: %T %v", err, err)
	}
	got := ClassifyWriteErr(err)
	if !errors.Is(got, cmn.ErrStoreOutcomeUnknown) || errors.Is(got, cmn.ErrDBUnavailable) {
		t.Fatalf("in-flight stall classified %v, want outcome unknown", got)
	}
	// The statement really went out: the server saw it.
	rcv := f.Received()
	if !strings.Contains(strings.Join(rcv, "\n"), "VALUES (3)") {
		t.Fatalf("server never received the statement: %q", rcv)
	}
	// The pool-eviction hook. pgconn closed the socket on the timeout; the
	// stdlib driver reports the dead connection as ErrBadConn at its next
	// checkout and database/sql discards it and retries on a fresh one. So
	// the next statement, once the server answers again, succeeds without
	// the caller ever seeing the corpse -- and the corpse is gone.
	f.stalled.Store(false)
	if err, took := execWithBudget(db, 2*time.Second, "INSERT INTO t VALUES (4)"); err != nil || took > time.Second {
		t.Fatalf("first statement after the stall: err=%v took=%v (the dead connection was handed back)", err, took)
	}
	if st := db.Stats(); st.OpenConnections != 1 {
		t.Fatalf("pool after recovery: %+v, want exactly the fresh connection", st)
	}
	// And the dead connection's startup was never replayed as a statement:
	// the server saw a second startup (the fresh dial) and the new insert.
	rcv = f.Received()
	if !strings.Contains(strings.Join(rcv, "\n"), "VALUES (4)") {
		t.Fatalf("post-recovery statement missing from the server log: %q", rcv)
	}
}

// Shape (a): a warm connection that has been idle for more than a second
// is pinged by the driver before reuse. Against a stalled server that ping
// absorbs the whole budget, so the statement is never sent -- and the
// classifier must say so (nothing sent, safe to retry), not "unknown".
func TestWarmIdleConnectionPingAbsorbsTheBudgetAsNotSent(t *testing.T) {
	f := newPgfake(t)
	db := openFake(t, f)
	if err, _ := execWithBudget(db, 2*time.Second, "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("warm-up exec: %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // past the driver's reset-session ping threshold
	f.stalled.Store(true)

	const budget = 300 * time.Millisecond
	err, took := execWithBudget(db, budget, "INSERT INTO t VALUES (2)")
	if err == nil {
		t.Fatal("exec against a stalled server succeeded")
	}
	if took > budget+500*time.Millisecond {
		t.Fatalf("exec took %v, budget %v", took, budget)
	}
	got := ClassifyWriteErr(err)
	if !errors.Is(got, cmn.ErrDBUnavailable) || errors.Is(got, cmn.ErrStoreOutcomeUnknown) {
		t.Fatalf("ping-absorbed budget classified %v, want unavailable (nothing sent)", got)
	}
	for _, m := range f.Received() {
		if strings.Contains(m, "VALUES (2)") {
			t.Fatalf("the statement reached the server although the classifier said not sent: %q", f.Received())
		}
	}
}

// Shape (c): nothing answers the dial's startup either. A cold outage:
// nothing sent, 503.
func TestStallOnConnectIsNotSent(t *testing.T) {
	f := newPgfake(t)
	f.stalled.Store(true)
	db := openFake(t, f)
	const budget = 300 * time.Millisecond
	err, took := execWithBudget(db, budget, "INSERT INTO t VALUES (1)")
	if err == nil {
		t.Fatal("exec against a server that never answers the startup succeeded")
	}
	if took > budget+500*time.Millisecond {
		t.Fatalf("exec took %v, budget %v", took, budget)
	}
	if got := ClassifyWriteErr(err); !errors.Is(got, cmn.ErrDBUnavailable) || errors.Is(got, cmn.ErrStoreOutcomeUnknown) {
		t.Fatalf("connect stall classified %v, want unavailable", got)
	}
	if got := ClassifyReadErr(err); !errors.Is(got, cmn.ErrDBUnavailable) {
		t.Fatalf("read classification of a connect stall: %v", got)
	}
}

// The pool itself can be the wait: every connection busy, the caller's
// deadline passes before one is free. database/sql returns the bare context
// error; nothing was sent.
func TestPoolWaitTimeoutIsNotSent(t *testing.T) {
	f := newPgfake(t)
	db := openFake(t, f)
	db.SetMaxOpenConns(1)
	held, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("hold a connection: %v", err)
	}
	defer held.Close()

	const budget = 100 * time.Millisecond
	err, took := execWithBudget(db, budget, "INSERT INTO t VALUES (1)")
	if !errors.Is(err, context.DeadlineExceeded) || pgxconn.Timeout(err) {
		t.Fatalf("pool wait returned %T %v, want the bare context error", err, err)
	}
	if took > budget+500*time.Millisecond {
		t.Fatalf("exec took %v, budget %v", took, budget)
	}
	if got := ClassifyWriteErr(err); !errors.Is(got, cmn.ErrDBUnavailable) {
		t.Fatalf("pool wait classified %v, want unavailable", got)
	}
}

// PingContext under a budget against a stalled server returns inside it.
func TestPingHonoursTheBudgetAgainstAStall(t *testing.T) {
	f := newPgfake(t)
	db := openFake(t, f)
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("warm-up ping: %v", err)
	}
	f.stalled.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := db.PingContext(ctx)
	if err == nil || time.Since(start) > 700*time.Millisecond {
		t.Fatalf("ping = %v after %v", err, time.Since(start))
	}
	if got := ClassifyReadErr(err); !errors.Is(got, cmn.ErrDBUnavailable) {
		t.Fatalf("stalled ping classified %v", got)
	}
}

// The server-side second line: statement_timeout rides in the startup
// packet as a run-time parameter, equal to the operation budget.
func TestStatementTimeoutIsSentInTheStartupPacket(t *testing.T) {
	t.Setenv(OpTimeoutEnv, "4321")
	f := newPgfake(t)
	db := openFake(t, f)
	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	st := f.Startups()
	if len(st) != 1 {
		t.Fatalf("want one startup packet, got %d", len(st))
	}
	if st[0]["statement_timeout"] != "4321" {
		t.Fatalf("statement_timeout in the startup packet = %q, want 4321 (packet %v)", st[0]["statement_timeout"], st[0])
	}
	cfg, err := pgx.ParseConfig(f.DSN())
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.RuntimeParams["statement_timeout"] != strconv.Itoa(4321) {
		t.Fatalf("pgx did not take statement_timeout as a run-time parameter: %v", cfg.RuntimeParams)
	}
}

func TestBudgetsFromEnv(t *testing.T) {
	t.Setenv(OpTimeoutEnv, "")
	t.Setenv(PingTimeoutEnv, "")
	t.Setenv(RefreshTimeoutEnv, "")
	t.Setenv(LookupTimeoutEnv, "")
	if OpTimeout() != 3000*time.Millisecond || PingTimeout() != 1000*time.Millisecond ||
		RefreshTimeout() != 3000*time.Millisecond || LookupTimeout() != 300*time.Millisecond {
		t.Fatalf("defaults: op=%v ping=%v refresh=%v lookup=%v", OpTimeout(), PingTimeout(), RefreshTimeout(), LookupTimeout())
	}
	t.Setenv(LookupTimeoutEnv, "250")
	if LookupTimeout() != 250*time.Millisecond {
		t.Fatalf("env not honoured: %v", LookupTimeout())
	}
	for _, bad := range []string{"0", "-5", "x", "1"} {
		t.Setenv(PingTimeoutEnv, bad)
		if got := PingTimeout(); got < minBudget {
			t.Fatalf("%q gave a budget below the floor: %v", bad, got)
		}
	}
	t.Setenv(PingTimeoutEnv, "1")
	if PingTimeout() != minBudget {
		t.Fatalf("floor not applied: %v", PingTimeout())
	}
}
