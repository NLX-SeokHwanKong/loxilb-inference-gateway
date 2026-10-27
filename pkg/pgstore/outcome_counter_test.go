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
	"database/sql/driver"
	"errors"
	"testing"

	pgxconn "github.com/jackc/pgx/v5/pgconn"

	cmn "github.com/loxilb-io/loxilb/common"
)

// The outcome-unknown counter moves once per newly classified unknown
// write, on the plane named, and never for a write that was not sent, a
// server verdict, or an error already carrying a verdict.
func TestOutcomeUnknownCounterMovesOncePerVerdict(t *testing.T) {
	a0, m0 := OutcomeUnknownTotal(PlaneAIKey), OutcomeUnknownTotal(PlaneMgmt)

	unknown := ClassifyWriteErrFor(PlaneAIKey, errors.New("socket closed after the statement was written"))
	if !errors.Is(unknown, cmn.ErrStoreOutcomeUnknown) {
		t.Fatalf("classified %v", unknown)
	}
	if OutcomeUnknownTotal(PlaneAIKey) != a0+1 || OutcomeUnknownTotal(PlaneMgmt) != m0 {
		t.Fatalf("aikey %d->%d mgmt %d->%d after one unknown on aikey", a0, OutcomeUnknownTotal(PlaneAIKey), m0, OutcomeUnknownTotal(PlaneMgmt))
	}
	// Re-classifying the same error, as a caller wrapping a transaction's
	// statement does, is not a second event.
	if ClassifyWriteErrFor(PlaneAIKey, unknown) != unknown || OutcomeUnknownTotal(PlaneAIKey) != a0+1 {
		t.Fatal("an error already carrying the verdict was counted again")
	}
	// Not sent, a server verdict, a no-row answer: not counted.
	for _, err := range []error{driver.ErrBadConn, context.DeadlineExceeded, &pgxconn.PgError{Code: "23505"}} {
		ClassifyWriteErrFor(PlaneAIKey, err)
	}
	if OutcomeUnknownTotal(PlaneAIKey) != a0+1 {
		t.Fatal("a not-sent or server-answered write was counted as outcome unknown")
	}
	// The management plane counts on its own series, through the plain
	// classifier the management store calls.
	if !errors.Is(ClassifyWriteErr(errors.New("write: broken pipe")), cmn.ErrStoreOutcomeUnknown) || OutcomeUnknownTotal(PlaneMgmt) != m0+1 {
		t.Fatalf("mgmt plane: %d -> %d", m0, OutcomeUnknownTotal(PlaneMgmt))
	}
}
