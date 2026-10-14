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
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/loxilb-io/loxilb/api/models"
	"github.com/loxilb-io/loxilb/api/restapi/operations/ai"
	cmn "github.com/loxilb-io/loxilb/common"
	"github.com/loxilb-io/loxilb/pkg/aikey"
)

// stubClassHook answers the objective hooks and the class side of the
// tenant and key hooks, and records what reached it.
type stubClassHook struct {
	cmn.NetHookInterface
	objectives []cmn.AIObjectiveEntry
	err        error

	added   *cmn.AIObjectiveEntry
	deleted string

	limitsSet    int
	tenantPatch  *cmn.AIClassPatch
	tenantEntry  *cmn.TenantRateLimitEntry
	keyPatch     *cmn.AIClassPatch
	keyPatched   int
	limitPatched int
	created      *cmn.ApiKeyEntry
	summary      *cmn.ApiKeySummary
}

func (s *stubClassHook) NetAIObjectiveGet() ([]cmn.AIObjectiveEntry, error) {
	return s.objectives, s.err
}
func (s *stubClassHook) NetAIObjectiveAdd(e *cmn.AIObjectiveEntry) (int, error) {
	s.added = e
	return 0, s.err
}
func (s *stubClassHook) NetAIObjectiveDel(name string) (int, error) {
	s.deleted = name
	return 0, s.err
}
func (s *stubClassHook) NetTenantRateLimitSet(string, int, int, int, []cmn.TenantModelRateLimit) error {
	s.limitsSet++
	return s.err
}
func (s *stubClassHook) NetTenantClassPatch(_ string, p cmn.AIClassPatch) error {
	s.tenantPatch = &p
	return s.err
}
func (s *stubClassHook) NetTenantRateLimitGet(string) (*cmn.TenantRateLimitEntry, error) {
	return s.tenantEntry, s.err
}
func (s *stubClassHook) NetAPIKeyCreate(e cmn.ApiKeyEntry) (string, string, error) {
	s.created = &e
	return "sk-raw", "kid-1", s.err
}
func (s *stubClassHook) NetAPIKeyGet(string) (*cmn.ApiKeySummary, error) { return s.summary, s.err }
func (s *stubClassHook) NetAPIKeyPatch(string, []string, *bool) error {
	s.keyPatched++
	return s.err
}
func (s *stubClassHook) NetAPIKeyRateLimitPatch(string, *int, *int, *int) error {
	s.limitPatched++
	return s.err
}
func (s *stubClassHook) NetAPIKeyClassPatch(_ string, p cmn.AIClassPatch) error {
	s.keyPatch = &p
	return s.err
}

func withClassHook(t *testing.T) *stubClassHook {
	t.Helper()
	prev := ApiHooks
	t.Cleanup(func() { ApiHooks = prev })
	stub := &stubClassHook{}
	ApiHooks = stub
	return stub
}

func objectivePost(t *testing.T, body string) (int, string) {
	t.Helper()
	var attr models.AIObjectiveEntry
	if err := json.Unmarshal([]byte(body), &attr); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/netlox/v1/config/ai/objective", nil)
	return renderResponder(t, ConfigPostAIObjective(ai.PostConfigAiObjectiveParams{HTTPRequest: req, Attr: &attr}, nil))
}

func TestObjectivePostGetDeleteCarryEveryField(t *testing.T) {
	stub := withClassHook(t)

	code, body := objectivePost(t, `{"name":"realtime","services":["20.20.20.1:8080"],"priority":-7}`)
	if code != http.StatusOK || !strings.Contains(body, "Success") {
		t.Fatalf("POST answered %d %s", code, body)
	}
	want := cmn.AIObjectiveEntry{Name: "realtime", Services: []string{"20.20.20.1:8080"}, Priority: -7}
	if stub.added == nil || !reflect.DeepEqual(*stub.added, want) {
		t.Fatalf("the net layer was handed %+v, want %+v", stub.added, want)
	}

	stub.objectives = []cmn.AIObjectiveEntry{{Name: "batch", Priority: -5}, want}
	req := httptest.NewRequest(http.MethodGet, "/netlox/v1/config/ai/objective/all", nil)
	code, body = renderResponder(t, ConfigGetAIObjectiveAll(ai.GetConfigAiObjectiveAllParams{HTTPRequest: req}, nil))
	if code != http.StatusOK {
		t.Fatalf("GET answered %d %s", code, body)
	}
	var got struct {
		ObjectiveAttr []struct {
			Name     string   `json:"name"`
			Services []string `json:"services"`
			Priority *int32   `json:"priority"`
		} `json:"objectiveAttr"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("GET body: %v\n%s", err, body)
	}
	if len(got.ObjectiveAttr) != 2 || got.ObjectiveAttr[0].Name != "batch" ||
		got.ObjectiveAttr[1].Name != "realtime" || got.ObjectiveAttr[1].Services[0] != "20.20.20.1:8080" {
		t.Fatalf("GET returned %s", body)
	}
	// A priority of zero is a value, not an absence.
	stub.objectives = []cmn.AIObjectiveEntry{{Name: "plain"}}
	_, body = renderResponder(t, ConfigGetAIObjectiveAll(ai.GetConfigAiObjectiveAllParams{HTTPRequest: req}, nil))
	if !strings.Contains(body, `"priority":0`) {
		t.Fatalf("a zero priority is not in the reply: %s", body)
	}

	dreq := httptest.NewRequest(http.MethodDelete, "/netlox/v1/config/ai/objective/realtime", nil)
	code, _ = renderResponder(t, ConfigDeleteAIObjective(ai.DeleteConfigAiObjectiveNameParams{HTTPRequest: dreq, Name: "realtime"}, nil))
	if code != http.StatusOK || stub.deleted != "realtime" {
		t.Fatalf("DELETE answered %d, the net layer was asked to delete %q", code, stub.deleted)
	}
}

// A store that is not configured, a store that does not answer, a name the
// store refuses and a name it does not hold are four different answers, on
// every verb.
func TestObjectiveErrorsKeepTheirStatus(t *testing.T) {
	stub := withClassHook(t)
	req := httptest.NewRequest(http.MethodGet, "/netlox/v1/config/ai/objective/all", nil)
	cases := []struct {
		name string
		err  error
		code int
		text string
	}{
		{"no store", cmn.ErrKeyStoreUnconfigured, http.StatusServiceUnavailable, "ai_key_store_unconfigured"},
		{"store down", fmt.Errorf("aikey: %w", cmn.ErrDBUnavailable), http.StatusServiceUnavailable, "ai_key_store_unavailable"},
		{"refused", cmn.NewValidationError("name", "invalid objective name %q", "a b"), http.StatusBadRequest, "invalid objective name"},
		{"absent", aikey.ErrObjectiveNotFound, http.StatusNotFound, "not found"},
		{"other", errors.New("boom"), http.StatusInternalServerError, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub.err = c.err
			answers := map[string]func() (int, string){
				"POST": func() (int, string) { return objectivePost(t, `{"name":"o","priority":1}`) },
				"GET": func() (int, string) {
					return renderResponder(t, ConfigGetAIObjectiveAll(ai.GetConfigAiObjectiveAllParams{HTTPRequest: req}, nil))
				},
				"DELETE": func() (int, string) {
					return renderResponder(t, ConfigDeleteAIObjective(ai.DeleteConfigAiObjectiveNameParams{HTTPRequest: req, Name: "o"}, nil))
				},
			}
			for verb, fn := range answers {
				code, body := fn()
				if code != c.code || !strings.Contains(body, c.text) {
					t.Errorf("%s answered %d %s, want %d with %q", verb, code, body, c.code, c.text)
				}
			}
		})
	}
}

func tenantLimitPost(t *testing.T, body string) (int, string) {
	t.Helper()
	var mod models.TenantRateLimitMod
	if err := json.Unmarshal([]byte(body), &mod); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/netlox/v1/config/ai/tenant/ratelimit", nil)
	return renderResponder(t, ConfigPostAITenantRateLimit(ai.PostConfigAiTenantRatelimitParams{HTTPRequest: req, Body: &mod}, nil))
}

// A tenant's limits and its class are two writes. A body of limits does not
// reach the class, a body of class fields alone does not reach the limits,
// and a body of both reaches both.
func TestTenantPostKeepsLimitsAndClassApart(t *testing.T) {
	t.Run("limits only", func(t *testing.T) {
		stub := withClassHook(t)
		if code, body := tenantLimitPost(t, `{"tenant_id":"t1","rps":5,"tokens_per_min":1000}`); code != http.StatusNoContent {
			t.Fatalf("answered %d %s", code, body)
		}
		if stub.limitsSet != 1 || stub.tenantPatch != nil {
			t.Fatalf("limits written %d times, class patch %+v; want the limits alone", stub.limitsSet, stub.tenantPatch)
		}
	})
	t.Run("no field at all is still a write of zero limits", func(t *testing.T) {
		stub := withClassHook(t)
		tenantLimitPost(t, `{"tenant_id":"t1"}`)
		if stub.limitsSet != 1 || stub.tenantPatch != nil {
			t.Fatalf("limits written %d times, class patch %+v", stub.limitsSet, stub.tenantPatch)
		}
	})
	t.Run("class only", func(t *testing.T) {
		stub := withClassHook(t)
		if code, body := tenantLimitPost(t, `{"tenant_id":"t1","priority_class":"sheddable"}`); code != http.StatusNoContent {
			t.Fatalf("answered %d %s", code, body)
		}
		if stub.limitsSet != 0 {
			t.Fatalf("a body of class fields alone wrote the limits")
		}
		p := stub.tenantPatch
		if p == nil || p.PriorityClass == nil || *p.PriorityClass != "sheddable" ||
			p.DefaultObjective != nil || p.AllowedObjectives != nil {
			t.Fatalf("class patch %+v, want priority_class alone", p)
		}
	})
	t.Run("an empty value and an empty list are present, and clear", func(t *testing.T) {
		stub := withClassHook(t)
		tenantLimitPost(t, `{"tenant_id":"t1","default_objective":"","allowed_objectives":[]}`)
		p := stub.tenantPatch
		if p == nil || p.PriorityClass != nil || p.DefaultObjective == nil || *p.DefaultObjective != "" ||
			p.AllowedObjectives == nil || len(*p.AllowedObjectives) != 0 {
			t.Fatalf("class patch %+v, want default_objective and allowed_objectives cleared", p)
		}
	})
	t.Run("both", func(t *testing.T) {
		stub := withClassHook(t)
		tenantLimitPost(t, `{"tenant_id":"t1","rps":5,"allowed_objectives":["batch","realtime"]}`)
		if stub.limitsSet != 1 || stub.tenantPatch == nil ||
			!reflect.DeepEqual(*stub.tenantPatch.AllowedObjectives, []string{"batch", "realtime"}) {
			t.Fatalf("limits written %d times, class patch %+v", stub.limitsSet, stub.tenantPatch)
		}
	})
	t.Run("a class the store refuses is a 400, a store that is down a 503", func(t *testing.T) {
		stub := withClassHook(t)
		stub.err = cmn.NewValidationError("priority_class", "invalid priority_class %q", "urgent")
		if code, _ := tenantLimitPost(t, `{"tenant_id":"t1","priority_class":"urgent"}`); code != http.StatusBadRequest {
			t.Fatalf("answered %d, want 400", code)
		}
		stub.err = cmn.ErrDBUnavailable
		if code, _ := tenantLimitPost(t, `{"tenant_id":"t1","priority_class":"standard"}`); code != http.StatusServiceUnavailable {
			t.Fatalf("answered %d, want 503", code)
		}
	})
}

func TestTenantGetCarriesClass(t *testing.T) {
	stub := withClassHook(t)
	stub.tenantEntry = &cmn.TenantRateLimitEntry{TenantID: "t1", RPS: 5,
		AIClassPolicy: cmn.AIClassPolicy{PriorityClass: "standard", DefaultObjective: "batch", AllowedObjectives: []string{"batch"}}}
	req := httptest.NewRequest(http.MethodGet, "/netlox/v1/config/ai/tenant/ratelimit/t1", nil)
	code, body := renderResponder(t, ConfigGetAITenantRateLimit(ai.GetConfigAiTenantRatelimitTenantIDParams{HTTPRequest: req, TenantID: "t1"}, nil))
	if code != http.StatusOK || !strings.Contains(body, `"priority_class":"standard"`) ||
		!strings.Contains(body, `"default_objective":"batch"`) || !strings.Contains(body, `"allowed_objectives":["batch"]`) {
		t.Fatalf("GET answered %d %s", code, body)
	}
	// A tenant without a class says nothing about one.
	stub.tenantEntry = &cmn.TenantRateLimitEntry{TenantID: "t1", RPS: 5}
	_, body = renderResponder(t, ConfigGetAITenantRateLimit(ai.GetConfigAiTenantRatelimitTenantIDParams{HTTPRequest: req, TenantID: "t1"}, nil))
	for _, key := range []string{"priority_class", "default_objective", "allowed_objectives"} {
		if strings.Contains(body, key) {
			t.Fatalf("a tenant without a class reports %s: %s", key, body)
		}
	}
}

func TestAPIKeyCreateAndSummaryCarryClass(t *testing.T) {
	stub := withClassHook(t)
	var reqBody models.APIKeyCreateRequest
	if err := json.Unmarshal([]byte(`{"tenant_id":"t1","priority_class":"sheddable","default_objective":"batch","allowed_objectives":["batch"]}`), &reqBody); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/netlox/v1/config/ai/apikey", nil)
	code, body := renderResponder(t, ConfigPostAIApikey(ai.PostConfigAiApikeyParams{HTTPRequest: req, Body: &reqBody}, nil))
	if code != http.StatusCreated {
		t.Fatalf("create answered %d %s", code, body)
	}
	want := cmn.AIClassPolicy{PriorityClass: "sheddable", DefaultObjective: "batch", AllowedObjectives: []string{"batch"}}
	if stub.created == nil || !reflect.DeepEqual(stub.created.AIClassPolicy, want) {
		t.Fatalf("the net layer was handed %+v, want class %+v", stub.created, want)
	}

	stub.summary = &cmn.ApiKeySummary{KeyID: "kid-1", TenantID: "t1", Enabled: true, AIClassPolicy: want}
	greq := httptest.NewRequest(http.MethodGet, "/netlox/v1/config/ai/apikey/kid-1", nil)
	code, body = renderResponder(t, ConfigGetAIApikeyByID(ai.GetConfigAiApikeyKeyIDParams{HTTPRequest: greq, KeyID: "kid-1"}, nil))
	if code != http.StatusOK || !strings.Contains(body, `"priority_class":"sheddable"`) ||
		!strings.Contains(body, `"default_objective":"batch"`) || !strings.Contains(body, `"allowed_objectives":["batch"]`) {
		t.Fatalf("GET answered %d %s", code, body)
	}
}

func apiKeyPatch(body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/netlox/v1/config/ai/apikey/kid-1", strings.NewReader(body))
	ConfigPatchAIApikey(rec, req, "kid-1")
	return rec
}

// The class fields of a key are patched on their own: a field that is
// absent or null is left, an empty one clears, and the key's other fields
// are not written.
func TestAPIKeyPatchClassFields(t *testing.T) {
	stub := withClassHook(t)
	rec := apiKeyPatch(`{"priority_class":"standard","allowed_objectives":[]}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("answered %d %s", rec.Code, rec.Body.String())
	}
	p := stub.keyPatch
	if p == nil || p.PriorityClass == nil || *p.PriorityClass != "standard" || p.DefaultObjective != nil ||
		p.AllowedObjectives == nil || len(*p.AllowedObjectives) != 0 {
		t.Fatalf("class patch %+v", p)
	}
	if stub.keyPatched != 0 || stub.limitPatched != 0 {
		t.Fatalf("a patch of class fields wrote the key's other fields (%d, %d)", stub.keyPatched, stub.limitPatched)
	}

	stub = withClassHook(t)
	if rec := apiKeyPatch(`{"priority_class":null,"default_objective":null,"allowed_objectives":null}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "allowed_objectives") {
		t.Fatalf("a body of nulls answered %d %s; want 400 naming the patchable fields", rec.Code, rec.Body.String())
	}
	if stub.keyPatch != nil {
		t.Fatalf("a body of nulls reached the net layer")
	}

	stub = withClassHook(t)
	apiKeyPatch(`{"rate_limit_rps":3}`)
	if stub.keyPatch != nil || stub.limitPatched != 1 {
		t.Fatalf("a patch of a limit reached the class (%+v) or missed the limit (%d)", stub.keyPatch, stub.limitPatched)
	}

	for name, c := range map[string]struct {
		err  error
		code int
	}{
		"refused":    {cmn.NewValidationError("priority_class", "invalid"), http.StatusBadRequest},
		"absent":     {aikey.ErrKeyNotFound, http.StatusNotFound},
		"store down": {cmn.ErrDBUnavailable, http.StatusServiceUnavailable},
	} {
		stub = withClassHook(t)
		stub.err = c.err
		if rec := apiKeyPatch(`{"default_objective":"batch"}`); rec.Code != c.code {
			t.Errorf("%s: answered %d, want %d", name, rec.Code, c.code)
		}
	}
}

func TestJWTAuthProfileCarriesPriorityClass(t *testing.T) {
	var attr models.JWTAuthProfileEntry
	if err := json.Unmarshal([]byte(`{"name":"kc","issuer":"https://idp.example.com/r","priority_class":"sheddable"}`), &attr); err != nil {
		t.Fatal(err)
	}
	pm := jwtAuthProfileModFromEntry(&attr)
	if pm.PriorityClass != "sheddable" {
		t.Fatalf("priority_class reached the net layer as %q", pm.PriorityClass)
	}
	if back := jwtAuthProfileEntryFromMod(&pm); back.PriorityClass != "sheddable" {
		t.Fatalf("priority_class read back as %q", back.PriorityClass)
	}
	pm.PriorityClass = ""
	raw, _ := json.Marshal(jwtAuthProfileEntryFromMod(&pm))
	if strings.Contains(string(raw), "priority_class") {
		t.Fatalf("a profile without a class reports one: %s", raw)
	}
}

// ai_objective_strict reaches the rule layer with its presence bit, null is
// refused before any rule hook, and the stored value and the class
// configuration's source are read back.
func TestAIObjectiveStrictCreateAndReadBack(t *testing.T) {
	prev := ApiHooks
	defer func() { ApiHooks = prev }()

	for _, c := range []struct {
		name, field string
		want, set   bool
	}{
		{"on", `,"ai_objective_strict":true`, true, true},
		{"off, declared", `,"ai_objective_strict":false`, false, true},
		{"omitted", ``, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			stub := &stubLbAddHook{}
			ApiHooks = stub
			res := ConfigPostLoadbalancer(connectionLimitCreateParams(t, sprintfBody(connectionLimitCreateBody, c.field)), nil)
			if stub.captured == nil {
				t.Fatalf("create handler never reached NetLbRuleAdd: %T", res)
			}
			s := stub.captured.Serv
			if s.AIObjectiveStrict != c.want || s.AIObjectiveStrictPresent != c.set {
				t.Fatalf("ai_objective_strict reached the rule layer as %v present=%v, want %v present=%v",
					s.AIObjectiveStrict, s.AIObjectiveStrictPresent, c.want, c.set)
			}
		})
	}
	t.Run("null", func(t *testing.T) {
		stub := &stubLbAddHook{}
		ApiHooks = stub
		ConfigPostLoadbalancer(connectionLimitCreateParams(t, sprintfBody(connectionLimitCreateBody, `,"ai_objective_strict":null`)), nil)
		if stub.captured != nil {
			t.Fatalf("a null ai_objective_strict reached NetLbRuleAdd")
		}
	})

	out := serializeLBRule(cmn.LbRuleMod{Serv: cmn.LbServiceArg{
		ServIP: "10.10.10.254", ServPort: 2050, Proto: "tcp", AIObjectiveStrict: true,
		FcEffective: &cmn.FcEffectiveArg{Mode: "off", ConfigSource: cmn.AIClassSourceLKG},
	}})
	if sa := out.ServiceArguments; !sa.AiObjectiveStrict || sa.FcEffective == nil || sa.FcEffective.ConfigSource != "lkg" {
		t.Fatalf("read back as strict=%v fc_effective=%+v", sa.AiObjectiveStrict, sa.FcEffective)
	}
}
