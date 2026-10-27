package aikey

// scriptDB is a DBTX that answers each statement from a script: a stall
// that waits for the context, a "no such row", a driver error, or one row of
// values. It goes through database/sql with a fake driver so the service's
// own Scan calls see real *sql.Row behaviour (sql.ErrNoRows on an empty
// result, the context error on a stall), which the stall-only double in
// stall_test.go cannot produce.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
)

type scriptAnswer struct {
	block  bool           // wait for the context: a stalled store
	noRows bool           // the row does not exist
	err    error          // a driver-level failure
	values []driver.Value // one row
}

type scriptDB struct {
	name string
	db   *sql.DB

	mu      sync.Mutex
	answers []scriptAnswer
	repeat  *scriptAnswer // when the script runs out, answer this
	calls   int
	// stmts is every statement the driver saw, in order, with BEGIN, COMMIT
	// and ROLLBACK recorded as such: the oracle for "one transaction".
	stmts []string
	// failCommit, when set, is what the next COMMIT answers with (once).
	failCommit error
}

var (
	scriptsMu sync.Mutex
	scripts   = map[string]*scriptDB{}
	scriptSeq int
)

type scriptDriver struct{}

func (scriptDriver) Open(name string) (driver.Conn, error) {
	scriptsMu.Lock()
	d := scripts[name]
	scriptsMu.Unlock()
	if d == nil {
		return nil, fmt.Errorf("no script %q", name)
	}
	return &scriptConn{d: d}, nil
}

var registerScriptDriver sync.Once

func newScriptDB(t *testing.T) *scriptDB {
	t.Helper()
	registerScriptDriver.Do(func() { sql.Register("aikey-script", scriptDriver{}) })
	scriptsMu.Lock()
	scriptSeq++
	d := &scriptDB{name: fmt.Sprintf("script-%d", scriptSeq)}
	scripts[d.name] = d
	scriptsMu.Unlock()
	db, err := sql.Open("aikey-script", d.name)
	if err != nil {
		t.Fatalf("open the script driver: %v", err)
	}
	d.db = db
	t.Cleanup(func() {
		db.Close()
		scriptsMu.Lock()
		delete(scripts, d.name)
		scriptsMu.Unlock()
	})
	return d
}

func (d *scriptDB) push(a ...scriptAnswer) {
	d.mu.Lock()
	d.answers = append(d.answers, a...)
	d.mu.Unlock()
}

func (d *scriptDB) always(a scriptAnswer) {
	d.mu.Lock()
	d.repeat = &a
	d.mu.Unlock()
}

func (d *scriptDB) next() (scriptAnswer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if len(d.answers) > 0 {
		a := d.answers[0]
		d.answers = d.answers[1:]
		return a, nil
	}
	if d.repeat != nil {
		return *d.repeat, nil
	}
	return scriptAnswer{}, errors.New("script exhausted: the store was asked more often than the test allowed")
}

// statements returns the SQL (or BEGIN/COMMIT/ROLLBACK) in the order seen.
func (d *scriptDB) statements() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.stmts...)
}

func (d *scriptDB) record(stmt string) {
	d.mu.Lock()
	d.stmts = append(d.stmts, stmt)
	d.mu.Unlock()
}

func (d *scriptDB) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *scriptDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return d.db.ExecContext(ctx, q, args...)
}
func (d *scriptDB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return d.db.QueryContext(ctx, q, args...)
}
func (d *scriptDB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	return d.db.QueryRowContext(ctx, q, args...)
}
func (d *scriptDB) PingContext(ctx context.Context) error { return d.db.PingContext(ctx) }
func (d *scriptDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return d.db.BeginTx(ctx, opts)
}

type scriptConn struct{ d *scriptDB }

func (c *scriptConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("script driver: prepare is not scripted")
}
func (c *scriptConn) Close() error { return nil }
func (c *scriptConn) Begin() (driver.Tx, error) {
	return nil, errors.New("script driver: Begin without a context is not scripted")
}

// BeginTx costs no scripted answer: the script is about statements. The
// transaction's end is recorded, and COMMIT fails when the script says so.
func (c *scriptConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.d.record("BEGIN")
	return &scriptTx{d: c.d}, nil
}

type scriptTx struct{ d *scriptDB }

func (t *scriptTx) Commit() error {
	t.d.record("COMMIT")
	t.d.mu.Lock()
	err := t.d.failCommit
	t.d.failCommit = nil
	t.d.mu.Unlock()
	return err
}
func (t *scriptTx) Rollback() error { t.d.record("ROLLBACK"); return nil }

func (c *scriptConn) answer(ctx context.Context) (scriptAnswer, error) {
	a, err := c.d.next()
	if err != nil {
		return a, err
	}
	if a.block {
		<-ctx.Done()
		return a, ctx.Err()
	}
	if a.err != nil {
		return a, a.err
	}
	return a, nil
}

func (c *scriptConn) QueryContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Rows, error) {
	c.d.record(q)
	a, err := c.answer(ctx)
	if err != nil {
		return nil, err
	}
	if a.noRows {
		return &scriptRows{cols: []string{"none"}}, nil
	}
	cols := make([]string, len(a.values))
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d", i)
	}
	return &scriptRows{cols: cols, row: a.values}, nil
}

func (c *scriptConn) ExecContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.d.record(q)
	if _, err := c.answer(ctx); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

type scriptRows struct {
	cols []string
	row  []driver.Value
	done bool
}

func (r *scriptRows) Columns() []string { return r.cols }
func (r *scriptRows) Close() error      { return nil }
func (r *scriptRows) Next(dest []driver.Value) error {
	if r.done || r.row == nil {
		return io.EOF
	}
	copy(dest, r.row)
	r.done = true
	return nil
}
