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
	"errors"
	"reflect"
	"sync"
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
)

func strp(s string) *string { return &s }

func TestClassPatchApply(t *testing.T) {
	base := cmn.AIClassPolicy{PriorityClass: "standard", DefaultObjective: "batch", AllowedObjectives: []string{"batch"}}
	if got := (cmn.AIClassPatch{}).Apply(base); !reflect.DeepEqual(got, base) || !(cmn.AIClassPatch{}).IsZero() {
		t.Errorf("an empty patch changed the policy: %+v", got)
	}
	none := []string{}
	got := cmn.AIClassPatch{PriorityClass: strp(""), AllowedObjectives: &none}.Apply(base)
	if got.PriorityClass != "" || got.DefaultObjective != "batch" || len(got.AllowedObjectives) != 0 {
		t.Errorf("a patch that clears two fields left %+v", got)
	}
	list := []string{"a"}
	got = cmn.AIClassPatch{AllowedObjectives: &list}.Apply(base)
	list[0] = "changed"
	if got.AllowedObjectives[0] != "a" {
		t.Errorf("the applied list shares the patch's memory")
	}
}

// ---- the store legs (need AIKEY_TEST_DSN) ----

// A patch writes the fields it names and keeps the others; the limits of
// the tenant are not its business, in either direction.
func TestTenantClassPatchStore(t *testing.T) {
	svc := storeFixture(t)
	if err := svc.SetTenantRateLimit(context.Background(), "t1", 25, 120000, 40); err != nil {
		t.Fatal(err)
	}
	if err := svc.PatchTenantClass(context.Background(), "t1", cmn.AIClassPatch{PriorityClass: strp("standard")}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	allowed := []string{"realtime", "batch"}
	if err := svc.PatchTenantClass(context.Background(), "t1", cmn.AIClassPatch{DefaultObjective: strp("batch"), AllowedObjectives: &allowed}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	want := cmn.AIClassPolicy{PriorityClass: "standard", DefaultObjective: "batch", AllowedObjectives: []string{"batch", "realtime"}}
	if got, err := svc.GetTenantClass(context.Background(), "t1"); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("class after two patches = %+v, %v; want %+v", got, err, want)
	}
	svc.Cache.Flush()
	if rps, tpm, burst, err := svc.GetTenantRateLimit("t1"); err != nil || rps != 25 || tpm != 120000 || burst != 40 {
		t.Errorf("limits after class patches = %d, %d, %d, %v", rps, tpm, burst, err)
	}
	// The limits written again, to zero: the class stays.
	if err := svc.SetTenantRateLimit(context.Background(), "t1", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetTenantClass(context.Background(), "t1"); !reflect.DeepEqual(got, want) {
		t.Errorf("class after the limits were zeroed = %+v, want %+v", got, want)
	}
	// One field cleared, the others kept.
	if err := svc.PatchTenantClass(context.Background(), "t1", cmn.AIClassPatch{DefaultObjective: strp("")}); err != nil {
		t.Fatal(err)
	}
	want.DefaultObjective = ""
	if got, _ := svc.GetTenantClass(context.Background(), "t1"); !reflect.DeepEqual(got, want) {
		t.Errorf("class after clearing default_objective = %+v, want %+v", got, want)
	}
	// A patch on a tenant with no row creates one of zero limits.
	if err := svc.PatchTenantClass(context.Background(), "fresh", cmn.AIClassPatch{PriorityClass: strp("sheddable")}); err != nil {
		t.Fatal(err)
	}
	if e, err := svc.GetTenantRateLimitEntry(context.Background(), "fresh"); err != nil || e.RPS != 0 || e.TokensPerMin != 0 {
		t.Errorf("entry of a class-only tenant = %+v, %v", e, err)
	}
	// A refused value changes nothing; an empty patch is not a write.
	if err := svc.PatchTenantClass(context.Background(), "t1", cmn.AIClassPatch{PriorityClass: strp("urgent")}); err == nil {
		t.Errorf("an unknown class was stored")
	}
	if got, _ := svc.GetTenantClass(context.Background(), "t1"); !reflect.DeepEqual(got, want) {
		t.Errorf("class after a refused patch = %+v, want %+v", got, want)
	}
	if err := svc.PatchTenantClass(context.Background(), "nobody", cmn.AIClassPatch{}); err != nil {
		t.Errorf("an empty patch = %v", err)
	}
	if _, err := svc.GetTenantRateLimitEntry(context.Background(), "nobody"); err == nil {
		t.Errorf("an empty patch created a tenant row")
	}
}

func TestAPIKeyClassAtCreateAndPatchStore(t *testing.T) {
	svc := storeFixture(t)

	want := cmn.AIClassPolicy{PriorityClass: "sheddable", DefaultObjective: "batch", AllowedObjectives: []string{"batch", "realtime"}}
	_, keyID, err := svc.CreateAPIKey(context.Background(), cmn.ApiKeyEntry{TenantID: "t1", Name: "k", Enabled: true,
		AIClassPolicy: cmn.AIClassPolicy{PriorityClass: "sheddable", DefaultObjective: "batch", AllowedObjectives: []string{"realtime", "batch"}}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, err := svc.GetAPIKeyClass(context.Background(), keyID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("class of a key created with one = %+v, %v; want %+v", got, err, want)
	}
	// It is in the snapshot the request path reads, at once.
	if got := svc.Classes().Keys[keyID]; !reflect.DeepEqual(got, want) {
		t.Errorf("snapshot after create = %+v, want %+v", got, want)
	}
	// A key asked for with a class the store refuses is not created.
	before, _ := svc.ListAPIKeys(context.Background(), "t1")
	if _, _, err := svc.CreateAPIKey(context.Background(), cmn.ApiKeyEntry{TenantID: "t1", Name: "bad", Enabled: true,
		AIClassPolicy: cmn.AIClassPolicy{PriorityClass: "urgent"}}); err == nil {
		t.Errorf("a key with an unknown class was created")
	}
	if after, _ := svc.ListAPIKeys(context.Background(), "t1"); len(after) != len(before) {
		t.Errorf("a refused create left a key behind: %d keys, was %d", len(after), len(before))
	}

	// Patch: one field, the others kept; the key's limits untouched.
	one := 7
	if err := svc.PatchAPIKeyRateLimits(context.Background(), keyID, &one, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.PatchAPIKeyClass(context.Background(), keyID, cmn.AIClassPatch{PriorityClass: strp("")}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	want.PriorityClass = ""
	if got, _ := svc.GetAPIKeyClass(context.Background(), keyID); !reflect.DeepEqual(got, want) {
		t.Errorf("class after clearing priority_class = %+v, want %+v", got, want)
	}
	svc.Cache.Flush()
	if k, err := svc.GetAPIKeyByID(keyID); err != nil || k.RateLimitRPS != 7 {
		t.Errorf("limits after a class patch = %+v, %v", k, err)
	}
	if err := svc.PatchAPIKeyClass(context.Background(), "no-such-key", cmn.AIClassPatch{PriorityClass: strp("standard")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("patching a key that does not exist = %v, want not found", err)
	}

	// The listing holds exactly the keys that set a field.
	_, plain, err := svc.CreateAPIKey(context.Background(), cmn.ApiKeyEntry{TenantID: "t1", Name: "plain", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	classes, err := svc.ListAPIKeyClasses(context.Background())
	if err != nil || !reflect.DeepEqual(classes[keyID], want) {
		t.Fatalf("listing = %+v, %v", classes, err)
	}
	if _, ok := classes[plain]; ok {
		t.Errorf("a key without a class is in the listing")
	}
}

// With the store away every class write is refused as unavailable, and
// nothing is half-done.
func TestClassPatchOutage(t *testing.T) {
	svc := storeFixture(t)
	if err := svc.PatchTenantClass(context.Background(), "t1", cmn.AIClassPatch{PriorityClass: strp("standard")}); err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	svc.db = nil
	svc.mu.Unlock()
	if err := svc.PatchTenantClass(context.Background(), "t1", cmn.AIClassPatch{PriorityClass: strp("sheddable")}); !errors.Is(err, cmn.ErrDBUnavailable) {
		t.Errorf("tenant class patch during an outage = %v, want unavailable", err)
	}
	if err := svc.PatchAPIKeyClass(context.Background(), "k", cmn.AIClassPatch{PriorityClass: strp("sheddable")}); !errors.Is(err, cmn.ErrDBUnavailable) {
		t.Errorf("key class patch during an outage = %v, want unavailable", err)
	}
	if _, err := svc.ListAPIKeyClasses(context.Background()); !errors.Is(err, cmn.ErrDBUnavailable) {
		t.Errorf("key class listing during an outage = %v, want unavailable", err)
	}
	if got := svc.Classes().Tenants["t1"].PriorityClass; got != "standard" {
		t.Errorf("the snapshot in force during an outage holds %q, want the last read", got)
	}
}

// Two patches of different fields that race both land: a patch is one
// statement, not a read and a write. Run under -race -count=20.
func TestClassPatchConcurrentFieldsBothLand(t *testing.T) {
	svc := storeFixture(t)
	_, keyID, err := svc.CreateAPIKey(context.Background(), cmn.ApiKeyEntry{TenantID: "t1", Name: "race", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 20; round++ {
		class := []string{"standard", "sheddable"}[round%2]
		def := []string{"batch", "realtime"}[round%2]
		allowed := []string{"tier:flex", def}
		var wg sync.WaitGroup
		errs := make(chan error, 6)
		for _, p := range []cmn.AIClassPatch{
			{PriorityClass: strp(class)}, {DefaultObjective: strp(def)}, {AllowedObjectives: &allowed},
		} {
			wg.Add(2)
			go func(p cmn.AIClassPatch) { defer wg.Done(); errs <- svc.PatchTenantClass(context.Background(), "t1", p) }(p)
			go func(p cmn.AIClassPatch) {
				defer wg.Done()
				errs <- svc.PatchAPIKeyClass(context.Background(), keyID, p)
			}(p)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		want := cmn.AIClassPolicy{PriorityClass: class, DefaultObjective: def}
		want.AllowedObjectives, _ = func() ([]string, error) {
			p, err := ValidateClassPolicy(cmn.AIClassPolicy{AllowedObjectives: allowed})
			return p.AllowedObjectives, err
		}()
		if got, err := svc.GetTenantClass(context.Background(), "t1"); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: tenant class %+v, %v; want %+v", round, got, err, want)
		}
		if got, err := svc.GetAPIKeyClass(context.Background(), keyID); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: key class %+v, %v; want %+v", round, got, err, want)
		}
	}
	// The snapshot in force is the last read, and holds what the store holds.
	if got := svc.Classes().Tenants["t1"].PriorityClass; got != "sheddable" {
		t.Errorf("snapshot after the races holds %q, want the store's", got)
	}
}
