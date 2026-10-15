package snapshot

import (
	"bytes"
	"errors"
	cmn "github.com/loxilb-io/loxilb/common"
	"os"
	"reflect"
	"strings"
	"testing"
)

func assignmentFixture() []cmn.GoBGPPolicyApply {
	return []cmn.GoBGPPolicyApply{{NeighIPAddress: "global", PolicyType: "import", Polices: []string{"second", "first"}, RouteAction: "reject"}, {NeighIPAddress: "192.0.2.2", PolicyType: "export", RouteAction: "reject"}}
}
func TestBGPAssignmentsSaveChangeRestore(t *testing.T) {
	h := newMockHooks()
	h.bgpAssignments = assignmentFixture()
	doc, err := Capture(h, "test", "test", TriggerManual, []string{DomainBGPPolicyAssignment})
	if err != nil {
		t.Fatal(err)
	}
	raw := encodeDoc(t, doc)
	h.bgpAssignments = []cmn.GoBGPPolicyApply{{NeighIPAddress: "global", PolicyType: "export", Polices: []string{"drift"}, RouteAction: "accept"}}
	result, err := newTestEngine(h, t.TempDir()).Restore(raw, RestoreOptions{Mode: ModeCommit})
	if err != nil || result.Result != ResultOK {
		t.Fatalf("restore: %+v %v", result, err)
	}
	expected := assignmentFixture()
	_ = sortByJSON(expected)
	if !reflect.DeepEqual(h.bgpAssignments, expected) {
		t.Fatalf("lost binding fields/order: %+v", h.bgpAssignments)
	}
}
func TestBGPAssignmentsFailureRollsBack(t *testing.T) {
	h := newMockHooks()
	h.bgpAssignments = assignmentFixture()
	doc, err := Capture(h, "test", "test", TriggerManual, []string{DomainBGPPolicyAssignment})
	if err != nil {
		t.Fatal(err)
	}
	before := []cmn.GoBGPPolicyApply{{NeighIPAddress: "global", PolicyType: "export", RouteAction: "reject"}}
	h.bgpAssignments = append([]cmn.GoBGPPolicyApply(nil), before...)
	h.failOn["NetGoBGPPolicyApplyAdd"] = errors.New("injected assignment failure")
	result, err := newTestEngine(h, t.TempDir()).Restore(encodeDoc(t, doc), RestoreOptions{Mode: ModeCommit})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result == ResultOK {
		t.Fatalf("failure accepted: %+v", result)
	}
	if !reflect.DeepEqual(h.bgpAssignments, before) {
		t.Fatalf("rollback lost prior binding: %+v", h.bgpAssignments)
	}
}
func TestBGPAssignmentFieldsAffectVerification(t *testing.T) {
	base := Domains{BGPPolicyAssignment: assignmentFixture()}
	_, want, err := DomainContent(DomainBGPPolicyAssignment, &base)
	if err != nil {
		t.Fatal(err)
	}
	cases := []func(*cmn.GoBGPPolicyApply){func(a *cmn.GoBGPPolicyApply) { a.NeighIPAddress = "changed" }, func(a *cmn.GoBGPPolicyApply) { a.PolicyType = "export" }, func(a *cmn.GoBGPPolicyApply) { a.Polices = []string{"first", "second"} }, func(a *cmn.GoBGPPolicyApply) { a.RouteAction = "accept" }}
	for i, change := range cases {
		d := Domains{BGPPolicyAssignment: assignmentFixture()}
		change(&d.BGPPolicyAssignment[0])
		_, got, err := DomainContent(DomainBGPPolicyAssignment, &d)
		if err != nil || got == want {
			t.Fatalf("field %d invisible to verify: %s %v", i, got, err)
		}
	}
}
func TestSchema111DoesNotWipeBGPAssignments(t *testing.T) {
	raw, err := os.ReadFile("testdata/snapshot-v1.11.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksum(doc); err != nil {
		t.Fatal(err)
	}
	h := newMockHooks()
	h.bgpAssignments = assignmentFixture()
	// Scope to a domain the legacy document actually covers. Its explicit
	// coverage must never be widened to the newly introduced bindings.
	doc.IncludedDomains = []string{DomainHalfClose}
	doc.Domains = Domains{HalfClose: doc.Domains.HalfClose}
	result, err := newTestEngine(h, t.TempDir()).Restore(encodeDoc(t, doc), RestoreOptions{Mode: ModeCommit})
	if err != nil || result.Result != ResultOK {
		t.Fatalf("legacy restore: %+v %v", result, err)
	}
	if !reflect.DeepEqual(h.bgpAssignments, assignmentFixture()) {
		t.Fatal("legacy document changed unrecorded bindings")
	}
}

func TestBGPReplacementRefusesToDestroyUnselectedAssignments(t *testing.T) {
	h := newMockHooks()
	h.bgpAssignments = assignmentFixture()
	doc := NewDocument("test", "test", TriggerManual)
	doc.IncludedDomains = []string{DomainBGP}
	result, err := newTestEngine(h, t.TempDir()).Restore(encodeDoc(t, doc), RestoreOptions{Mode: ModeCommit})
	if err != nil || len(result.Errors) == 0 || result.Result == ResultOK {
		t.Fatalf("expected preflight rejection: %+v %v", result, err)
	}
	if call := firstMutatingCall(h.Calls); call != "" {
		t.Fatalf("uncovered assignments were touched: %s", call)
	}
	if !reflect.DeepEqual(h.bgpAssignments, assignmentFixture()) {
		t.Fatal("live assignment changed")
	}
}

func TestBGPTeardownDetachesBeforeDefinitionsAndSets(t *testing.T) {
	h := newMockHooks()
	fixture := sampleDocument()
	for _, e := range ApplyOrder() {
		if e.Name == DomainBGP || e.Name == DomainBGPPolicyAssignment {
			if _, _, err := e.Apply(h, fixture, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.Calls = nil
	if _, err := Wipe(h, []string{DomainBGP, DomainBGPPolicyAssignment}); err != nil {
		t.Fatal(err)
	}
	assignment, definition, set := -1, -1, -1
	for i, c := range h.Calls {
		if strings.HasPrefix(c, "NetGoBGPPolicyApplyDel") {
			assignment = i
		}
		if strings.HasPrefix(c, "NetGoBGPPolicyDefinitionDel") {
			definition = i
		}
		if strings.HasPrefix(c, "NetGoBGPPolicyDefinedSetDel") {
			set = i
		}
	}
	if assignment < 0 || definition < 0 || set < 0 || assignment >= definition || definition >= set {
		t.Fatalf("dependency teardown order: %v", h.Calls)
	}
}

func TestBFDRuntimeHealthDoesNotChangeSnapshotDigest(t *testing.T) {
	a := Domains{BFD: []cmn.BFDMod{{Instance: "test", State: "Up", Interval: 1000000, RetryCount: 5}}}
	b := Domains{BFD: []cmn.BFDMod{{Instance: "test", State: "Down", Interval: 1000000, RetryCount: 5}}}
	left, err := DomainDigest(DomainBFD, &a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := DomainDigest(DomainBFD, &b)
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatal("runtime peer health must not be restored or compared as configuration")
	}
}

type corruptBGPGlobalHooks struct{ *mockHooks }

func (h corruptBGPGlobalHooks) NetGoBGPGCAdd(c *cmn.GoBGPGlobalConfig) (int, error) {
	wrong := *c
	wrong.RouterID = "192.0.2.99"
	h.bgpGC = &wrong
	return 0, nil
}
func TestBGPGlobalFieldsCannotEscapeVerification(t *testing.T) {
	h := corruptBGPGlobalHooks{newMockHooks()}
	doc := NewDocument("test", "test", TriggerManual)
	doc.IncludedDomains = []string{DomainBGP}
	doc.Domains.BGP.GlobalConfig = &cmn.GoBGPGlobalConfig{LocalAs: 65118, RouterID: "192.0.2.18", ListenPort: 1179}
	result, err := newTestEngine(h, t.TempDir()).Restore(encodeDoc(t, doc), RestoreOptions{Mode: ModeCommit})
	if err != nil {
		t.Fatal(err)
	}
	if result.Result == ResultOK || !strings.Contains(strings.Join(result.Errors, " "), "global configuration differs") {
		t.Fatalf("global field silently dropped: %+v", result)
	}
}
func TestBGPGlobalPreparedBeforeLBAdvertisement(t *testing.T) {
	h := newMockHooks()
	doc := NewDocument("test", "test", TriggerManual)
	doc.IncludedDomains = []string{DomainBGP, DomainLoadBalancer}
	doc.Domains.BGP.GlobalConfig = &cmn.GoBGPGlobalConfig{LocalAs: 65118, RouterID: "192.0.2.18", ListenPort: 1179}
	doc.Domains.LoadBalancer = restoreDoc("test").Domains.LoadBalancer
	result, err := newTestEngine(h, t.TempDir()).Restore(encodeDoc(t, doc), RestoreOptions{Mode: ModeCommit})
	if err != nil || result.Result != ResultOK {
		t.Fatalf("%+v %v", result, err)
	}
	prepare, lb := -1, -1
	for i, c := range h.Calls {
		if c == "NetGoBGPGCReplace" {
			prepare = i
		}
		if strings.HasPrefix(c, "NetLbRuleAdd") {
			lb = i
		}
	}
	if prepare < 0 || lb < 0 || prepare >= lb {
		t.Fatalf("LB advertised before speaker configuration: %v", h.Calls)
	}
}

func TestBGPAssignmentBootRetrySkipsOnlyIdenticalContent(t *testing.T) {
	h := newMockHooks()
	d := &Document{Domains: Domains{BGPPolicyAssignment: assignmentFixture()}}
	if _, _, err := applyBGPPolicyAssignment(h, d, true); err != nil {
		t.Fatal(err)
	}
	h.Calls = nil
	n, skipped, err := applyBGPPolicyAssignment(h, d, true)
	if err != nil || n != 0 || skipped != 2 {
		t.Fatalf("identical retry: %d %d %v", n, skipped, err)
	}
	for _, call := range h.Calls {
		if strings.HasPrefix(call, "NetGoBGPPolicyApplyAdd") {
			t.Fatal("duplicate assignment appended")
		}
	}
	d.Domains.BGPPolicyAssignment[0].RouteAction = "accept"
	if _, _, err := applyBGPPolicyAssignment(h, d, true); err == nil {
		t.Fatal("conflicting boot binding was accepted")
	}
}
func TestBGPAssignmentDeleteContinuesAfterOneFailure(t *testing.T) {
	h := newMockHooks()
	h.bgpAssignments = assignmentFixture()
	h.failOn["NetGoBGPPolicyApplyDel"] = errors.New("injected reset failure")
	n, err := deleteBGPPolicyAssignment(h)
	if err == nil || n != 1 || len(h.bgpAssignments) != 1 {
		t.Fatalf("remaining assignments not attempted: n=%d err=%v left=%v", n, err, h.bgpAssignments)
	}
}

type disabledAssignmentHooks struct{ *mockHooks }

func (h disabledAssignmentHooks) NetGoBGPPolicyApplyGet() ([]cmn.GoBGPPolicyApply, error) {
	h.log("NetGoBGPPolicyApplyGet")
	return nil, errors.New("loxilb BGP mode is disabled")
}
func (h disabledAssignmentHooks) NetGoBGPPolicyApplyAdd(*cmn.GoBGPPolicyApply) (int, error) {
	h.log("NetGoBGPPolicyApplyAdd")
	return -1, errors.New("loxilb BGP mode is disabled")
}

func TestEmptyBGPAssignmentBootDoesNotRequireSpeaker(t *testing.T) {
	h := disabledAssignmentHooks{newMockHooks()}
	for _, items := range [][]cmn.GoBGPPolicyApply{nil, {}} {
		h.Calls = nil
		n, skipped, err := applyBGPPolicyAssignment(h, &Document{Domains: Domains{BGPPolicyAssignment: items}}, true)
		if err != nil || n != 0 || skipped != 0 {
			t.Fatalf("empty boot apply: %d %d %v", n, skipped, err)
		}
		if len(h.Calls) != 0 {
			t.Fatalf("empty boot queried an optional subsystem: %v", h.Calls)
		}
	}
}

func TestNonemptyBGPAssignmentBootStillRequiresSpeaker(t *testing.T) {
	h := disabledAssignmentHooks{newMockHooks()}
	doc := &Document{Domains: Domains{BGPPolicyAssignment: assignmentFixture()}}
	if _, _, err := applyBGPPolicyAssignment(h, doc, true); err == nil {
		t.Fatal("nonempty desired assignments silently dropped")
	}
}

func TestSnapshotBootWithoutBGPReplaysOtherDomains(t *testing.T) {
	h := disabledAssignmentHooks{newMockHooks()}
	doc := restoreDoc("restored")
	if len(doc.Domains.BGPPolicyAssignment) != 0 {
		t.Fatal("fixture must have no BGP assignments")
	}
	result, err := newTestEngine(h, t.TempDir()).Restore(encodeDoc(t, doc), RestoreOptions{Boot: true})
	if err != nil || result.Result != ResultOK {
		t.Fatalf("non-BGP boot rejected: %+v %v", result, err)
	}
	if len(h.lbRules) == 0 {
		t.Fatal("boot failed to replay the load balancer")
	}
}
