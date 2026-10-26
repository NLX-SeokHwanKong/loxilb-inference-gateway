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

// The multi-statement writes run as one transaction, and the idempotent
// create converges a retry on one row. Driven through the scripted store so
// the statement sequence, the rollback and the classification of a failed
// COMMIT are asserted, not assumed.

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"

	cmn "github.com/loxilb-io/loxilb/common"
	"github.com/loxilb-io/loxilb/pkg/pgstore"
)

func scriptedService(t *testing.T) (*Service, *scriptDB) {
	t.Helper()
	db := newScriptDB(t)
	svc := &Service{db: db, Cache: cache.New(CacheExpirationTime*time.Minute, CacheCleanupInterval*time.Minute)}
	return svc, db
}

// kinds reduces the recorded statements to what they are, so a test states
// the shape of a write without quoting SQL.
func kinds(stmts []string) string {
	var out []string
	for _, s := range stmts {
		switch {
		case s == "BEGIN" || s == "COMMIT" || s == "ROLLBACK":
			out = append(out, s)
		case strings.Contains(s, "FOR UPDATE"):
			out = append(out, "LOCK")
		case strings.HasPrefix(s, "INSERT"):
			out = append(out, "INSERT")
		case strings.Contains(s, "SET key_hash"):
			out = append(out, "ROTATE")
		case strings.HasPrefix(s, "UPDATE"):
			out = append(out, "UPDATE")
		case strings.HasPrefix(s, "DELETE"):
			out = append(out, "DELETE")
		case strings.HasPrefix(s, "SELECT"):
			out = append(out, "SELECT")
		default:
			out = append(out, "?")
		}
	}
	return strings.Join(out, " ")
}

func TestCreateWithAClassIsOneTransaction(t *testing.T) {
	svc, db := scriptedService(t)
	db.always(scriptAnswer{}) // every statement succeeds

	_, keyID, err := svc.CreateAPIKey(context.Background(), cmn.ApiKeyEntry{
		TenantID: "t", Enabled: true, AIClassPolicy: cmn.AIClassPolicy{PriorityClass: cmn.AIClassSheddable},
	})
	if err != nil || keyID == "" {
		t.Fatalf("create: %v %q", err, keyID)
	}
	// The refresh after the write reads the store too; only the write's own
	// shape is asserted.
	got := kinds(db.statements())
	if !strings.HasPrefix(got, "BEGIN INSERT UPDATE COMMIT") {
		t.Fatalf("a create with a class is not one transaction: %s", got)
	}
}

func TestCreateRollsBackWhenTheClassStatementFails(t *testing.T) {
	svc, db := scriptedService(t)
	db.push(scriptAnswer{}, scriptAnswer{err: errors.New("stall after send")})

	_, _, err := svc.CreateAPIKey(context.Background(), cmn.ApiKeyEntry{
		TenantID: "t", Enabled: true, AIClassPolicy: cmn.AIClassPolicy{PriorityClass: cmn.AIClassSheddable},
	})
	if !errors.Is(err, cmn.ErrStoreOutcomeUnknown) {
		t.Fatalf("a class statement sent and unanswered = %v, want outcome unknown", err)
	}
	got := kinds(db.statements())
	if got != "BEGIN INSERT UPDATE ROLLBACK" {
		t.Fatalf("the insert was not rolled back with the failed class statement: %s", got)
	}
}

func TestCommitFailureIsOutcomeUnknownAndCounted(t *testing.T) {
	svc, db := scriptedService(t)
	db.always(scriptAnswer{})
	db.failCommit = errors.New("connection reset after COMMIT was sent")
	before := pgstore.OutcomeUnknownTotal(pgstore.PlaneAIKey)
	mgmtBefore := pgstore.OutcomeUnknownTotal(pgstore.PlaneMgmt)

	err := svc.SetTenantLimits(context.Background(), "t", 5, 500, 0, []cmn.TenantModelRateLimit{{Model: "m", TokensPerMin: 50}})
	if !errors.Is(err, cmn.ErrStoreOutcomeUnknown) {
		t.Fatalf("a COMMIT sent and unanswered = %v, want outcome unknown", err)
	}
	if got := kinds(db.statements()); got != "BEGIN UPDATE UPDATE COMMIT ROLLBACK" && got != "BEGIN INSERT INSERT COMMIT ROLLBACK" {
		// The upserts are INSERT ... ON CONFLICT statements.
		if !strings.HasPrefix(got, "BEGIN") || !strings.Contains(got, "COMMIT") {
			t.Fatalf("shape: %s", got)
		}
	}
	// The caches hold nothing the store never confirmed.
	if _, ok := svc.Cache.Get(cacheKeyForTenant("t")); ok {
		t.Fatal("a failed transaction wrote the tenant limit into the cache")
	}
	if _, ok := svc.lastKnownRateLimit(cacheKeyForModel("t", "m")); ok {
		t.Fatal("a failed transaction wrote the model limit into the last-known map")
	}
	if n := pgstore.OutcomeUnknownTotal(pgstore.PlaneAIKey); n != before+1 {
		t.Fatalf("outcome-unknown counter on the aikey plane moved %d -> %d, want +1", before, n)
	}
	if n := pgstore.OutcomeUnknownTotal(pgstore.PlaneMgmt); n != mgmtBefore {
		t.Fatalf("the mgmt plane's counter moved for an aikey write")
	}
}

func TestPatchAppliesEveryFieldInOneTransaction(t *testing.T) {
	svc, db := scriptedService(t)
	db.push(scriptAnswer{values: []driver.Value{"hash-1"}}) // keyHashByID
	db.always(scriptAnswer{})
	svc.Cache.Set(cacheKeyForHash("hash-1"), &cmn.ApiKeyEntry{KeyID: "kid-1"}, time.Minute)
	var mu sync.Mutex
	var evicted []KeyInvalidation
	svc.SetInvalidationSink(func(inv KeyInvalidation) { mu.Lock(); evicted = append(evicted, inv); mu.Unlock() })

	enabled, rps := false, 3
	pc := cmn.AIClassStandard
	err := svc.PatchAPIKeyAll(context.Background(), "kid-1", cmn.ApiKeyPatch{
		AllowedModels: []string{"m1"}, Enabled: &enabled, RateLimitRPS: &rps,
		Class: cmn.AIClassPatch{PriorityClass: &pc},
	})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	got := kinds(db.statements())
	if !strings.HasPrefix(got, "BEGIN SELECT UPDATE UPDATE UPDATE UPDATE COMMIT") {
		t.Fatalf("a PATCH naming four fields is not one transaction: %s", got)
	}
	if _, ok := svc.Cache.Get(cacheKeyForHash("hash-1")); ok {
		t.Fatal("the patched key is still cached")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 || evicted[0].KeyHash != "hash-1" || evicted[0].KeyID != "kid-1" {
		t.Fatalf("peers told %+v, want one eviction of the key", evicted)
	}
}

func TestPatchOfAMissingKeyWritesNothing(t *testing.T) {
	svc, db := scriptedService(t)
	db.push(scriptAnswer{noRows: true})
	enabled := false
	err := svc.PatchAPIKeyAll(context.Background(), "ghost", cmn.ApiKeyPatch{Enabled: &enabled})
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("patch of a missing key = %v, want not found", err)
	}
	if got := kinds(db.statements()); got != "BEGIN SELECT ROLLBACK" {
		t.Fatalf("a missing key reached a write: %s", got)
	}
}

func TestUserRateLimitReplaceIsOneTransaction(t *testing.T) {
	svc, db := scriptedService(t)
	db.push(scriptAnswer{})                                    // upsert user row
	db.push(scriptAnswer{noRows: true})                        // the model rows to forget
	db.push(scriptAnswer{})                                    // delete model rows
	db.push(scriptAnswer{err: errors.New("stall after send")}) // model upsert
	err := svc.SetUserRateLimit(context.Background(), cmn.UserRateLimitEntry{
		TenantID: "t", UserID: "u", RPS: 1, ModelLimits: []cmn.UserModelRateLimit{{Model: "m", TokensPerMin: 10}},
	})
	if !errors.Is(err, cmn.ErrStoreOutcomeUnknown) {
		t.Fatalf("model upsert sent and unanswered = %v", err)
	}
	if got := kinds(db.statements()); !strings.HasSuffix(got, "ROLLBACK") || strings.Contains(got, "COMMIT") {
		t.Fatalf("the aggregate row survived its models' failure: %s", got)
	}
	if _, ok := svc.Cache.Get(cacheKeyForUser("t", "u")); ok {
		t.Fatal("a rolled-back user limit is cached")
	}
}

// The idempotent create: the first request inserts; a retry finds the row,
// rotates the secret and answers the same key_id; once the key has
// authenticated the retry is refused.
func TestCreateWithClientOpIDConvergesAndRotatesUntilFirstUse(t *testing.T) {
	svc, db := scriptedService(t)
	var mu sync.Mutex
	var evicted []KeyInvalidation
	svc.SetInvalidationSink(func(inv KeyInvalidation) { mu.Lock(); evicted = append(evicted, inv); mu.Unlock() })
	entry := cmn.ApiKeyEntry{TenantID: "t", Name: "n", Enabled: true, ClientOpID: "op-1"}

	// 1. Nothing under this id: lock finds no row, insert.
	db.push(scriptAnswer{noRows: true}, scriptAnswer{})
	first, err := svc.CreateKey(context.Background(), entry)
	if err != nil || first.Reused || first.KeyID == "" || !strings.HasPrefix(first.RawKey, apiKeyPrefix) {
		t.Fatalf("first create: %+v %v", first, err)
	}
	if got := kinds(db.statements()); got != "BEGIN LOCK INSERT COMMIT" {
		t.Fatalf("first create shape: %s", got)
	}

	// 2. The answer was lost; the retry finds the row, never used.
	oldHash := hashKey(first.RawKey)
	svc.Cache.Set(cacheKeyForHash(oldHash), &cmn.ApiKeyEntry{KeyID: first.KeyID}, time.Minute)
	db.push(scriptAnswer{values: []driver.Value{first.KeyID, oldHash, nil}}, scriptAnswer{})
	second, err := svc.CreateKey(context.Background(), entry)
	if err != nil || !second.Reused || second.KeyID != first.KeyID || second.RawKey == first.RawKey || second.RawKey == "" {
		t.Fatalf("retry: %+v %v (first %+v)", second, err, first)
	}
	if got := kinds(db.statements()); !strings.HasSuffix(got, "BEGIN LOCK ROTATE COMMIT") {
		t.Fatalf("retry shape: %s", got)
	}
	if _, ok := svc.Cache.Get(cacheKeyForHash(oldHash)); ok {
		t.Fatal("the secret delivered to nobody is still cached as valid")
	}
	mu.Lock()
	if len(evicted) != 1 || evicted[0].KeyHash != oldHash || evicted[0].KeyID != first.KeyID {
		t.Fatalf("peers told %+v, want the rotated-out hash", evicted)
	}
	mu.Unlock()

	// 3. The key has authenticated: refused, nothing written.
	db.push(scriptAnswer{values: []driver.Value{first.KeyID, hashKey(second.RawKey), time.Now()}})
	_, err = svc.CreateKey(context.Background(), entry)
	var delivered *KeyAlreadyDeliveredError
	if !errors.As(err, &delivered) || delivered.KeyID != first.KeyID || !errors.Is(err, ErrKeyAlreadyDelivered) {
		t.Fatalf("retry after first use = %v, want already delivered naming %s", err, first.KeyID)
	}
	if got := kinds(db.statements()); !strings.HasSuffix(got, "BEGIN LOCK ROLLBACK") {
		t.Fatalf("a refused retry wrote something: %s", got)
	}
}

func TestClientOpIDIsValidated(t *testing.T) {
	svc, _ := scriptedService(t)
	for _, bad := range []string{strings.Repeat("x", maxClientOpIDLen+1), "has space", "tab\there", "ünïcode"} {
		_, err := svc.CreateKey(context.Background(), cmn.ApiKeyEntry{TenantID: "t", ClientOpID: bad})
		var invalid *cmn.ValidationError
		if !errors.As(err, &invalid) {
			t.Fatalf("client_op_id %q accepted: %v", bad, err)
		}
	}
}

// The data path records the first authentication of a key created with an
// operation id, off the worker, once; a key without one costs nothing.
func TestValidateMarksFirstUseOnceForKeysWithAnOperationID(t *testing.T) {
	svc, db := scriptedService(t)
	created := time.Unix(1_700_000_000, 0).UTC()
	row := func(opID any, firstUsed any) scriptAnswer {
		return scriptAnswer{values: []driver.Value{"kid-1", hashKey("k"), "t", "n", "", int64(0), int64(0), int64(0), created, nil, true, opID, firstUsed}}
	}

	db.push(row("op-1", nil), scriptAnswer{}) // the row, then the mark
	if _, err := svc.ValidateAPIKey("k"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for db.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := kinds(db.statements()); got != "SELECT UPDATE" {
		t.Fatalf("first use not marked: %s", got)
	}

	svc.Cache.Delete(cacheKeyForHash(hashKey("k")))
	db.push(row("op-1", created)) // already marked
	if _, err := svc.ValidateAPIKey("k"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	svc.Cache.Delete(cacheKeyForHash(hashKey("k")))
	db.push(row(nil, nil)) // no operation id
	if _, err := svc.ValidateAPIKey("k"); err != nil {
		t.Fatalf("validate: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := db.callCount(); n != 4 {
		t.Fatalf("%d store calls, want 4: a marked key or one without an id must not write", n)
	}
}
