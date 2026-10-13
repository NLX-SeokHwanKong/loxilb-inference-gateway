/*
 * Copyright (c) 2026 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
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
	"testing"

	cmn "github.com/loxilb-io/loxilb/common"
)

// The default reservation's two fields reach the rule layer the way the
// capacity gate's do: a declaration is copied with its presence bit, an
// omitted field is left for the rule layer to keep, and a value the gateway
// could not honour is refused before any rule hook runs.

func TestAIDefaultReservationCreateCopiesDeclaration(t *testing.T) {
	prev := ApiHooks
	defer func() { ApiHooks = prev }()

	cases := []struct {
		name        string
		field       string
		wantMode    string
		wantModeSet bool
		wantTokens  uint32
		wantTokSet  bool
	}{
		{"declared", `,"ai_default_reservation":"on","ai_default_max_tokens":2000`, "on", true, 2000, true},
		{"omitted", ``, "", false, 0, false},
		{"the switch alone", `,"ai_default_reservation":"off"`, "off", true, 0, false},
		{"the value alone", `,"ai_default_max_tokens":300`, "", false, 300, true},
		{"inherit and an explicit zero are resets, and are seen as declared",
			`,"ai_default_reservation":"inherit","ai_default_max_tokens":0`, "inherit", true, 0, true},
		{"the ceiling", `,"ai_default_max_tokens":100000000`, "", false, 100000000, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := &stubLbAddHook{}
			ApiHooks = stub
			raw := sprintfBody(connectionLimitCreateBody, c.field)
			res := ConfigPostLoadbalancer(connectionLimitCreateParams(t, raw), nil)
			if stub.captured == nil {
				t.Fatalf("create handler never reached NetLbRuleAdd: %T", res)
			}
			s := stub.captured.Serv
			if s.AIDefaultReservation != c.wantMode || s.AIDefaultReservationPresent != c.wantModeSet {
				t.Fatalf("ai_default_reservation reached the rule layer as %q present=%v, want %q present=%v",
					s.AIDefaultReservation, s.AIDefaultReservationPresent, c.wantMode, c.wantModeSet)
			}
			if s.AIDefaultMaxTokens != c.wantTokens || s.AIDefaultMaxTokensPresent != c.wantTokSet {
				t.Fatalf("ai_default_max_tokens reached the rule layer as %d present=%v, want %d present=%v",
					s.AIDefaultMaxTokens, s.AIDefaultMaxTokensPresent, c.wantTokens, c.wantTokSet)
			}
		})
	}
}

func TestAIDefaultReservationCreateRefusedBeforeRuleHook(t *testing.T) {
	prev := ApiHooks
	defer func() { ApiHooks = prev }()

	for name, field := range map[string]string{
		"a null switch":             `,"ai_default_reservation":null`,
		"a null value":              `,"ai_default_max_tokens":null`,
		"a word that is no switch":  `,"ai_default_reservation":"yes"`,
		"a value above the ceiling": `,"ai_default_max_tokens":100000001`,
		"a negative value":          `,"ai_default_max_tokens":-1`,
	} {
		t.Run(name, func(t *testing.T) {
			stub := &stubLbAddHook{}
			ApiHooks = stub
			raw := sprintfBody(connectionLimitCreateBody, field)
			ConfigPostLoadbalancer(connectionLimitCreateParams(t, raw), nil)
			if stub.captured != nil {
				t.Fatalf("a refused declaration reached NetLbRuleAdd: %+v", stub.captured.Serv)
			}
		})
	}
}

func TestAIDefaultReservationReadBack(t *testing.T) {
	lb := cmn.LbRuleMod{Serv: cmn.LbServiceArg{
		ServIP: "10.10.10.254", ServPort: 2050, Proto: "tcp",
		AIDefaultReservation: "on", AIDefaultMaxTokens: 2000,
		FcEffective: &cmn.FcEffectiveArg{
			Mode: "off", DefaultReservation: "on", DefaultMaxTokens: 2000, ReservationClamped: 7,
			Source: cmn.FcEffectiveSource{DefaultReservation: "rule", DefaultMaxTokens: "rule"},
		},
	}}
	out := serializeLBRule(lb)
	sa := out.ServiceArguments
	if sa.AiDefaultReservation != "on" || sa.AiDefaultMaxTokens != 2000 {
		t.Fatalf("declared values read back as %q / %d", sa.AiDefaultReservation, sa.AiDefaultMaxTokens)
	}
	eff := sa.FcEffective
	if eff == nil || eff.DefaultReservation != "on" || eff.DefaultMaxTokens != 2000 || eff.ReservationClamped != 7 {
		t.Fatalf("fc_effective read back as %+v", eff)
	}
	if eff.Source == nil || eff.Source.DefaultReservation != "rule" || eff.Source.DefaultMaxTokens != "rule" {
		t.Fatalf("fc_effective.source read back as %+v", eff.Source)
	}
}

type resvWarningHook struct {
	warningLbAddHook
	resvMsg   string
	resvAsked *cmn.LbRuleMod
}

func (s *resvWarningHook) NetAIDefaultReservationWarning(m *cmn.LbRuleMod) string {
	s.resvAsked = m
	return s.resvMsg
}

// A rule whose default reservation differs from another rule's on the same
// VIP and port is still applied: the reply carries the warning in its result
// text, after the tenant-table warning when both apply, and a refused rule
// is not asked about.
func TestAIDefaultReservationWarningRidesTheSuccessReply(t *testing.T) {
	prev := ApiHooks
	defer func() { ApiHooks = prev }()
	raw := sprintfBody(connectionLimitCreateBody, `,"ai_default_reservation":"on","ai_default_max_tokens":256`)

	result := func(res interface{}) string {
		rr, ok := res.(*ResultResponse)
		if !ok {
			t.Fatalf("answered %#v, want a result reply", res)
		}
		return rr.Result
	}

	only := &resvWarningHook{resvMsg: "ai_default_reservation: 512 tokens is in effect"}
	ApiHooks = only
	if got := result(ConfigPostLoadbalancer(connectionLimitCreateParams(t, raw), nil)); got != "Success; warning: ai_default_reservation: 512 tokens is in effect" {
		t.Errorf("with the listener warning: result %q", got)
	}
	if only.captured == nil || only.resvAsked == nil || only.resvAsked.Serv.AIDefaultMaxTokens != 256 {
		t.Errorf("the rule was not applied and then asked about: applied=%v asked=%v", only.captured != nil, only.resvAsked)
	}

	both := &resvWarningHook{resvMsg: "listener differs"}
	both.msg = "300 tenants are known"
	ApiHooks = both
	if got := result(ConfigPostLoadbalancer(connectionLimitCreateParams(t, raw), nil)); got != "Success; warning: 300 tenants are known; warning: listener differs" {
		t.Errorf("with both warnings: result %q", got)
	}

	ApiHooks = &resvWarningHook{}
	if got := result(ConfigPostLoadbalancer(connectionLimitCreateParams(t, raw), nil)); got != "Success" {
		t.Errorf("nothing to warn about: result %q", got)
	}
}
