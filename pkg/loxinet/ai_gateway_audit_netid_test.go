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

	// A bearer token decided (no key id): the mode stays what it was.
	a = aiNetActor(aiDataActor("acme", "alice", ""), aiAuthSrcCredential, "line-1")
	if a.Auth != audit.AuthNone || a.NetIdentity != "line-1" {
		t.Errorf("token-decided actor = %+v, want the mode untouched", a)
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
