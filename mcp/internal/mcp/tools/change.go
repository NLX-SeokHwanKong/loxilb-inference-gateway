/*
 * Copyright (c) 2026 NetLOX Inc
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

package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// The change gate: every mutation whose blast radius is wider than one
// object previews before it applies. Called without confirm_token the tool
// reads the current state, computes the proposed state and answers with
// {action: preview, current, proposed, diff, rollback, impact,
// confirm_token}; nothing changes. Called again with the token (bound to the
// exact arguments) it applies and returns the object re-read from the API.
// gateDestructive is the delete-only shape of the same step and shares the
// token handling.

// rollbackSpec names the call that restores the state the preview captured.
// Tool is empty when no single call restores it; Note then says why.
type rollbackSpec struct {
	Tool string         `json:"tool,omitempty" jsonschema:"tool that restores the state captured in current"`
	Args map[string]any `json:"args,omitempty" jsonschema:"arguments for the rollback call (arbitrary JSON); confirm it like any other change"`
	Note string         `json:"note,omitempty" jsonschema:"why no rollback call exists, when tool is empty"`
}

// changePlan is what a tool computes for the preview: the state before, the
// state after, how to undo, and the manifest impact class of the operation.
type changePlan struct {
	Current  any
	Proposed any
	Rollback *rollbackSpec
	Impact   string
}

// gateChange implements the change gate for one call. args must be the
// input struct with its ConfirmToken field cleared (the token binding).
// plan is invoked only when a preview is due; it performs the reads. It
// returns (previewOut, true, nil) when the call must stop at the preview and
// (zero, false, nil) when the caller may apply the change.
func (d *Deps) gateChange(tool, target, confirmToken string, args any, plan func() (changePlan, error)) (mutOut, bool, error) {
	if d.Confirm == nil {
		return mutOut{}, false, nil // --no-confirm
	}
	if confirmToken == "" && d.autopilot(tool, target, args) {
		return mutOut{}, false, nil
	}
	binding, err := guard.BindArgs(tool, target, args)
	if err != nil {
		return mutOut{}, false, err
	}
	if confirmToken != "" {
		if err := d.Confirm.Redeem(confirmToken, binding); err != nil {
			d.auditMut(tool, target, args, false, "confirm: "+err.Error())
			return mutOut{}, false, err
		}
		return mutOut{}, false, nil
	}
	p, err := plan()
	if err != nil {
		return mutOut{}, false, err
	}
	tok, err := d.Confirm.Issue(binding)
	if err != nil {
		return mutOut{}, false, err
	}
	return mutOut{
		Target:            target,
		Action:            "preview",
		Current:           sanitizeAny(p.Current, 0),
		Proposed:          sanitizeAny(p.Proposed, 0),
		Diff:              diffAny(p.Current, p.Proposed),
		Rollback:          p.Rollback,
		Impact:            p.Impact,
		ConfirmToken:      tok,
		ConfirmExpiresSec: int(guard.DefaultConfirmTTL.Seconds()),
		Warning: "NO CHANGE MADE. To execute, call " + tool + " again with identical " +
			"arguments plus this confirm_token before it expires. The token is single-use " +
			"and bound to these exact arguments.",
	}, true, nil
}

// autopilot reports whether the tool may skip the preview and writes the
// bypass audit event. Tools in guard.NeverAutopilot are never eligible.
func (d *Deps) autopilot(tool, target string, args any) bool {
	if d.Autopilot == nil || !d.Autopilot(tool) {
		return false
	}
	var argMap map[string]any
	if raw, err := json.Marshal(args); err == nil {
		_ = json.Unmarshal(raw, &argMap)
	}
	d.Audit.Log(guard.Event{
		Kind: guard.EventAutopilot, Tool: tool, Target: target,
		Args: guard.Redact(argMap), OK: true,
	})
	return true
}

// executed builds the result of an applied change: the API's answer plus the
// object re-read afterwards.
func executed(target string, result, readBack any) mutOut {
	return mutOut{Target: target, Action: "executed", Result: sanitizeAny(result, 0), ReadBack: sanitizeAny(readBack, 0)}
}

// diffAny compares two JSON-shaped values. For two objects it lists the keys
// whose value differs as {key: {from, to}}, recursing into nested objects
// (keys only in proposed count as changes, keys only in current are
// untouched); anything else is a single {from, to} pair when the values
// differ, or an empty object.
func diffAny(current, proposed any) map[string]any {
	cur := jsonNormalize(current)
	prop := jsonNormalize(proposed)
	out := map[string]any{}
	cm, cok := cur.(map[string]any)
	pm, pok := prop.(map[string]any)
	if !cok || !pok {
		if !reflect.DeepEqual(cur, prop) {
			out["from"] = cur
			out["to"] = prop
		}
		return out
	}
	keys := make([]string, 0, len(pm))
	for k := range pm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if reflect.DeepEqual(cm[k], pm[k]) {
			continue
		}
		cn, cnOK := cm[k].(map[string]any)
		pn, pnOK := pm[k].(map[string]any)
		if cnOK && pnOK {
			if nested := diffAny(cn, pn); len(nested) > 0 {
				out[k] = nested
			}
			continue
		}
		out[k] = map[string]any{"from": cm[k], "to": pm[k]}
	}
	return out
}

// jsonNormalize round-trips a value through JSON so that numbers, nil
// slices and typed structs compare the way the agent sees them.
func jsonNormalize(v any) any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw)
	}
	return out
}

// overlay returns base with every key of patch set (a shallow merge used to
// derive the proposed state of a "only supplied fields" tool).
func overlay(base map[string]any, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		out[k] = v
	}
	return out
}

// pick copies the named keys of src that are present (a projection for
// current state and rollback arguments).
func pick(src map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, k := range keys {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
	return out
}

// mapValue returns v as an object, or an empty one.
func mapValue(v any) map[string]any {
	m, _ := jsonNormalize(v).(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
