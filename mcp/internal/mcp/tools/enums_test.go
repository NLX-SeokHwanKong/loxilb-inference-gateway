/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSwaggerTablesGenerated(t *testing.T) {
	props := swaggerProps["serviceArguments"]
	if len(props) < 100 {
		t.Fatalf("swaggerProps[serviceArguments] has %d entries; the generated table looks stale", len(props))
	}
	has := func(list []string, want string) bool {
		for _, v := range list {
			if v == want {
				return true
			}
		}
		return false
	}
	if !has(props, "fc_mode") || !has(props, "externalIP") {
		t.Error("serviceArguments table misses fc_mode or externalIP")
	}
	if !has(swaggerEnum["serviceArguments.api_key_auth"], "jwt") {
		t.Errorf("api_key_auth enum = %v, want it to include jwt", swaggerEnum["serviceArguments.api_key_auth"])
	}
	if swaggerEnumDoc["serviceArguments.api_key_auth"] == "" {
		t.Error("api_key_auth has no generated description")
	}
	if _, ok := swaggerExcludedField["ApiKeyCreateRequest.api_key"]; !ok {
		t.Error("ApiKeyCreateRequest.api_key must be an excluded field")
	}
	for _, m := range []string{"endpoints", "ApiKeyCreateRequest", "TenantRateLimitMod"} {
		if len(swaggerProps[m]) == 0 {
			t.Errorf("swaggerProps[%s] is empty", m)
		}
	}
}

func TestValidateServiceExtra(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		want  string // substring of the error, "" for accepted
	}{
		{"empty", nil, ""},
		{"known", map[string]any{"fc_mode": "observe", "security": 1}, ""},
		{"unknown", map[string]any{"fc_foo": 1}, "not a serviceArguments property"},
		{"read-back fc", map[string]any{"fc_effective": "enforce"}, "read-back only"},
		{"read-back tls", map[string]any{"backend_tls_effective": true}, "read-back only"},
		{"read-back half-close", map[string]any{"half_close_effective": "hold"}, "read-back only"},
		{"first refusal is sorted", map[string]any{"zzz": 1, "aaa": 1}, `"aaa"`},
	}
	for _, tc := range cases {
		err := validateServiceExtra(tc.extra)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

// An invalid service_extra key is refused before the request is built: the
// mock backend must see no POST at all.
func TestLbCreateValidatesServiceExtraBeforeRequest(t *testing.T) {
	posts := 0
	var lastBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/netlox/v1/config/loadbalancer", func(w http.ResponseWriter, r *http.Request) {
		posts++
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &lastBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "Success"})
	})
	d := newDeps(t, mux)
	base := lbCreateIn{
		ExternalIP: "10.0.0.1", Port: 80, Protocol: "tcp",
		Endpoints: []lbEndpointIn{{IP: "10.0.0.2", Port: 8080}},
	}

	bad := base
	bad.ServiceExtra = map[string]any{"fc_effective": "enforce"}
	if _, _, err := d.lbCreate()(context.Background(), nil, bad); err == nil {
		t.Fatal("read-back key accepted")
	}
	bad.ServiceExtra = map[string]any{"not_a_field": 1}
	if _, _, err := d.lbCreate()(context.Background(), nil, bad); err == nil {
		t.Fatal("unknown key accepted")
	}
	if posts != 0 {
		t.Fatalf("backend saw %d POSTs for refused keys", posts)
	}

	good := base
	good.ServiceExtra = map[string]any{"fc_mode": "observe"}
	if _, out, err := d.lbCreate()(context.Background(), nil, good); err != nil || out.Action != "executed" {
		t.Fatalf("valid key refused: err=%v out=%+v", err, out)
	}
	if posts != 1 {
		t.Fatalf("backend saw %d POSTs, want 1", posts)
	}
	svc, _ := lastBody["serviceArguments"].(map[string]any)
	if svc["fc_mode"] != "observe" {
		t.Errorf("fc_mode not forwarded: %v", svc)
	}
}
