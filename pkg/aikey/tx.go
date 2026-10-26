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

package aikey

import (
	"context"
	"database/sql"
	"errors"
)

// stmtRunner is the part of DBTX a statement needs: the pool, or a
// transaction on it. *sql.Tx has no ping, so a transaction body and the
// helpers it calls are written against this rather than against DBTX.
type stmtRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// txBeginner is the part of *sql.DB a multi-statement write needs. It is
// kept apart from DBTX, as pkg/user keeps it, so a double with no use for
// transactions is not forced to implement one.
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

var errNoTransactions = errors.New("aikey: store handle does not support transactions")

// inTx runs body inside one transaction on db and commits it, so that a
// write made of several statements has one outcome: all of it, or none of
// it. Before this, a key whose class statement failed was removed by a
// compensating delete that could itself fail, and a tenant limit whose
// model rows failed half-way was left partially applied with the caller
// told only about the statement that failed.
//
// The transaction runs under ctx, which carries the operation budget and
// the caller's own context. A failure of BEGIN or COMMIT is classified like
// any other write: a COMMIT that was sent and not answered within the budget
// is the textbook outcome-unknown case, because the server may finish it
// when it resumes. The body's own errors are returned as the body made them,
// already classified by the statement that failed; the deferred rollback
// is then a best effort on a connection that may already be gone.
func inTx(ctx context.Context, db DBTX, body func(tx stmtRunner) error) error {
	b, ok := db.(txBeginner)
	if !ok {
		return errNoTransactions
	}
	tx, err := b.BeginTx(ctx, nil)
	if err != nil {
		return writeErr(err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a commit, or on a dead connection, has nothing to report
	if err := body(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return writeErr(err)
	}
	return nil
}
