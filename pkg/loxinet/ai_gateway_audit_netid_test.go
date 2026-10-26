/*
 * Copyright (c) 2026 LoxiLB Authors
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
	"testing"

	"github.com/loxilb-io/loxilb/pkg/audit"
)

// A request with no binding is recorded as it always was: the actor says
// nothing about a network identity, whatever the source argument holds.
func TestAINetActorWithoutABindingChangesNothing(t *testing.T) {
	// A bearer token is not in this list: it changes the mode on its own.
	for _, src := range []int{aiAuthSrcCredential, aiAuthSrcNetwork} {
		keyed := aiDataActor("acme", "u-1", "k-1")
		if got := aiNetActor(keyed, src, ""); got != keyed {
			t.Errorf("src %d: keyed actor changed to %+v", src, got)
		}
		keyless := aiDataActor("", "", "")
		if got := aiNetActor(keyless, src, ""); got != keyless {
			t.Errorf("src %d: keyless actor changed to %+v", src, got)
		}
	}
}

// The binding is named whenever it matched; the mode reads network only
// when the binding decided the tenant.
func TestAINetActorNamesTheBinding(t *testing.T) {
	a := aiNetActor(aiDataActor("line-1-tenant", "", ""), aiAuthSrcNetwork, "line-1")
	if a.Auth != audit.AuthNetwork || a.NetIdentity != "line-1" || a.Tenant != "line-1-tenant" {
		t.Errorf("network-decided actor = %+v", a)
	}

	// A key decided; the binding matched beside it.
	a = aiNetActor(aiDataActor("acme", "", "k-1"), aiAuthSrcCredential, "line-1")
	if a.Auth != audit.AuthAPIKey || a.NetIdentity != "line-1" {
		t.Errorf("credential-decided actor = %+v, want apikey with the binding named", a)
	}

	// A bearer token decided and the binding matched beside it.
	a = aiNetActor(aiDataActor("acme", "alice", ""), aiAuthSrcBearer, "line-1")
	if a.Auth != audit.AuthBearer || a.NetIdentity != "line-1" {
		t.Errorf("token-decided actor = %+v, want bearer with the binding named", a)
	}
}

// A request a validated bearer token decided reads as bearer on each of its
// three records, with no binding anywhere; an API key and an anonymous
// request read as they always did.
func TestBearerTokenIsRecordedAsBearer(t *testing.T) {
	a := aiNetActor(aiDataActor("acme", "alice", ""), aiAuthSrcBearer, "")
	if a.Auth != audit.AuthBearer || a.NetIdentity != "" || a.Tenant != "acme" || a.User != "alice" {
		t.Errorf("bearer actor = %+v", a)
	}
	if a := aiNetActor(aiDataActor("acme", "", "k-1"), aiAuthSrcCredential, ""); a.Auth != audit.AuthAPIKey {
		t.Errorf("API-key actor = %+v", a)
	}
	if a := aiNetActor(aiDataActor("", "", ""), aiAuthSrcCredential, ""); a.Auth != audit.AuthNone {
		t.Errorf("anonymous actor = %+v", a)
	}

	_, drain := startTrail(t)
	if !emitAIComplete(aiCompleteRecord{
		RequestID: "req-jwt", TenantID: "acme", UserID: "alice", SvcIdent: "10.0.0.1:8080",
		ModelName: "llama-3", StatusCode: 200, WorkerID: 1, ClientIP: "10.20.0.7",
		AuthSrc: aiAuthSrcBearer,
	}) {
		t.Fatal("emit refused")
	}
	if !emitAISettle(aiSettleRecord{
		RequestID: "req-jwt", TenantID: "acme", UserID: "alice", SvcIdent: "10.0.0.1:8080",
		ModelName: "llama-3", TokensIn: 7, TokensOut: 3, Allowed: true, WorkerID: 1,
		AuthSrc: aiAuthSrcBearer,
	}) {
		t.Fatal("emit refused")
	}
	if !emitAIDeny(aiDenyRecord{
		RequestID: "req-jwt-denied", TenantID: "acme", UserID: "alice", SvcIdent: "10.0.0.1:8080",
		Stage: aiStageRateLimit, HTTPStatus: 429, ErrorCode: "rate_limit_exceeded",
		WorkerID: 1, ClientIP: "10.20.0.7",
		AuthSrc: aiAuthSrcBearer,
	}) {
		t.Fatal("emit refused")
	}
	// A token that did not validate decided nothing.
	if !emitAIDeny(aiDenyRecord{
		RequestID: "req-jwt-invalid", SvcIdent: "10.0.0.1:8080",
		Stage: aiStageAuth, HTTPStatus: 401, ErrorCode: "invalid_token",
		WorkerID: 1, ClientIP: "10.20.0.7",
	}) {
		t.Fatal("emit refused")
	}

	seen := map[string]string{}
	for _, l := range drain() {
		et := l.str("event_type")
		if et != eventAIComplete && et != eventAISettle && et != eventAIDeny {
			continue
		}
		a := l.actor()
		seen[et+" "+l.str("request_id")] = a.str("auth")
		if _, found := a["net_identity"]; found {
			t.Errorf("%s: net_identity present with no binding", et)
		}
		if _, found := a["key_id"]; found {
			t.Errorf("%s: key_id present on a token request", et)
		}
	}
	want := map[string]string{
		eventAIComplete + " req-jwt":     "bearer",
		eventAISettle + " req-jwt":       "bearer",
		eventAIDeny + " req-jwt-denied":  "bearer",
		eventAIDeny + " req-jwt-invalid": "none",
	}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("%s: auth %q, want %q", k, seen[k], v)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("records seen: %v", seen)
	}
}

func TestEmitAICompleteCarriesTheNetworkIdentity(t *testing.T) {
	_, drain := startTrail(t)

	if !emitAIComplete(aiCompleteRecord{
		RequestID: "req-net", TenantID: "line-1-tenant", SvcIdent: "10.0.0.1:8080",
		ModelName: "llama-3", StatusCode: 200, WorkerID: 1, ClientIP: "10.20.0.7",
		AuthSrc: aiAuthSrcNetwork, NetIdentity: "line-1",
	}) {
		t.Fatal("emit refused")
	}
	if !emitAIComplete(aiCompleteRecord{
		RequestID: "req-key", TenantID: "acme", KeyID: "k-1", SvcIdent: "10.0.0.1:8080",
		ModelName: "llama-3", StatusCode: 200, WorkerID: 1, ClientIP: "10.20.0.7",
		AuthSrc: aiAuthSrcCredential, NetIdentity: "line-1",
	}) {
		t.Fatal("emit refused")
	}
	if !emitAIComplete(aiCompleteRecord{
		RequestID: "req-plain", TenantID: "acme", KeyID: "k-1", SvcIdent: "10.0.0.1:8080",
		ModelName: "llama-3", StatusCode: 200, WorkerID: 1, ClientIP: "10.20.0.7",
	}) {
		t.Fatal("emit refused")
	}

	byReq := map[string]auditLine{}
	for _, l := range ofEvent(drain(), eventAIComplete) {
		byReq[l.str("request_id")] = l
	}
	if len(byReq) != 3 {
		t.Fatalf("want 3 completion records, got %d", len(byReq))
	}

	a := byReq["req-net"].actor()
	if a.str("auth") != "network" || a.str("net_identity") != "line-1" || a.str("tenant") != "line-1-tenant" {
		t.Errorf("network-decided record actor = %v", a)
	}
	if _, found := a["key_id"]; found {
		t.Error("a network-decided record carries a key_id")
	}

	a = byReq["req-key"].actor()
	if a.str("auth") != "apikey" || a.str("net_identity") != "line-1" || a.str("key_id") != "k-1" {
		t.Errorf("credential-decided record actor = %v", a)
	}

	a = byReq["req-plain"].actor()
	if _, found := a["net_identity"]; found {
		t.Error("net_identity present on a request no binding matched")
	}
	if a.str("auth") != "apikey" {
		t.Errorf("auth = %q on a plain keyed request", a.str("auth"))
	}
}

func TestEmitAIDenyCarriesTheNetworkIdentity(t *testing.T) {
	_, drain := startTrail(t)

	// A valid key from a network bound to another tenant.
	if !emitAIDeny(aiDenyRecord{
		RequestID: "req-mismatch", TenantID: "acme", KeyID: "k-1", SvcIdent: "10.0.0.1:8080",
		Stage: aiStageAuth, HTTPStatus: 403, ErrorCode: "network_tenant_mismatch",
		WorkerID: 1, ClientIP: "10.20.0.7",
		AuthSrc: aiAuthSrcCredential, NetIdentity: "line-1",
	}) {
		t.Fatal("emit refused")
	}
	// The binding's tenant, over its quota.
	if !emitAIDeny(aiDenyRecord{
		RequestID: "req-quota", TenantID: "line-1-tenant", SvcIdent: "10.0.0.1:8080",
		Stage: aiStageReserve, HTTPStatus: 429, ErrorCode: "token_quota_would_exceed",
		WorkerID: 1, ClientIP: "10.20.0.7",
		AuthSrc: aiAuthSrcNetwork, NetIdentity: "line-1",
	}) {
		t.Fatal("emit refused")
	}
	// The resolver could not answer on a service that fails closed: no
	// binding is known, so none is named.
	if !emitAIDeny(aiDenyRecord{
		RequestID: "req-fault", SvcIdent: "10.0.0.1:8080",
		Stage: aiStageAuth, HTTPStatus: 503, ErrorCode: "netid_unavailable",
		WorkerID: 1, ClientIP: "10.20.0.7",
	}) {
		t.Fatal("emit refused")
	}

	byReq := map[string]auditLine{}
	for _, l := range ofEvent(drain(), eventAIDeny) {
		byReq[l.str("request_id")] = l
	}
	if len(byReq) != 3 {
		t.Fatalf("want 3 deny records, got %d", len(byReq))
	}

	r := byReq["req-mismatch"]
	if a := r.actor(); a.str("auth") != "apikey" || a.str("net_identity") != "line-1" || a.str("tenant") != "acme" {
		t.Errorf("mismatch actor = %v, want the key's identity with the binding that refused it", a)
	}
	if o := r.outcome(); o.num("status") != 403 || o.str("reason") != string(audit.ReasonAuthz) {
		t.Errorf("mismatch outcome = %v, want 403/authz: the credential is valid", o)
	}

	r = byReq["req-quota"]
	if a := r.actor(); a.str("auth") != "network" || a.str("net_identity") != "line-1" || a.str("tenant") != "line-1-tenant" {
		t.Errorf("quota actor = %v", a)
	}
	if o := r.outcome(); o.str("reason") != string(audit.ReasonQuota) {
		t.Errorf("quota outcome = %v", o)
	}

	r = byReq["req-fault"]
	if a := r.actor(); a.str("auth") != "none" {
		t.Errorf("fault actor = %v", a)
	} else if _, found := a["net_identity"]; found {
		t.Error("net_identity present where the resolver named no binding")
	}
	if o := r.outcome(); o.num("status") != 503 || o.str("reason") != string(audit.ReasonAdmission) {
		t.Errorf("fault outcome = %v, want 503/admission: the gateway's fault, not a failed credential", o)
	}
}

func TestNetIDCompleteOutcome(t *testing.T) {
	if got := netIDCompleteOutcome(aiAuthSrcNetwork); got != "admitted" {
		t.Errorf("network-decided = %q", got)
	}
	if got := netIDCompleteOutcome(aiAuthSrcCredential); got != "matched" {
		t.Errorf("credential-decided = %q", got)
	}
}

// One request writes up to three records. A reader joins them on the
// request id, so the charge must name the actor the completion names.
func TestEmitAISettleCarriesTheNetworkIdentity(t *testing.T) {
	_, drain := startTrail(t)

	if !emitAISettle(aiSettleRecord{
		RequestID: "req-net", TenantID: "line-1-tenant", SvcIdent: "10.0.0.1:8080",
		ModelName: "llama-3", TokensIn: 7, TokensOut: 3, Allowed: true, WorkerID: 1,
		AuthSrc: aiAuthSrcNetwork, NetIdentity: "line-1",
	}) {
		t.Fatal("emit refused")
	}
	if !emitAIComplete(aiCompleteRecord{
		RequestID: "req-net", TenantID: "line-1-tenant", SvcIdent: "10.0.0.1:8080",
		ModelName: "llama-3", StatusCode: 200, WorkerID: 1, ClientIP: "10.20.0.7",
		AuthSrc: aiAuthSrcNetwork, NetIdentity: "line-1",
	}) {
		t.Fatal("emit refused")
	}
	if !emitAISettle(aiSettleRecord{
		RequestID: "req-key", TenantID: "acme", KeyID: "k-1", SvcIdent: "10.0.0.1:8080",
		ModelName: "llama-3", TokensIn: 7, TokensOut: 3, Allowed: true, WorkerID: 1,
		AuthSrc: aiAuthSrcCredential, NetIdentity: "line-1",
	}) {
		t.Fatal("emit refused")
	}
	if !emitAISettle(aiSettleRecord{
		RequestID: "req-plain", TenantID: "acme", KeyID: "k-1", SvcIdent: "10.0.0.1:8080",
		ModelName: "llama-3", TokensIn: 7, TokensOut: 3, Allowed: true, WorkerID: 1,
	}) {
		t.Fatal("emit refused")
	}

	lines := drain()
	settle := map[string]auditLine{}
	for _, l := range ofEvent(lines, eventAISettle) {
		settle[l.str("request_id")] = l
	}
	if len(settle) != 3 {
		t.Fatalf("want 3 settle records, got %d", len(settle))
	}

	a := settle["req-net"].actor()
	if a.str("auth") != "network" || a.str("net_identity") != "line-1" || a.str("tenant") != "line-1-tenant" {
		t.Errorf("network-decided settle actor = %v", a)
	}
	complete := ofEvent(lines, eventAIComplete)
	if len(complete) != 1 {
		t.Fatalf("want 1 completion record, got %d", len(complete))
	}
	c := complete[0].actor()
	for _, f := range []string{"auth", "net_identity", "tenant"} {
		if a.str(f) != c.str(f) {
			t.Errorf("%s: settle %q, completion %q on one request", f, a.str(f), c.str(f))
		}
	}

	a = settle["req-key"].actor()
	if a.str("auth") != "apikey" || a.str("net_identity") != "line-1" || a.str("key_id") != "k-1" {
		t.Errorf("credential-decided settle actor = %v", a)
	}

	a = settle["req-plain"].actor()
	if _, found := a["net_identity"]; found {
		t.Error("net_identity present on the charge of a request no binding matched")
	}
	if a.str("auth") != "apikey" {
		t.Errorf("auth = %q on a plain keyed charge", a.str("auth"))
	}
}

// A completion names the class its request was given, and only when the
// class stage ran for it: a request handled while no class is configured
// leaves a record with none of the class fields.
func TestEmitAICompleteCarriesTheClass(t *testing.T) {
	_, drain := startTrail(t)
	base := aiCompleteRecord{TenantID: "acme", SvcIdent: "10.0.0.1:8080", ModelName: "llama-3", StatusCode: 200, WorkerID: 1}

	plain := base
	plain.RequestID = "req-plain"
	shed := base
	shed.RequestID, shed.Priority, shed.ClassInfo, shed.Objective = "req-shed", -5, aiClassInfoSet|aiClassNumSheddable, "batch"
	std := base
	std.RequestID, std.ClassInfo = "req-std", aiClassInfoSet|aiClassNumStandard|aiClassInfoDenied
	crit := base
	crit.RequestID, crit.Priority, crit.ClassInfo, crit.Objective = "req-crit", 1, aiClassInfoSet|aiClassNumCritical, "tier:priority"
	unread := base
	unread.RequestID, unread.ClassInfo = "req-unread", aiClassInfoSet|aiClassNumStandard|aiClassInfoHintUnread
	// The class bits without the "ran" bit say nothing.
	stale := base
	stale.RequestID, stale.Priority, stale.ClassInfo, stale.Objective = "req-stale", 9, aiClassNumCritical|aiClassInfoDenied, "left-over"
	for _, c := range []aiCompleteRecord{plain, shed, std, crit, unread, stale} {
		if !emitAIComplete(c) {
			t.Fatal("emit refused")
		}
	}

	classKeys := []string{"priority_class", "priority", "objective", "objective_denied", "hint_not_applied"}
	seen := 0
	for _, l := range drain() {
		if l.str("event_type") != eventAIComplete {
			continue
		}
		seen++
		d := l.detail()
		switch id := l.str("request_id"); id {
		case "req-plain", "req-stale":
			for _, k := range classKeys {
				if _, found := d[k]; found {
					t.Errorf("%s: %q present on a request the class stage did not run for", id, k)
				}
			}
		case "req-shed":
			if d.str("priority_class") != "sheddable" || d.num("priority") != -5 || d.str("objective") != "batch" {
				t.Errorf("%s: detail %v", id, d)
			}
			if _, found := d["objective_denied"]; found {
				t.Errorf("%s: objective_denied on a hint that was applied", id)
			}
		case "req-std":
			if d.str("priority_class") != "standard" || d["objective_denied"] != true {
				t.Errorf("%s: detail %v", id, d)
			}
			if v, found := d["priority"]; !found || v != float64(0) {
				t.Errorf("%s: priority %v, want 0 written", id, v)
			}
			if _, found := d["objective"]; found {
				t.Errorf("%s: an objective named although none was applied", id)
			}
		case "req-crit":
			if d.str("priority_class") != "critical" || d.num("priority") != 1 || d.str("objective") != "tier:priority" {
				t.Errorf("%s: detail %v", id, d)
			}
		case "req-unread":
			if d.str("priority_class") != "standard" || d["hint_not_applied"] != true {
				t.Errorf("%s: detail %v", id, d)
			}
		default:
			t.Errorf("unexpected record %q", id)
		}
	}
	if seen != 6 {
		t.Errorf("%d completions read, want 6", seen)
	}
}

// A refusal for an objective the request may not use is filed as an
// authorization refusal at its own stage.
func TestAIDenyObjectiveStage(t *testing.T) {
	if got := aiDenyStage(aiStageObjective); got != "objective" {
		t.Errorf("stage %q", got)
	}
	if got := aiDenyReason(aiStageObjective, "objective_not_allowed"); got != audit.ReasonAuthz {
		t.Errorf("reason %q", got)
	}
	// The stage's number is the data plane's: AI_GW_STAGE_OBJECTIVE follows
	// AI_GW_STAGE_CAPACITY.
	if got := int(aiStageObjective); got != 6 {
		t.Errorf("aiStageObjective = %d, want 6", got)
	}
}

// A refusal names the class its request had, with the same fields and the
// same meanings as a completion, so the records of one request agree; a
// refusal made before the class stage ran names none. An objective refusal
// stays stage objective, reason authz.
func TestEmitAIDenyCarriesTheClass(t *testing.T) {
	_, drain := startTrail(t)
	base := aiDenyRecord{TenantID: "acme", SvcIdent: "10.0.0.1:8080", ModelName: "llama-3",
		HTTPStatus: 403, WorkerID: 1}

	obj := base
	obj.RequestID, obj.Stage, obj.ErrorCode = "req-obj", aiStageObjective, "objective_not_allowed"
	obj.Priority, obj.ClassInfo, obj.Objective = -5, aiClassInfoSet|aiClassNumSheddable|aiClassInfoDenied, "batch"
	rate := base
	rate.RequestID, rate.Stage, rate.HTTPStatus, rate.ErrorCode = "req-rate", aiStageRateLimit, 429, "rate_limit_exceeded"
	rate.Priority, rate.ClassInfo, rate.Objective = 10, aiClassInfoSet|aiClassNumCritical, "realtime"
	early := base
	early.RequestID, early.Stage, early.HTTPStatus, early.ErrorCode = "req-early", aiStageAuth, 401, "invalid_api_key"
	for _, d := range []aiDenyRecord{obj, rate, early} {
		if !emitAIDeny(d) {
			t.Fatal("emit refused")
		}
	}
	seen := 0
	for _, l := range drain() {
		if l.str("event_type") != eventAIDeny {
			continue
		}
		seen++
		d := l.detail()
		switch id := l.str("request_id"); id {
		case "req-obj":
			if d.str("priority_class") != "sheddable" || d.num("priority") != -5 ||
				d.str("objective") != "batch" || d["objective_denied"] != true {
				t.Errorf("%s: detail %v", id, d)
			}
			if d.str("stage") != "objective" || l.outcome().str("reason") != "authz" {
				t.Errorf("%s: stage %q reason %q, want objective / authz", id, d.str("stage"), l.outcome().str("reason"))
			}
		case "req-rate":
			if d.str("priority_class") != "critical" || d.num("priority") != 10 || d.str("objective") != "realtime" {
				t.Errorf("%s: detail %v", id, d)
			}
		case "req-early":
			for _, k := range []string{"priority_class", "priority", "objective", "objective_denied", "hint_not_applied"} {
				if _, found := d[k]; found {
					t.Errorf("%s: %q present on a refusal made before the class stage", id, k)
				}
			}
		default:
			t.Errorf("unexpected record %q", id)
		}
	}
	if seen != 3 {
		t.Fatalf("saw %d deny records, want 3", seen)
	}
}
