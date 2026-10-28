/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package tools

import (
	"reflect"
	"testing"
)

func TestDiffAnyNamesChangedKeysOnly(t *testing.T) {
	cur := map[string]any{"a": 1, "b": "x", "nested": map[string]any{"k": true, "u": 1}, "only_current": 9}
	prop := map[string]any{"a": 2, "b": "x", "nested": map[string]any{"k": false, "u": 1}, "new": "v"}
	got := diffAny(cur, prop)
	want := map[string]any{
		"a":      map[string]any{"from": float64(1), "to": float64(2)},
		"nested": map[string]any{"k": map[string]any{"from": true, "to": false}},
		"new":    map[string]any{"from": nil, "to": "v"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diff = %v, want %v", got, want)
	}
	if d := diffAny(cur, cur); len(d) != 0 {
		t.Errorf("identical states diff to %v", d)
	}
	if d := diffAny(nil, map[string]any{"a": 1}); d["from"] != nil || d["to"] == nil {
		t.Errorf("nil current must diff to a single from/to pair: %v", d)
	}
}

func TestOverlayAndPick(t *testing.T) {
	base := map[string]any{"a": 1, "b": 2}
	out := overlay(base, map[string]any{"b": 3, "c": 4})
	if out["a"] != 1 || out["b"] != 3 || out["c"] != 4 || base["b"] != 2 {
		t.Errorf("overlay = %v (base %v)", out, base)
	}
	if p := pick(base, "a", "zz"); len(p) != 1 || p["a"] != 1 {
		t.Errorf("pick = %v", p)
	}
}

func TestLBRollbackRebuildsTypedArguments(t *testing.T) {
	existing := map[string]any{
		"serviceArguments": map[string]any{"externalIP": "1.2.3.4", "port": float64(80), "protocol": "tcp", "mode": float64(4),
			"host": "models.example", "sse_mode": true, "api_key_auth": "required", "kvExactMode": float64(1), "name": ""},
		"endpoints": []any{map[string]any{"endpointIP": "10.0.0.1", "targetPort": float64(8000), "weight": float64(1), "ep_role": float64(1)}},
	}
	rb := lbRollback(existing)
	if rb.Tool != "lb_create" || rb.Args["host"] != "models.example" || rb.Args["kv_exact_mode"] != float64(1) {
		t.Errorf("rollback args: %v", rb.Args)
	}
	if _, has := rb.Args["name"]; has {
		t.Error("empty name must not be carried")
	}
	eps, _ := rb.Args["endpoints"].([]map[string]any)
	if len(eps) != 1 || eps[0]["ip"] != "10.0.0.1" || eps[0]["ep_role"] != 1 {
		t.Errorf("rollback endpoints: %v", rb.Args["endpoints"])
	}
}
