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
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"

	pgxconn "github.com/jackc/pgx/v5/pgconn"

	cmn "github.com/loxilb-io/loxilb/common"
)

// Write-outcome classification.
//
// A deadline bounds how long a write waits; it cannot retract a statement
// that has already reached the server's socket. When a write fails, the
// caller therefore needs to know not just that it failed but whether the
// server may still apply it. Two answers, and nothing in between:
//
//   - cmn.ErrDBUnavailable: the statement was never handed to the server.
//     Retrying is safe. HTTP 503.
//   - cmn.ErrStoreOutcomeUnknown: the statement was (or may have been)
//     sent and no answer arrived within the budget. The server may commit it
//     when it resumes. The caller must read the row back before deciding
//     whether to retry. HTTP 504.
//
// The split is made from what pgx and database/sql actually return, read
// from their source at v5.10.0, not from the wording of the error:
//
//   - pgconn marks an error it raised BEFORE sending anything with
//     SafeToRetry()==true (a lock failure, a context already done at entry,
//     a dial failure). The stdlib driver turns those into driver.ErrBadConn,
//     which database/sql retries on another connection and, if every attempt
//     fails the same way, returns as-is. A context that expires while
//     database/sql is still waiting for a pooled connection is returned as
//     the bare context error, without pgconn ever being asked.
//   - A deadline that fires while a statement is in flight makes pgconn
//     return its *errTimeout wrapper (pgconn.Timeout(err)==true) with
//     SafeToRetry()==false, and close the connection so the pool cannot
//     reuse it. That is the sent-and-unanswered case.
//   - Anything the server examined comes back as a *pgconn.PgError and is
//     an application outcome, not a transport one. It is returned unchanged
//     so the caller's own classification (a unique violation is a 409) keeps
//     working.
//
// An error that fits none of those shapes is reported as outcome unknown:
// the conservative answer, because "not committed" is the one a caller acts
// on by retrying. The cause stays in the chain (errors.Is finds both the
// sentinel and the original), so a caller's own classification of the
// cause, and the log line, keep working.

// ClassifyWriteErr maps the error of a store write to the write-outcome
// contract. nil, *pgconn.PgError and sql.ErrNoRows pass through unchanged.
func ClassifyWriteErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, cmn.ErrDBUnavailable) || errors.Is(err, cmn.ErrStoreOutcomeUnknown) {
		return err
	}
	var pgErr *pgxconn.PgError
	if errors.As(err, &pgErr) || errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if notSent(err) {
		return fmt.Errorf("%w: %w", cmn.ErrDBUnavailable, err)
	}
	return fmt.Errorf("%w: %w", cmn.ErrStoreOutcomeUnknown, err)
}

// ClassifyReadErr maps the error of a store read. A read has no outcome to
// be unsure about, so every transport failure, including a deadline that
// fired after the statement was sent, is reported as the store being
// unavailable. nil, *pgconn.PgError and sql.ErrNoRows pass through.
func ClassifyReadErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, cmn.ErrDBUnavailable) {
		return err
	}
	var pgErr *pgxconn.PgError
	if errors.As(err, &pgErr) || errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if IsUnavailable(err) || pgxconn.Timeout(err) || notSent(err) ||
		errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %w", cmn.ErrDBUnavailable, err)
	}
	return err
}

// notSent reports whether err shows the statement never reached the server.
func notSent(err error) bool {
	// pgx raised the error before writing to the socket, or the stdlib
	// driver already said so by handing database/sql an ErrBadConn.
	if pgxconn.SafeToRetry(err) || errors.Is(err, driver.ErrBadConn) {
		return true
	}
	// A dial that failed: there was no session to send on.
	var connErr *pgxconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	// database/sql's own answers for a statement it never ran: the pool
	// handle or transaction was already closed when it was asked.
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return true
	}
	// A deadline that pgconn saw fire after the send arrives wrapped in its
	// timeout type; the bare context error is database/sql giving up while
	// it was still waiting for a free connection. (A bare context.Canceled
	// is NOT classified here: pgconn returns it bare after a send too.)
	if errors.Is(err, context.DeadlineExceeded) && !pgxconn.Timeout(err) {
		return true
	}
	return false
}
