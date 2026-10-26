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

// Store legs (AIKEY_TEST_DSN) for the idempotent create: a retry converges
// on one row, rotates the undelivered secret, and is refused once the key
// has authenticated. Against a real PostgreSQL, because the lock, the
// partial unique index and the first-use mark are what the scripted store
// cannot prove.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	cmn "github.com/loxilb-io/loxilb/common"
)

func TestCreateKeyWithClientOpIDConvergesToOneRow(t *testing.T) {
	svc := storeFixture(t)
	db := poolOf(t, svc)
	entry := cmn.ApiKeyEntry{TenantID: "team-c", Name: "retried", Enabled: true, ClientOpID: "op-1234"}

	// The create whose answer is lost: the row exists, the secret was
	// delivered to nobody.
	first, err := svc.CreateKey(context.Background(), entry)
	if err != nil || first.Reused || first.RawKey == "" {
		t.Fatalf("first create: %+v %v", first, err)
	}

	// The retry: same row, same key_id, a new secret; the old one is dead.
	second, err := svc.CreateKey(context.Background(), entry)
	if err != nil || !second.Reused || second.KeyID != first.KeyID || second.RawKey == first.RawKey {
		t.Fatalf("retry: %+v %v (first %+v)", second, err, first)
	}
	var n int
	if err := db.QueryRowContext(context.Background(), fmt.Sprintf(
		"SELECT count(*) FROM %s.api_keys WHERE tenant_id = $1", Schema), "team-c").Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows for the tenant = %d %v, want 1", n, err)
	}
	if _, err := svc.ValidateAPIKey(first.RawKey); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("the rotated-out secret still authenticates: %v", err)
	}
	e, err := svc.ValidateAPIKey(second.RawKey)
	if err != nil || e.KeyID != first.KeyID || e.ClientOpID != "op-1234" {
		t.Fatalf("the new secret does not authenticate: %+v %v", e, err)
	}
	// That authentication is the key's first use, recorded off the worker.
	waitFirstUsed(t, db, first.KeyID, true)

	// From here a retry must not rotate the secret somebody holds.
	_, err = svc.CreateKey(context.Background(), entry)
	var delivered *KeyAlreadyDeliveredError
	if !errors.As(err, &delivered) || delivered.KeyID != first.KeyID {
		t.Fatalf("retry after first use = %v, want already delivered for %s", err, first.KeyID)
	}
	svc.Cache.Delete(cacheKeyForHash(hashKey(second.RawKey)))
	if e, err := svc.ValidateAPIKey(second.RawKey); err != nil || e.KeyID != first.KeyID {
		t.Fatalf("the refused retry rotated the secret in use: %v", err)
	}

	// The handle is visible on the management reads, for reconciliation.
	list, err := svc.ListAPIKeys(context.Background(), "team-c")
	if err != nil || len(list) != 1 || list[0].ClientOpID != "op-1234" {
		t.Fatalf("listing: %+v %v", list, err)
	}
	svc.Cache.Delete(cacheKeyForID(first.KeyID))
	got, err := svc.GetAPIKeyByID(first.KeyID)
	if err != nil || got.ClientOpID != "op-1234" {
		t.Fatalf("by id: %+v %v", got, err)
	}

	// The index is per tenant: another tenant may use the same id.
	other := entry
	other.TenantID = "team-d"
	if c, err := svc.CreateKey(context.Background(), other); err != nil || c.Reused || c.KeyID == first.KeyID {
		t.Fatalf("another tenant's create with the same id: %+v %v", c, err)
	}
}

func TestCreateKeyWithoutClientOpIDIsUnchanged(t *testing.T) {
	svc := storeFixture(t)
	db := poolOf(t, svc)
	entry := cmn.ApiKeyEntry{TenantID: "team-e", Name: "plain", Enabled: true}
	a, err := svc.CreateKey(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateKey(context.Background(), entry)
	if err != nil || b.KeyID == a.KeyID || b.Reused {
		t.Fatalf("two creates without an id did not make two keys: %+v %+v %v", a, b, err)
	}
	if _, err := svc.ValidateAPIKey(a.RawKey); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	waitFirstUsed(t, db, a.KeyID, false)
}

func waitFirstUsed(t *testing.T, db *sql.DB, keyID string, want bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var marked bool
		if err := db.QueryRowContext(context.Background(), fmt.Sprintf(
			"SELECT first_used_at IS NOT NULL FROM %s.api_keys WHERE key_id = $1", Schema), keyID).Scan(&marked); err != nil {
			t.Fatal(err)
		}
		if marked == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("first_used_at set = %v, want %v", marked, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
