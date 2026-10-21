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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"

	cmn "github.com/loxilb-io/loxilb/common"
)

func sampleWithLimits() *ClassSnapshot {
	snap := sampleSnapshot()
	snap.limits = &limitRows{
		Tenants:      []lkgTenantLimit{{Tenant: "t1", RPS: 25, TokensPerMin: 120000, BurstPct: 40}},
		TenantModels: []lkgTenantModelLimit{{Tenant: "t1", Model: "m1", TokensPerMin: 900}},
		Defaults: []lkgDefaults{{Scope: cmn.RateLimitScopeGlobal, DefaultTenantRPS: 3, DefaultTenantTPM: 300},
			{Scope: cmn.RateLimitScopeRule, RuleIdent: "10.0.0.1:80", VipSharedRPS: 9}},
	}
	return snap
}

// startedFromFile is a store-less process that has read the file, as a
// gateway that starts while the store is away does.
func startedFromFile(t *testing.T, dir string) *Service {
	t.Helper()
	next := &Service{Cache: cache.New(time.Minute, time.Minute)}
	next.class.dir.Store(&dir)
	next.classTick()
	if src, _, _ := next.ClassSource(); src != cmn.AIClassSourceLKG {
		t.Fatalf("source = %s, want lkg", src)
	}
	return next
}

// A process that starts while the store is away serves the limit rows of the
// file where it would otherwise have nothing to enforce: a tenant's limits,
// its model limits and the defaults. A tenant the file does not hold is a
// tenant the store had no row for: no limits, not a refusal.
func TestClassLKGServesLimitsAfterARestart(t *testing.T) {
	svc, dir := classFixture(t)
	svc.writeClassLKG(sampleWithLimits())
	next := startedFromFile(t, dir)

	if rps, tpm, burst, err := next.GetTenantRateLimit("t1"); err != nil || rps != 25 || tpm != 120000 || burst != 40 {
		t.Errorf("tenant limits from the file = %d, %d, %d, %v", rps, tpm, burst, err)
	}
	if rps, tpm, burst, err := next.GetTenantRateLimit("not-in-the-file"); err != nil || rps != 0 || tpm != 0 || burst != 0 {
		t.Errorf("a tenant the file does not hold = %d, %d, %d, %v; want no limits", rps, tpm, burst, err)
	}
	if tpm, err := next.GetTenantModelRateLimit("t1", "m1"); err != nil || tpm != 900 {
		t.Errorf("model limit from the file = %d, %v", tpm, err)
	}
	if tpm, err := next.GetTenantModelRateLimit("t1", "other"); err != nil || tpm != 0 {
		t.Errorf("a model the file does not hold = %d, %v", tpm, err)
	}
	g, ok, err := next.GetRateLimitDefaults(cmn.RateLimitScopeGlobal, "")
	if err != nil || !ok || g.DefaultTenantRPS != 3 || g.DefaultTenantTPM != 300 {
		t.Errorf("global defaults from the file = %+v, %v, %v", g, ok, err)
	}
	if r, ok, err := next.GetRateLimitDefaults(cmn.RateLimitScopeRule, "10.0.0.9:80"); err != nil || ok {
		t.Errorf("a rule the file holds no defaults for = %+v, %v, %v; want none, no error", r, ok, err)
	}
	// Keys are not in the file: a key lookup still fails closed.
	if _, err := next.GetAPIKeyByID("k1"); err == nil {
		t.Errorf("a key was answered from the file")
	}
}

// What this process confirmed itself wins over the file, and once the store
// has answered the file's rows are no longer served.
func TestClassLKGLimitsGiveWayToTheStore(t *testing.T) {
	svc, dir := classFixture(t)
	svc.writeClassLKG(sampleWithLimits())
	next := startedFromFile(t, dir)

	next.rlLastKnown.Store(cacheKeyForTenant("t1"), &rateLimitCacheEntry{rps: 1})
	if rps, _, _, err := next.GetTenantRateLimit("t1"); err != nil || rps != 1 {
		t.Errorf("a value this process confirmed = %d, %v; want it over the file's", rps, err)
	}

	if !next.publishClasses(&ClassSnapshot{Source: cmn.AIClassSourceStore, Gen: 1}) {
		t.Fatalf("a store snapshot was not published over the file's")
	}
	if _, _, _, err := next.GetTenantRateLimit("not-in-the-file"); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("after the store answered, an unconfirmed tenant during an outage = %v, want unavailable", err)
	}
}

// A file of version 1 (classes only) is still read for its classes; it has
// no limits to serve, so a network tenant fails closed as before.
func TestClassLKGVersion1HasNoLimits(t *testing.T) {
	_, dir := classFixture(t)
	v1 := `{"version":1,"source_timestamp":"2023-11-14T22:15:00Z","objectives":[{"name":"x","priority":1}],"tenants":{},"keys":{}}`
	if err := os.WriteFile(filepath.Join(dir, ClassLKGFileName), []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	next := startedFromFile(t, dir)
	if _, ok := next.Classes().Objectives["x"]; !ok {
		t.Errorf("a version 1 file's objectives were not read")
	}
	if _, _, _, err := next.GetTenantRateLimit("t1"); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("a version 1 file answered a limit: %v", err)
	}
	// The store then answers with the same classes and no limit rows: the
	// file is rewritten as version 2, though nothing else changed.
	same := &ClassSnapshot{Source: cmn.AIClassSourceStore, Gen: next.class.readGen.Add(1),
		ReadAt:     time.Now().UTC(),
		Objectives: map[string]cmn.AIObjectiveEntry{"x": {Name: "x", Priority: 1}},
		Tenants:    map[string]cmn.AIClassPolicy{}, Keys: map[string]cmn.AIClassPolicy{},
		limits: &limitRows{}}
	if !next.publishClasses(same) {
		t.Fatal("the store's snapshot was not published over the file's")
	}
	next.writeClassLKGIfCurrent(same)
	data, err := os.ReadFile(filepath.Join(dir, ClassLKGFileName))
	var f classLKGFile
	if err == nil {
		err = json.Unmarshal(data, &f)
	}
	if err != nil || f.Version != classLKGVersion {
		t.Errorf("a version 1 file was not rewritten as version %d (%v):\n%s", classLKGVersion, err, data)
	}
}

// A snapshot is published only over an older one: whatever order the reads
// finish in, the newest stays, and what is published never goes back.
func TestClassPublishNeverGoesBack(t *testing.T) {
	svc, _ := classFixture(t)
	var mu sync.Mutex
	var seen []uint64
	svc.SetClassChangeHook(func(s *ClassSnapshot) {
		mu.Lock()
		seen = append(seen, s.Gen)
		mu.Unlock()
	})
	const n = 64
	gens := make([]uint64, n)
	for i := range gens {
		gens[i] = svc.class.readGen.Add(1)
	}
	var wg sync.WaitGroup
	for i := n - 1; i >= 0; i-- {
		wg.Add(1)
		go func(g uint64) {
			defer wg.Done()
			svc.publishClasses(&ClassSnapshot{Source: cmn.AIClassSourceStore, Gen: g})
		}(gens[i])
	}
	wg.Wait()
	if got := svc.Classes().Gen; got != gens[n-1] {
		t.Errorf("published generation = %d, want the newest %d", got, gens[n-1])
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("published %d after %d", seen[i], seen[i-1])
		}
	}
	// The file never replaces a store snapshot, and a store snapshot of
	// any generation replaces the file's.
	if svc.publishClasses(&ClassSnapshot{Source: cmn.AIClassSourceStore, Gen: gens[0]}) {
		t.Errorf("an older read was published over a newer one")
	}
}

// The objective list of the API: the store's when it answers; while it does
// not, the applied one with its source and the time the store answered with
// it; 503 only when nothing was applied yet.
func TestObjectiveListDuringAnOutage(t *testing.T) {
	svc, dir := classFixture(t)
	if _, err := svc.ObjectiveList(); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("nothing applied, no store = %v, want unavailable", err)
	}
	svc.writeClassLKG(sampleWithLimits())
	next := startedFromFile(t, dir)
	got, err := next.ObjectiveList()
	if err != nil {
		t.Fatalf("applied list during an outage: %v", err)
	}
	if got.Source != cmn.AIClassSourceLKG || !got.SourceTimestamp.Equal(sampleSnapshot().ReadAt) {
		t.Errorf("source %q at %v, want lkg at the file's timestamp", got.Source, got.SourceTimestamp)
	}
	if len(got.Objectives) != 2 || got.Objectives[0].Name != "batch" || got.Objectives[1].Name != "realtime" {
		t.Errorf("objectives %+v, want the file's, by name", got.Objectives)
	}
	// The store is away and says so; its last answered read is the one the
	// file was written after.
	if got.StoreReachable || !got.StoreLastRead.Equal(sampleSnapshot().ReadAt) {
		t.Errorf("store reachable %v, last read %v; want false at the file's timestamp", got.StoreReachable, got.StoreLastRead)
	}
	// A snapshot this process read from the store, the store away again:
	// the list is that one, and says it came from the store, when.
	at := time.Date(2026, 10, 14, 5, 0, 0, 0, time.UTC)
	next.publishClasses(&ClassSnapshot{Source: cmn.AIClassSourceStore, Gen: 9, ReadAt: at,
		Objectives: map[string]cmn.AIObjectiveEntry{"only": {Name: "only"}}})
	if got, err = next.ObjectiveList(); err != nil || got.Source != cmn.AIClassSourceStore ||
		!got.SourceTimestamp.Equal(at) || len(got.Objectives) != 1 || got.StoreReachable {
		t.Errorf("applied store list = %+v, %v", got, err)
	}
}

// The store's latest answer is kept by read ticket: a slow read that fails
// after a later one answered does not mark the store away, and the time of
// the last answered read moves only when one answers.
func TestStoreHealthFollowsTheLatestRead(t *testing.T) {
	svc := &Service{}
	if ok, at := svc.StoreHealth(); ok || !at.IsZero() {
		t.Fatalf("before any read: %v %v", ok, at)
	}
	svc.noteStoreRead(5, nil)
	ok, first := svc.StoreHealth()
	if !ok || first.IsZero() {
		t.Fatalf("after an answered read: %v %v", ok, first)
	}
	svc.noteStoreRead(4, ErrDBUnavailable)
	if ok, at := svc.StoreHealth(); !ok || !at.Equal(first) {
		t.Errorf("an older failed read changed the answer: %v %v", ok, at)
	}
	svc.noteStoreRead(6, ErrDBUnavailable)
	if ok, at := svc.StoreHealth(); ok || !at.Equal(first) {
		t.Errorf("after a later failed read: %v %v, want false at %v", ok, at, first)
	}
	// The refresher's own reads are recorded, under the same tickets: no
	// store configured is a read that did not answer.
	svc = &Service{}
	svc.noteStoreRead(svc.class.readGen.Add(1), nil)
	if err := svc.RefreshClasses(); err == nil {
		t.Fatal("a refresh without a store answered")
	}
	if ok, _ := svc.StoreHealth(); ok {
		t.Error("a failed refresh left the store reachable")
	}
}

// Two refreshes publish in order but may reach the disk in the other order:
// the older, written last, must not land.
func TestClassLKGOlderWriteDoesNotLand(t *testing.T) {
	svc, dir := classFixture(t)
	older := sampleWithLimits()
	older.Gen = 5
	newer := sampleWithLimits()
	newer.Gen = 6
	newer.Keys = map[string]cmn.AIClassPolicy{"k-newer": {PriorityClass: "standard"}}
	if !svc.publishClasses(older) || !svc.publishClasses(newer) {
		t.Fatal("the two snapshots were not published in order")
	}
	svc.writeClassLKGIfCurrent(newer)
	svc.writeClassLKGIfCurrent(older)
	data, err := os.ReadFile(filepath.Join(dir, ClassLKGFileName))
	if err != nil || !strings.Contains(string(data), "k-newer") {
		t.Errorf("the file holds the older snapshot after the older write (%v):\n%s", err, data)
	}
}

// A limit write refreshes the snapshot at once, so the file follows it and a
// restart with the store away right after it serves the new row (store leg).
func TestLimitWritesReachTheFileAtOnce(t *testing.T) {
	svc := storeFixture(t)
	dir := t.TempDir()
	svc.class.dir.Store(&dir)
	// Each write is read back from the file before the next one: a later
	// write's refresh must not stand in for an earlier write's own.
	if err := svc.SetTenantRateLimit("t-new", 7, 700, 0); err != nil {
		t.Fatal(err)
	}
	if rps, tpm, _, err := startedFromFile(t, dir).GetTenantRateLimit("t-new"); err != nil || rps != 7 || tpm != 700 {
		t.Errorf("a tenant limit written just before the restart = %d, %d, %v", rps, tpm, err)
	}
	if err := svc.SetTenantModelRateLimit("t-new", "m1", 70); err != nil {
		t.Fatal(err)
	}
	if tpm, err := startedFromFile(t, dir).GetTenantModelRateLimit("t-new", "m1"); err != nil || tpm != 70 {
		t.Errorf("a model limit written just before the restart = %d, %v", tpm, err)
	}
	if err := svc.SetRateLimitDefaults(cmn.RateLimitDefaultsEntry{Scope: cmn.RateLimitScopeGlobal, DefaultTenantRPS: 2}); err != nil {
		t.Fatal(err)
	}
	if g, ok, err := startedFromFile(t, dir).GetRateLimitDefaults(cmn.RateLimitScopeGlobal, ""); err != nil || !ok || g.DefaultTenantRPS != 2 {
		t.Errorf("defaults written just before the restart = %+v, %v, %v", g, ok, err)
	}
}

// A store that answers is reported reachable, at the time of this read,
// after a snapshot served from the file (store leg).
func TestObjectiveListSaysTheStoreAnswered(t *testing.T) {
	svc := storeFixture(t)
	svc.noteStoreRead(svc.class.readGen.Add(1), ErrDBUnavailable)
	before := time.Now().UTC()
	got, err := svc.ObjectiveList()
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != cmn.AIClassSourceStore || !got.StoreReachable || got.StoreLastRead.Before(before) {
		t.Errorf("list read now: source %q reachable %v last read %v (asked at %v)",
			got.Source, got.StoreReachable, got.StoreLastRead, before)
	}
}

// A file of the current version written before the rule knobs of the
// priority bands existed carries nothing about them (they live on the
// load-balancer rules, not here) and is read as it was: the bands add
// nothing a file must hold.
func TestClassLKGCurrentVersionWithoutBandContent(t *testing.T) {
	_, dir := classFixture(t)
	v2 := `{"version":2,"source_timestamp":"2023-11-14T22:15:00Z",` +
		`"objectives":[{"name":"realtime","priority":10}],` +
		`"tenants":{"t1":{"priority_class":"critical"}},"keys":{},` +
		`"tenant_limits":[{"tenant":"t1","rps":25,"tokens_per_min":120000,"burst_pct":40}],` +
		`"tenant_model_limits":[],"rate_limit_defaults":[]}`
	if err := os.WriteFile(filepath.Join(dir, ClassLKGFileName), []byte(v2), 0o600); err != nil {
		t.Fatal(err)
	}
	next := startedFromFile(t, dir)
	snap := next.Classes()
	if _, ok := snap.Objectives["realtime"]; !ok || snap.Tenants["t1"].PriorityClass != "critical" {
		t.Fatalf("the file's classes were not read: %+v", snap)
	}
	if rps, _, _, err := next.GetTenantRateLimit("t1"); err != nil || rps != 25 {
		t.Errorf("the file's limits were not served: %d, %v", rps, err)
	}
}
