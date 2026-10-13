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

package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loxilb-io/loxilb/api/models"
	"github.com/loxilb-io/loxilb/api/restapi/operations/ai"
	cmn "github.com/loxilb-io/loxilb/common"
)

// stubNetIdentityHook answers the three binding hooks and nothing else.
type stubNetIdentityHook struct {
	cmn.NetHookInterface
	stored  []cmn.NetIdentityMod
	addErr  error
	delErr  error
	warning string
	added   *cmn.NetIdentityMod
	deleted string
}

func (s *stubNetIdentityHook) NetAINetIdentityGet() ([]cmn.NetIdentityMod, error) {
	return s.stored, nil
}

func (s *stubNetIdentityHook) NetAINetIdentityAdd(pm *cmn.NetIdentityMod) (int, error) {
	s.added = pm
	return 0, s.addErr
}

func (s *stubNetIdentityHook) NetAINetIdentityDel(name string) (int, error) {
	s.deleted = name
	return 0, s.delErr
}

func (s *stubNetIdentityHook) NetAINetIdentityWarning(*cmn.NetIdentityMod) string { return s.warning }

func netIdentityPost(t *testing.T, body string) (int, string) {
	t.Helper()
	var attr models.NetIdentityEntry
	if err := json.Unmarshal([]byte(body), &attr); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/netlox/v1/config/ai/netidentity", nil)
	return renderResponder(t, ConfigPostNetIdentity(ai.PostConfigAiNetidentityParams{HTTPRequest: req, Attr: &attr}, nil))
}

// Every field of the request reaches the net layer, and every stored field
// reaches the reply.
func TestNetIdentityPostAndGetCarryEveryField(t *testing.T) {
	prev := ApiHooks
	defer func() { ApiHooks = prev }()
	stub := &stubNetIdentityHook{}
	ApiHooks = stub

	code, body := netIdentityPost(t, `{"name":"line-1","match":{"cidr":["10.20.0.0/24"]},
		"services":["20.20.20.1:8080"],"tenant":"line-1","allowed_models":["llama-3.1-8b"],
		"enforce":"strict","credential":"none","forward_identity":true,"priority_class":"critical","fail_mode":"closed"}`)
	if code != http.StatusOK || !strings.Contains(body, `"result":"Success"`) || strings.Contains(body, "warning") {
		t.Fatalf("POST answered %d %s", code, body)
	}
	want := cmn.NetIdentityMod{
		Name: "line-1", Match: cmn.NetIdentityMatch{CIDR: []string{"10.20.0.0/24"}},
		Services: []string{"20.20.20.1:8080"}, Tenant: "line-1", AllowedModels: []string{"llama-3.1-8b"},
		Enforce: "strict", Credential: "none", ForwardIdentity: true, PriorityClass: "critical", FailMode: "closed",
	}
	got, _ := json.Marshal(stub.added)
	wantJSON, _ := json.Marshal(want)
	if string(got) != string(wantJSON) {
		t.Fatalf("the net layer was handed %s, want %s", got, wantJSON)
	}

	stub.stored = []cmn.NetIdentityMod{want, {
		Name: "tunnel", Match: cmn.NetIdentityMatch{IngressIf: "vlan100"}, Tenant: "zone-b",
		Enforce: "advisory", Credential: "network", PriorityClass: "standard", FailMode: "neutral",
	}}
	req := httptest.NewRequest(http.MethodGet, "/netlox/v1/config/ai/netidentity", nil)
	code, body = renderResponder(t, ConfigGetNetIdentityAll(ai.GetConfigAiNetidentityAllParams{HTTPRequest: req}, nil))
	if code != http.StatusOK {
		t.Fatalf("GET answered %d %s", code, body)
	}
	var out struct {
		NetIdentityAttr []cmn.NetIdentityMod `json:"netIdentityAttr"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode reply: %v (%s)", err, body)
	}
	gotList, _ := json.Marshal(out.NetIdentityAttr)
	wantList, _ := json.Marshal(stub.stored)
	if string(gotList) != string(wantList) {
		t.Fatalf("GET listed %s, want %s", gotList, wantList)
	}
}

// With nothing configured the list is empty, never null.
func TestNetIdentityGetEmptyIsAnEmptyList(t *testing.T) {
	prev := ApiHooks
	defer func() { ApiHooks = prev }()
	ApiHooks = &stubNetIdentityHook{}
	req := httptest.NewRequest(http.MethodGet, "/netlox/v1/config/ai/netidentity", nil)
	code, body := renderResponder(t, ConfigGetNetIdentityAll(ai.GetConfigAiNetidentityAllParams{HTTPRequest: req}, nil))
	if code != http.StatusOK || !strings.Contains(body, `"netIdentityAttr":[]`) {
		t.Fatalf("empty list answered %d %s", code, body)
	}
}

// A refusal answers with the status its kind has, and its own text.
func TestNetIdentityRefusalStatuses(t *testing.T) {
	prev := ApiHooks
	defer func() { ApiHooks = prev }()
	const post = `{"name":"x","match":{"cidr":["10.20.0.0/24"]},"tenant":"t"}`

	stub := &stubNetIdentityHook{addErr: cmn.NewValidationError("match.cidr", "match.cidr: host bits set")}
	ApiHooks = stub
	if code, body := netIdentityPost(t, post); code != http.StatusBadRequest || !strings.Contains(body, "host bits set") {
		t.Errorf("a validation refusal answered %d %s", code, body)
	}
	stub.addErr = cmn.NewConflictError("binding a already maps cidr 10.20.0.0/24 on every service to tenant ta")
	if code, body := netIdentityPost(t, post); code != http.StatusConflict || !strings.Contains(body, "binding a already maps") {
		t.Errorf("a conflict answered %d %s", code, body)
	}

	stub.delErr = errors.New("no such network-identity binding gone")
	req := httptest.NewRequest(http.MethodDelete, "/netlox/v1/config/ai/netidentity/gone", nil)
	code, body := renderResponder(t, ConfigDeleteNetIdentity(ai.DeleteConfigAiNetidentityNameParams{HTTPRequest: req, Name: "gone"}, nil))
	if code != http.StatusNotFound || stub.deleted != "gone" {
		t.Errorf("deleting a binding that is not there answered %d %s (asked for %q)", code, body, stub.deleted)
	}
	stub.delErr = nil
	code, body = renderResponder(t, ConfigDeleteNetIdentity(ai.DeleteConfigAiNetidentityNameParams{HTTPRequest: req, Name: "line-1"}, nil))
	if code != http.StatusOK || !strings.Contains(body, `"result":"Success"`) {
		t.Errorf("DELETE answered %d %s", code, body)
	}
}

// A warning rides the success reply and refuses nothing.
func TestNetIdentityPostCarriesTheWarning(t *testing.T) {
	prev := ApiHooks
	defer func() { ApiHooks = prev }()
	ApiHooks = &stubNetIdentityHook{warning: `match.ingress_if: no interface "vlan300" on this node; the binding matches nothing until it exists`}
	code, body := netIdentityPost(t, `{"name":"zone","match":{"ingress_if":"vlan300"},"tenant":"zone"}`)
	if code != http.StatusOK || !strings.Contains(body, "Success; warning: match.ingress_if") || !strings.Contains(body, "vlan300") {
		t.Fatalf("POST with a warning answered %d %s", code, body)
	}
}
