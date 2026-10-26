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
package loxinet

import (
	"strings"
	"testing"

	"github.com/loxilb-io/loxilb/pkg/aikey"

	cmn "github.com/loxilb-io/loxilb/common"
)

// classSnapshotFor stands a class snapshot in for the store's until the
// test ends.
func classSnapshotFor(t *testing.T, snap *aikey.ClassSnapshot) {
	t.Helper()
	prev := aiClassSnapshotSrc
	aiClassSnapshotSrc = func() *aikey.ClassSnapshot { return snap }
	t.Cleanup(func() { aiClassSnapshotSrc = prev })
}

// The export hands the gate what the resolver decided: every field of the
// answer crosses the C struct unchanged, for each kind of request the
// resolver tells apart.
func TestClassExportAnswersAsTheResolver(t *testing.T) {
	snap := classSnap()
	classSnapshotFor(t, snap)
	for _, in := range []classInput{
		{tenant: "nobody", svc: classSvc},
		{tenant: "vip", svc: classSvc},
		{tenant: "vip", svc: classSvc, hint: "batch"},
		{tenant: "vip", svc: classSvc, hint: "urgent"},
		{tenant: "premium", svc: classSvc, hint: "realtime"},
		{tenant: "premium", svc: classSvc, hint: "scoped"},
		{tenant: "premium", svc: classSvc, hint: "elsewhere"},
		{tenant: "premium", svc: classSvc, hint: "ghost"},
		{tenant: "capped", svc: classSvc, hint: "realtime"},
		{tenant: "capped", svc: classSvc, hint: "urgent"},
		{tenant: "free", svc: classSvc},
		{tenant: "vip", svc: classSvc, keyID: "k-shed"},
		{tenant: "vip", svc: classSvc, keyID: "k-narrow", hint: "realtime"},
		{tenant: "capped", svc: classSvc, keyID: "k-raise"},
		{tenant: "premium", svc: classSvc, tier: aiTierFlex},
		{tenant: "premium", svc: classSvc, tier: aiTierPriority},
		{tenant: "capped", svc: classSvc, tier: aiTierFast},
		{tenant: "premium", svc: classSvc, hint: "ghost", tier: aiTierPriority},
	} {
		want := classResolve(snap, in)
		rc, got := aiClassResolveC(in, "", "")
		if rc != 0 {
			t.Errorf("%+v: rc %d", in, rc)
		}
		if got != want {
			t.Errorf("%+v: the export answered %+v, the resolver %+v", in, got, want)
		}
	}
}

// With no snapshot (no store, or nothing read yet) the export answers. A
// request that asks for nothing is standard, and the struct it was handed
// comes back cleared; one that names an objective named one nobody has.
func TestClassExportWithoutSnapshot(t *testing.T) {
	classSnapshotFor(t, nil)
	rc, got := aiClassResolveC(classInput{tenant: "vip", svc: classSvc}, "", "")
	if rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	if want := (classAnswer{}); got != want {
		t.Errorf("answer %+v, want standard with nothing applied", got)
	}
	in := classInput{tenant: "vip", svc: classSvc, hint: "realtime", tier: aiTierPriority}
	rc, got = aiClassResolveC(in, "", "")
	if want := classResolve(nil, in); rc != 0 || got != want || !got.denied || got.priority != 0 {
		t.Errorf("rc %d, answer %+v, want %+v (standard, the objective refused)", rc, got, want)
	}
}

// A binding and a JWT profile the gateway does not know put no cap on a
// request: an unknown name must not read as the lowest class.
func TestClassExportUnknownBindingAndProfilePutNoCap(t *testing.T) {
	snap := classSnap()
	classSnapshotFor(t, snap)
	in := classInput{tenant: "vip", svc: classSvc}
	rc, got := aiClassResolveC(in, "no-such-binding", "no-such-profile")
	if rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	if want := classResolve(snap, in); got != want {
		t.Errorf("answer %+v, want %+v", got, want)
	}
}

// An objective name of the longest length the API accepts fits the C
// field whole.
func TestClassExportCarriesTheLongestObjectiveName(t *testing.T) {
	name := strings.Repeat("o", cmn.AIObjectiveNameMax)
	snap := &aikey.ClassSnapshot{
		Objectives: map[string]cmn.AIObjectiveEntry{name: {Name: name, Priority: -3}},
		Tenants:    map[string]cmn.AIClassPolicy{"t": {AllowedObjectives: []string{name}}},
	}
	classSnapshotFor(t, snap)
	rc, got := aiClassResolveC(classInput{tenant: "t", svc: classSvc, hint: name}, "", "")
	if rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	if got.objective != name || got.priority != -3 || got.class != aiClassNumSheddable {
		t.Errorf("answer %+v", got)
	}
}
