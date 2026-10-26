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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/patrickmn/go-cache"

	cmn "github.com/loxilb-io/loxilb/common"
)

func TestObjectiveValidation(t *testing.T) {
	for _, name := range []string{"", "has space", "a,b", "tier:flex", "TIER:fast", "x|y", strings.Repeat("n", 64), "line\nbreak", "ünï"} {
		if _, err := ValidateObjective(cmn.AIObjectiveEntry{Name: name}); err == nil {
			t.Errorf("objective name %q accepted", name)
		}
	}
	for _, name := range []string{"realtime-chat", "batch_v2", "a.b", strings.Repeat("n", 63)} {
		if _, err := ValidateObjective(cmn.AIObjectiveEntry{Name: name}); err != nil {
			t.Errorf("objective name %q refused: %v", name, err)
		}
	}
	got, err := ValidateObjective(cmn.AIObjectiveEntry{Name: "o", Services: []string{"10.0.0.2:80", "10.0.0.1:80", "10.0.0.2:80"}})
	if err != nil || !reflect.DeepEqual(got.Services, []string{"10.0.0.1:80", "10.0.0.2:80"}) {
		t.Errorf("services = %v, %v; want sorted, without the duplicate", got.Services, err)
	}
	for _, svc := range []string{"", "a,b", "a b"} {
		if _, err := ValidateObjective(cmn.AIObjectiveEntry{Name: "o", Services: []string{svc}}); err == nil {
			t.Errorf("service %q accepted", svc)
		}
	}
	if _, err := ValidateObjective(cmn.AIObjectiveEntry{Name: "o", Services: make([]string, cmn.AIObjectiveServicesMax+1)}); err == nil {
		t.Errorf("more services than the bound accepted")
	}
}

func TestClassPolicyValidation(t *testing.T) {
	if _, err := ValidateClassPolicy(cmn.AIClassPolicy{PriorityClass: "urgent"}); err == nil {
		t.Errorf("an unknown class accepted")
	}
	if _, err := ValidateClassPolicy(cmn.AIClassPolicy{DefaultObjective: "a b"}); err == nil {
		t.Errorf("a default objective that is no name accepted")
	}
	if _, err := ValidateClassPolicy(cmn.AIClassPolicy{AllowedObjectives: []string{"ok", "no,good"}}); err == nil {
		t.Errorf("an allowed objective that is no name accepted")
	}
	long := make([]string, cmn.AIObjectiveListMax+1)
	for i := range long {
		long[i] = "o" + strings.Repeat("x", i%10) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}
	if _, err := ValidateClassPolicy(cmn.AIClassPolicy{AllowedObjectives: long}); err == nil {
		t.Errorf("a list over the bound accepted")
	}
	got, err := ValidateClassPolicy(cmn.AIClassPolicy{PriorityClass: "sheddable", AllowedObjectives: []string{"b", "a", "b"}})
	if err != nil || !reflect.DeepEqual(got.AllowedObjectives, []string{"a", "b"}) {
		t.Errorf("allowed = %v, %v; want sorted, without the duplicate", got.AllowedObjectives, err)
	}
	// The body's tier hints are allowed by name; no other tier: name is.
	if got, err := ValidateClassPolicy(cmn.AIClassPolicy{AllowedObjectives: []string{"tier:flex", "batch", "tier:priority", "tier:fast"}}); err != nil ||
		!reflect.DeepEqual(got.AllowedObjectives, []string{"batch", "tier:fast", "tier:flex", "tier:priority"}) {
		t.Errorf("tier names = %v, %v; want accepted and sorted", got.AllowedObjectives, err)
	}
	for _, name := range []string{"tier:auto", "tier:", "TIER:flex", "tier:flex "} {
		if _, err := ValidateClassPolicy(cmn.AIClassPolicy{AllowedObjectives: []string{name}}); err == nil {
			t.Errorf("allowed objective %q accepted", name)
		}
	}
	if _, err := ValidateClassPolicy(cmn.AIClassPolicy{DefaultObjective: "tier:flex"}); err == nil {
		t.Errorf("a tier name accepted as a default objective: a default is an objective row")
	}
	if got, _ := ValidateClassPolicy(cmn.AIClassPolicy{AllowedObjectives: []string{}}); got.AllowedObjectives != nil || !got.IsZero() {
		t.Errorf("an empty list is not the zero policy: %+v", got)
	}
}

// classFixture is a store-less service with a directory for the file.
func classFixture(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc := &Service{Cache: cache.New(time.Minute, time.Minute)}
	svc.class.dir.Store(&dir)
	return svc, dir
}

func sampleSnapshot() *ClassSnapshot {
	return &ClassSnapshot{
		Objectives: map[string]cmn.AIObjectiveEntry{
			"realtime": {Name: "realtime", Priority: 10, Services: []string{"10.0.0.1:80"}, UpdatedAt: time.Unix(1700000000, 0).UTC()},
			"batch":    {Name: "batch", Priority: -5, UpdatedAt: time.Unix(1700000001, 0).UTC()},
		},
		Tenants: map[string]cmn.AIClassPolicy{"t1": {PriorityClass: "standard", DefaultObjective: "batch", AllowedObjectives: []string{"batch", "realtime"}}},
		Keys:    map[string]cmn.AIClassPolicy{"k1": {PriorityClass: "sheddable"}},
		ReadAt:  time.Unix(1700000100, 0).UTC(),
		Source:  cmn.AIClassSourceStore,
	}
}

func TestClassLKGRoundTrip(t *testing.T) {
	svc, dir := classFixture(t)
	want := sampleSnapshot()
	svc.writeClassLKG(want)

	path := filepath.Join(dir, ClassLKGFileName)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the file was not written: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 600", st.Mode().Perm())
	}
	var f map[string]json.RawMessage
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("the file is not JSON: %v", err)
	}
	if string(f["version"]) != "2" {
		t.Errorf("version = %s, want 2", f["version"])
	}
	if !strings.Contains(string(f["source_timestamp"]), "2023-11-14T22:15:00Z") {
		t.Errorf("source_timestamp = %s, want the time the store answered", f["source_timestamp"])
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}

	// A gateway that starts with the store unreachable reads it back.
	next := &Service{Cache: cache.New(time.Minute, time.Minute)}
	next.class.dir.Store(&dir)
	if err := next.RefreshClasses(); !errors.Is(err, ErrDBUnavailable) {
		t.Fatalf("refresh with no store = %v, want ErrDBUnavailable", err)
	}
	if next.Classes() != nil {
		t.Fatalf("a failed refresh published something")
	}
	next.classTick()
	got := next.Classes()
	if got == nil {
		t.Fatalf("the file was not loaded on the first interval without a store")
	}
	if got.Source != cmn.AIClassSourceLKG || !got.ReadAt.Equal(want.ReadAt) {
		t.Errorf("source, read at = %s, %v; want lkg and the file's timestamp", got.Source, got.ReadAt)
	}
	if !reflect.DeepEqual(got.Objectives, want.Objectives) || !reflect.DeepEqual(got.Tenants, want.Tenants) || !reflect.DeepEqual(got.Keys, want.Keys) {
		t.Errorf("content read back differs:\n got %+v\nwant %+v", got, want)
	}
	src, _, ticks := next.ClassSource()
	if src != cmn.AIClassSourceLKG || ticks != 1 {
		t.Errorf("source %s, intervals %d; want lkg, 1", src, ticks)
	}
	next.classTick()
	if _, _, ticks = next.ClassSource(); ticks != 2 {
		t.Errorf("intervals served from the file = %d, want 2", ticks)
	}
}

func TestClassLKGNotRewrittenWhenUnchanged(t *testing.T) {
	svc, dir := classFixture(t)
	snap := sampleSnapshot()
	svc.writeClassLKG(snap)
	path := filepath.Join(dir, ClassLKGFileName)
	first, _ := os.ReadFile(path)

	again := sampleSnapshot()
	again.ReadAt = snap.ReadAt.Add(time.Hour) // a later read of the same rows
	svc.writeClassLKG(again)
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Errorf("an unchanged read rewrote the file")
	}

	again.Keys["k2"] = cmn.AIClassPolicy{PriorityClass: "critical"}
	svc.writeClassLKG(again)
	third, _ := os.ReadFile(path)
	if string(third) == string(second) || !strings.Contains(string(third), "k2") {
		t.Errorf("a changed read did not rewrite the file")
	}
}

func TestClassLKGRefusals(t *testing.T) {
	for name, content := range map[string]string{
		"another version": `{"version":3,"source_timestamp":"2023-11-14T22:15:00Z","objectives":[{"name":"x","priority":1}],"tenants":{},"keys":{}}`,
		"not JSON":        `{"version":1,`,
		"empty":           ``,
	} {
		svc, dir := classFixture(t)
		if err := os.WriteFile(filepath.Join(dir, ClassLKGFileName), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if svc.loadClassLKG() || svc.Classes() != nil {
			t.Errorf("%s: the file was used", name)
		}
	}
	svc, _ := classFixture(t)
	if svc.loadClassLKG() {
		t.Errorf("a missing file was used")
	}
	// The file never replaces what the store answered with.
	svc, dir := classFixture(t)
	svc.writeClassLKG(sampleSnapshot())
	fromStore := &ClassSnapshot{Source: cmn.AIClassSourceStore}
	svc.class.snap.Store(fromStore)
	if svc.loadClassLKG() || svc.Classes() != fromStore {
		t.Errorf("the file replaced a snapshot read from the store (dir %s)", dir)
	}
	// No directory, no file.
	bare := &Service{Cache: cache.New(time.Minute, time.Minute)}
	bare.writeClassLKG(sampleSnapshot())
	if bare.loadClassLKG() {
		t.Errorf("a service with no directory loaded a file")
	}
}

func TestClassChangeHook(t *testing.T) {
	svc, dir := classFixture(t)
	var seen []*ClassSnapshot
	svc.SetClassChangeHook(func(s *ClassSnapshot) { seen = append(seen, s) })
	svc.writeClassLKG(sampleSnapshot())
	_ = dir
	fresh := &Service{Cache: cache.New(time.Minute, time.Minute)}
	fresh.class.dir.Store(&dir)
	fresh.SetClassChangeHook(func(s *ClassSnapshot) { seen = append(seen, s) })
	fresh.classTick()
	if len(seen) != 1 || seen[0].Source != cmn.AIClassSourceLKG {
		t.Errorf("the hook saw %d snapshots; want the one loaded from the file", len(seen))
	}
	if !seen[0].Configured() || (&ClassSnapshot{}).Configured() || (*ClassSnapshot)(nil).Configured() {
		t.Errorf("Configured() is wrong for a loaded, an empty or a nil snapshot")
	}
}

// ---- the store legs (need AIKEY_TEST_DSN) ----

func TestObjectiveStore(t *testing.T) {
	svc := storeFixture(t)
	dir := t.TempDir()
	svc.class.dir.Store(&dir)

	if err := svc.SetObjective(cmn.AIObjectiveEntry{Name: "realtime", Priority: 10, Services: []string{"10.0.0.2:80", "10.0.0.1:80"}}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := svc.SetObjective(cmn.AIObjectiveEntry{Name: "batch", Priority: -5}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := svc.SetObjective(cmn.AIObjectiveEntry{Name: "bad name"}); err == nil {
		t.Errorf("an invalid objective was stored")
	}
	list, err := svc.ListObjectives()
	if err != nil || len(list) != 2 || list[0].Name != "batch" || list[1].Name != "realtime" {
		t.Fatalf("list = %+v, %v; want batch, realtime", list, err)
	}
	if list[1].Priority != 10 || !reflect.DeepEqual(list[1].Services, []string{"10.0.0.1:80", "10.0.0.2:80"}) || list[1].UpdatedAt.IsZero() {
		t.Errorf("realtime read back as %+v", list[1])
	}
	// A local write is in the snapshot at once, and in the file.
	snap := svc.Classes()
	if snap == nil || snap.Source != cmn.AIClassSourceStore || len(snap.Objectives) != 2 || snap.Objectives["batch"].Priority != -5 {
		t.Fatalf("snapshot after two writes = %+v", snap)
	}
	if data, err := os.ReadFile(filepath.Join(dir, ClassLKGFileName)); err != nil || !strings.Contains(string(data), `"realtime"`) {
		t.Errorf("the file does not hold the objectives: %v", err)
	}
	// Replace.
	if err := svc.SetObjective(cmn.AIObjectiveEntry{Name: "batch", Priority: -1}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if p := svc.Classes().Objectives["batch"].Priority; p != -1 {
		t.Errorf("batch priority after a replace = %d, want -1", p)
	}
	if err := svc.DeleteObjective("batch"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.DeleteObjective("batch"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting it again = %v, want not found", err)
	}
	if _, ok := svc.Classes().Objectives["batch"]; ok {
		t.Errorf("a deleted objective is still in the snapshot")
	}
}

func TestTenantAndKeyClassStore(t *testing.T) {
	svc := storeFixture(t)

	// A tenant with limits keeps them when its class is written, and the
	// other way round.
	if err := svc.SetTenantRateLimit("t1", 25, 120000, 40); err != nil {
		t.Fatal(err)
	}
	want := cmn.AIClassPolicy{PriorityClass: "standard", DefaultObjective: "batch", AllowedObjectives: []string{"batch", "realtime"}}
	if err := svc.SetTenantClass("t1", cmn.AIClassPolicy{PriorityClass: "standard", DefaultObjective: "batch", AllowedObjectives: []string{"realtime", "batch"}}); err != nil {
		t.Fatalf("set tenant class: %v", err)
	}
	svc.Cache.Flush()
	if rps, tpm, burst, err := svc.GetTenantRateLimit("t1"); err != nil || rps != 25 || tpm != 120000 || burst != 40 {
		t.Errorf("limits after a class write = %d, %d, %d, %v", rps, tpm, burst, err)
	}
	if err := svc.SetTenantRateLimit("t1", 30, 1000, 0); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.GetTenantClass("t1"); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("class after a limits write = %+v, %v; want %+v", got, err, want)
	}
	// A tenant with a class and no limits has no limits.
	if err := svc.SetTenantClass("t2", cmn.AIClassPolicy{PriorityClass: "sheddable"}); err != nil {
		t.Fatal(err)
	}
	svc.Cache.Flush()
	if rps, tpm, burst, err := svc.GetTenantRateLimit("t2"); err != nil || rps != 0 || tpm != 0 || burst != 0 {
		t.Errorf("limits of a class-only tenant = %d, %d, %d, %v; want zeroes", rps, tpm, burst, err)
	}
	if got, err := svc.GetTenantClass("nobody"); err != nil || !got.IsZero() {
		t.Errorf("class of a tenant with no row = %+v, %v; want none", got, err)
	}
	for _, bad := range []string{"", "a|b"} {
		if err := svc.SetTenantClass(bad, cmn.AIClassPolicy{PriorityClass: "standard"}); err == nil {
			t.Errorf("tenant id %q accepted", bad)
		}
	}
	if err := svc.SetTenantClass("t1", cmn.AIClassPolicy{PriorityClass: "urgent"}); err == nil {
		t.Errorf("an unknown class was stored")
	}

	// Keys.
	_, keyID, err := svc.CreateAPIKey(cmn.ApiKeyEntry{TenantID: "t1", Name: "k", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := svc.GetAPIKeyClass(keyID); err != nil || !got.IsZero() {
		t.Errorf("class of a new key = %+v, %v; want none", got, err)
	}
	keyWant := cmn.AIClassPolicy{PriorityClass: "sheddable", AllowedObjectives: []string{"batch"}}
	if err := svc.SetAPIKeyClass(keyID, keyWant); err != nil {
		t.Fatalf("set key class: %v", err)
	}
	if got, err := svc.GetAPIKeyClass(keyID); err != nil || !reflect.DeepEqual(got, keyWant) {
		t.Errorf("key class = %+v, %v; want %+v", got, err, keyWant)
	}
	if err := svc.SetAPIKeyClass("no-such-key", keyWant); !errors.Is(err, ErrNotFound) {
		t.Errorf("class of a key that does not exist = %v, want not found", err)
	}
	if _, err := svc.GetAPIKeyClass("no-such-key"); !errors.Is(err, ErrNotFound) {
		t.Errorf("reading it = %v, want not found", err)
	}

	// The snapshot holds exactly the rows that set something.
	snap := svc.Classes()
	if len(snap.Tenants) != 2 || !reflect.DeepEqual(snap.Tenants["t1"], want) || snap.Tenants["t2"].PriorityClass != "sheddable" {
		t.Errorf("tenants in the snapshot = %+v", snap.Tenants)
	}
	if len(snap.Keys) != 1 || !reflect.DeepEqual(snap.Keys[keyID], keyWant) {
		t.Errorf("keys in the snapshot = %+v", snap.Keys)
	}
	// Clearing a class takes the row out of it.
	if err := svc.SetTenantClass("t2", cmn.AIClassPolicy{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.Classes().Tenants["t2"]; ok {
		t.Errorf("a tenant whose class was cleared is still in the snapshot")
	}
}

// During an outage the snapshot in force stays, writes are refused, and the
// file is not needed.
func TestClassOutage(t *testing.T) {
	svc := storeFixture(t)
	dir := t.TempDir()
	svc.class.dir.Store(&dir)
	if err := svc.SetObjective(cmn.AIObjectiveEntry{Name: "realtime", Priority: 10}); err != nil {
		t.Fatal(err)
	}
	before := svc.Classes()
	svc.mu.Lock()
	svc.db = nil
	svc.mu.Unlock()

	svc.classTick()
	if svc.Classes() != before {
		t.Errorf("an outage replaced the snapshot read from the store")
	}
	if src, _, ticks := svc.ClassSource(); src != cmn.AIClassSourceStore || ticks != 0 {
		t.Errorf("source %s, file intervals %d during an outage after a read; want store, 0", src, ticks)
	}
	if err := svc.SetObjective(cmn.AIObjectiveEntry{Name: "x", Priority: 1}); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("set objective during an outage = %v, want ErrDBUnavailable", err)
	}
	if err := svc.DeleteObjective("realtime"); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("delete objective during an outage = %v, want ErrDBUnavailable", err)
	}
	if err := svc.SetTenantClass("t1", cmn.AIClassPolicy{PriorityClass: "standard"}); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("set tenant class during an outage = %v, want ErrDBUnavailable", err)
	}
	if _, err := svc.ListObjectives(); !errors.Is(err, ErrDBUnavailable) {
		t.Errorf("list during an outage = %v, want ErrDBUnavailable", err)
	}
}
