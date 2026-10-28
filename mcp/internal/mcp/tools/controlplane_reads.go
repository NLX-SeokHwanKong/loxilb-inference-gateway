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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// Control-plane reads: single-rule reads with their lifecycle and traffic
// status, the KV-exact composition status, the AI control-plane lists
// (objectives, network identities, JWT profiles, per-user rate limits and
// defaults, model profiles), the gateway's capability verdicts, the
// maintenance episode, the half-close hold settings, the audit trail and
// the instance snapshot. Each tool performs one GET (two for lb_get with
// include) and projects or sanitizes the payload; none sends a mutation.

// ---- lb_get ----

type lbGetIn struct {
	Target     string   `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	ExternalIP string   `json:"external_ip,omitempty" jsonschema:"rule VIP; with port and protocol the composite key of the rule (omit when id is given)"`
	Port       int      `json:"port,omitempty" jsonschema:"rule port (composite key)"`
	Protocol   string   `json:"protocol,omitempty" jsonschema:"rule protocol tcp|udp|sctp (composite key)"`
	ID         string   `json:"id,omitempty" jsonschema:"opaque rule id assigned by the gateway, as lb_list reports it; alternative to the composite key"`
	Include    []string `json:"include,omitempty" jsonschema:"extra reads to attach: status (lifecycle: adminStateUp, operatingStatus, lastUpdated) and/or stats (activeConnections, bytesIn, bytesOut, totalConnections)"`
}

type lbGetOut struct {
	Target string         `json:"target"`
	Rule   lbRule         `json:"rule"`
	Status map[string]any `json:"status,omitempty"`
	Stats  map[string]any `json:"stats,omitempty"`
	Note   string         `json:"note,omitempty"`
}

// The composite-key sub-resources are spelled as whole literals so the
// contract scanner matches each to its own operation.
func lbStatusPath(ip string, port int, proto string) string {
	return fmt.Sprintf("/config/loadbalancer/externalipaddress/%s/port/%d/protocol/%s/status",
		url.PathEscape(ip), port, url.PathEscape(strings.ToLower(proto)))
}

func lbStatsPath(ip string, port int, proto string) string {
	return fmt.Sprintf("/config/loadbalancer/externalipaddress/%s/port/%d/protocol/%s/stats",
		url.PathEscape(ip), port, url.PathEscape(strings.ToLower(proto)))
}

func lbKvexactStatusPath(ip string, port int, proto string) string {
	return fmt.Sprintf("/config/loadbalancer/externalipaddress/%s/port/%d/protocol/%s/kvexactstatus",
		url.PathEscape(ip), port, url.PathEscape(strings.ToLower(proto)))
}

func (d *Deps) lbGet() sdk.ToolHandlerFor[lbGetIn, lbGetOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in lbGetIn) (*sdk.CallToolResult, lbGetOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, lbGetOut{}, err
		}
		wantStatus, wantStats := false, false
		for _, inc := range in.Include {
			switch strings.ToLower(inc) {
			case "status":
				wantStatus = true
			case "stats":
				wantStats = true
			default:
				return nil, lbGetOut{}, fmt.Errorf("include: %q is not one of status, stats", inc)
			}
		}
		var entry map[string]any
		switch {
		case in.ID != "":
			if in.ExternalIP != "" || in.Port != 0 || in.Protocol != "" {
				return nil, lbGetOut{}, fmt.Errorf("give either id or the composite key external_ip + port + protocol, not both")
			}
			if err := validatePathSegment(in.ID); err != nil {
				return nil, lbGetOut{}, fmt.Errorf("id: %w", err)
			}
			if err := c.Get(ctx, "/config/loadbalancer/id/"+url.PathEscape(in.ID), &entry); err != nil {
				d.audit("lb_get", c.Name(), false, err.Error())
				return nil, lbGetOut{}, err
			}
		default:
			if err := validIP(in.ExternalIP, "external_ip"); err != nil {
				return nil, lbGetOut{}, err
			}
			if in.Port <= 0 || in.Port > 65535 {
				return nil, lbGetOut{}, fmt.Errorf("port: %d is out of range", in.Port)
			}
			if err := validL4Proto(in.Protocol); err != nil {
				return nil, lbGetOut{}, err
			}
			rule, found, err := c.LBRuleByKey(ctx, in.ExternalIP, in.Port, in.Protocol)
			if err != nil {
				d.audit("lb_get", c.Name(), false, err.Error())
				return nil, lbGetOut{}, err
			}
			if !found {
				d.audit("lb_get", c.Name(), false, "no rule at key")
				return nil, lbGetOut{}, fmt.Errorf("no rule at %s:%d/%s", in.ExternalIP, in.Port, strings.ToLower(in.Protocol))
			}
			entry = rule
		}
		rule, ok := projectLBRule(entry)
		if !ok {
			d.audit("lb_get", c.Name(), false, "payload without serviceArguments")
			return nil, lbGetOut{}, fmt.Errorf("the gateway answered without serviceArguments")
		}
		out := lbGetOut{Target: c.Name(), Rule: rule}
		if wantStatus || wantStats {
			port := intValue(rule.Port)
			if wantStatus {
				var st map[string]any
				if err := c.Get(ctx, lbStatusPath(rule.ExternalIP, port, rule.Protocol), &st); err != nil {
					d.audit("lb_get", c.Name(), false, err.Error())
					return nil, lbGetOut{}, err
				}
				out.Status = mapValue(sanitizeAny(st, 0))
			}
			if wantStats {
				var st map[string]any
				if err := c.Get(ctx, lbStatsPath(rule.ExternalIP, port, rule.Protocol), &st); err != nil {
					d.audit("lb_get", c.Name(), false, err.Error())
					return nil, lbGetOut{}, err
				}
				out.Stats = mapValue(sanitizeAny(st, 0))
			}
			out.Note = "status and stats are read by the VIP:port/protocol key; on a VIP:port shared by several host/path/model rules they describe the first rule at that key"
		}
		d.audit("lb_get", c.Name(), true, "")
		return nil, out, nil
	}
}

// ---- lb_kvexact_status ----

type lbKvexactStatusIn struct {
	Target     string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	ExternalIP string `json:"external_ip" jsonschema:"rule VIP (composite key)"`
	Port       int    `json:"port" jsonschema:"rule port (composite key)"`
	Protocol   string `json:"protocol" jsonschema:"rule protocol tcp|udp|sctp (composite key)"`
	ModelName  string `json:"model_name,omitempty" jsonschema:"restrict to the KV-exact rule serving this model when the key carries several"`
}

func (d *Deps) lbKvexactStatus() sdk.ToolHandlerFor[lbKvexactStatusIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in lbKvexactStatusIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		if err := validIP(in.ExternalIP, "external_ip"); err != nil {
			return nil, nil, err
		}
		if in.Port <= 0 || in.Port > 65535 {
			return nil, nil, fmt.Errorf("port: %d is out of range", in.Port)
		}
		if err := validL4Proto(in.Protocol); err != nil {
			return nil, nil, err
		}
		q := url.Values{}
		if in.ModelName != "" {
			q.Set("model_name", in.ModelName)
		}
		var raw struct {
			Attr []any `json:"kvExactStatusAttr"`
		}
		if err := c.GetQ(ctx, lbKvexactStatusPath(in.ExternalIP, in.Port, in.Protocol), q, &raw); err != nil {
			d.audit("lb_kvexact_status", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("lb_kvexact_status", c.Name(), true, "")
		return nil, map[string]any{
			"target":  c.Name(),
			"count":   len(raw.Attr),
			"entries": sanitizeAny(raw.Attr, 0),
			"note":    "resolved status from the gateway (binding identity and generation, hash contract, attestation ladder desired/enforced with reason codes); this is a read model and never a configuration body",
		}, nil
	}
}

// ---- AI control-plane lists ----

func (d *Deps) aiObjectiveList() sdk.ToolHandlerFor[targetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in targetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		var raw map[string]any
		if err := c.Get(ctx, "/config/ai/objective", &raw); err != nil {
			d.audit("ai_objective_list", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_objective_list", c.Name(), true, "")
		list, _ := raw["objectiveAttr"].([]any)
		return nil, map[string]any{
			"target":           c.Name(),
			"count":            len(list),
			"objectives":       sanitizeAny(list, 0),
			"source":           clean(stringValue(raw["source"])),
			"source_timestamp": clean(stringValue(raw["source_timestamp"])),
			"store_reachable":  raw["store_reachable"],
			"store_last_read":  clean(stringValue(raw["store_last_read"])),
		}, nil
	}
}

func (d *Deps) aiNetidentityList() sdk.ToolHandlerFor[targetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in targetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		var raw struct {
			Attr []any `json:"netIdentityAttr"`
		}
		if err := c.Get(ctx, "/config/ai/netidentity", &raw); err != nil {
			d.audit("ai_netidentity_list", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_netidentity_list", c.Name(), true, "")
		return nil, map[string]any{"target": c.Name(), "count": len(raw.Attr), "bindings": sanitizeAny(raw.Attr, 0)}, nil
	}
}

func (d *Deps) aiJwtprofileList() sdk.ToolHandlerFor[targetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in targetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		var raw struct {
			Attr []any `json:"jwtAuthProfileAttr"`
		}
		if err := c.Get(ctx, "/config/ai/jwtauthprofile", &raw); err != nil {
			d.audit("ai_jwtprofile_list", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_jwtprofile_list", c.Name(), true, "")
		return nil, map[string]any{
			"target":   c.Name(),
			"count":    len(raw.Attr),
			"profiles": stripKeyMaterial(sanitizeAny(raw.Attr, 0)),
			"note":     "desired configuration only (issuer, JWKS URL, claims); not issuer reachability or keyset health",
		}, nil
	}
}

type userRatelimitListIn struct {
	Target   string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	TenantID string `json:"tenant_id" jsonschema:"tenant whose explicit per-user rows to list"`
}

func (d *Deps) aiUserRatelimitList() sdk.ToolHandlerFor[userRatelimitListIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in userRatelimitListIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		if err := validatePathSegment(in.TenantID); err != nil {
			return nil, nil, fmt.Errorf("tenant_id: %w", err)
		}
		var raw []any
		if err := c.Get(ctx, "/config/ai/user/ratelimit/"+url.PathEscape(in.TenantID), &raw); err != nil {
			d.audit("ai_user_ratelimit_list", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_user_ratelimit_list", c.Name(), true, "")
		return nil, map[string]any{
			"target":    c.Name(),
			"tenant_id": clean(in.TenantID),
			"count":     len(raw),
			"users":     sanitizeAny(raw, 0),
			"note":      "explicit rows only, without model limits (ai_user_ratelimit_get carries those); users without a row follow the defaults",
		}, nil
	}
}

type userRatelimitGetIn struct {
	Target   string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	TenantID string `json:"tenant_id" jsonschema:"tenant of the user"`
	UserID   string `json:"user_id" jsonschema:"user whose explicit rate-limit row to read"`
}

func (d *Deps) aiUserRatelimitGet() sdk.ToolHandlerFor[userRatelimitGetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in userRatelimitGetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		if err := validatePathSegment(in.TenantID); err != nil {
			return nil, nil, fmt.Errorf("tenant_id: %w", err)
		}
		if err := validatePathSegment(in.UserID); err != nil {
			return nil, nil, fmt.Errorf("user_id: %w", err)
		}
		var raw any
		if err := c.Get(ctx, fmt.Sprintf("/config/ai/user/ratelimit/%s/%s", url.PathEscape(in.TenantID), url.PathEscape(in.UserID)), &raw); err != nil {
			d.audit("ai_user_ratelimit_get", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_user_ratelimit_get", c.Name(), true, "")
		return nil, map[string]any{"target": c.Name(), "user_ratelimit": sanitizeAny(raw, 0)}, nil
	}
}

type ratelimitDefaultsGetIn struct {
	Target    string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Scope     string `json:"scope" jsonschema:"global or rule"`
	RuleIdent string `json:"rule_ident,omitempty" jsonschema:"for scope rule: the service the row belongs to; ignored for scope global"`
}

func (d *Deps) aiRatelimitDefaultsGet() sdk.ToolHandlerFor[ratelimitDefaultsGetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ratelimitDefaultsGetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		scope := strings.ToLower(in.Scope)
		if scope != "global" && scope != "rule" {
			return nil, nil, fmt.Errorf("scope: %q is not global or rule", in.Scope)
		}
		q := url.Values{}
		if scope == "rule" {
			if in.RuleIdent == "" {
				return nil, nil, fmt.Errorf("rule_ident is required for scope rule")
			}
			q.Set("rule_ident", in.RuleIdent)
		}
		var raw any
		if err := c.GetQ(ctx, "/config/ai/ratelimit/defaults/"+scope, q, &raw); err != nil {
			d.audit("ai_ratelimit_defaults_get", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_ratelimit_defaults_get", c.Name(), true, "")
		return nil, map[string]any{"target": c.Name(), "defaults": sanitizeAny(raw, 0)}, nil
	}
}

func (d *Deps) aiModelProfileList() sdk.ToolHandlerFor[targetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in targetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		var raw map[string]any
		if err := c.Get(ctx, "/config/ai/model-profiles", &raw); err != nil {
			d.audit("ai_model_profile_list", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_model_profile_list", c.Name(), true, "")
		list, _ := raw["profiles"].([]any)
		return nil, map[string]any{
			"target":              c.Name(),
			"registry_generation": raw["registryGeneration"],
			"set_digest":          clean(stringValue(raw["setDigest"])),
			"count":               len(list),
			"profiles":            sanitizeAny(list, 0),
			"note":                "the published registry generation only; a profile listed here has passed digest, tokenizer and template verification, and a cache of that publication, not an authority on what a request may use",
		}, nil
	}
}

type modelProfileGetIn struct {
	Target    string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	ProfileID string `json:"profile_id" jsonschema:"profile id as ai_model_profile_list reports it"`
}

func (d *Deps) aiModelProfileGet() sdk.ToolHandlerFor[modelProfileGetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in modelProfileGetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		if err := validatePathSegment(in.ProfileID); err != nil {
			return nil, nil, fmt.Errorf("profile_id: %w", err)
		}
		var raw any
		if err := c.Get(ctx, "/config/ai/model-profiles/"+url.PathEscape(in.ProfileID), &raw); err != nil {
			d.audit("ai_model_profile_get", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_model_profile_get", c.Name(), true, "")
		return nil, map[string]any{"target": c.Name(), "profile": sanitizeAny(raw, 0)}, nil
	}
}

// ---- status_capabilities ----

type capabilitiesIn struct {
	Target    string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	ModelName string `json:"model_name,omitempty" jsonschema:"evaluate model-scoped capabilities for this model"`
}

func (d *Deps) statusCapabilities() sdk.ToolHandlerFor[capabilitiesIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in capabilitiesIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		q := url.Values{}
		if in.ModelName != "" {
			q.Set("model_name", in.ModelName)
		}
		var raw struct {
			Caps []map[string]any `json:"capabilities"`
		}
		if err := c.GetQ(ctx, "/status/capabilities", q, &raw); err != nil {
			d.audit("status_capabilities", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("status_capabilities", c.Name(), true, "")
		var notReady []string
		for _, cp := range raw.Caps {
			if !boolValue(cp["ready"]) {
				notReady = append(notReady, clean(stringValue(cp["name"])))
			}
		}
		return nil, map[string]any{
			"target":       c.Name(),
			"count":        len(raw.Caps),
			"not_ready":    notReady,
			"capabilities": sanitizeAny(raw.Caps, 0),
			"note":         "a capability reported not ready refuses every attempt to use it with 412 and the reason carried here",
		}, nil
	}
}

// ---- audit_sink_get ----

type auditSinkGetIn struct {
	Target string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Name   string `json:"name,omitempty" jsonschema:"a secondary sink's name (GET /audit/sinks/{name}); omit for the primary remote sink (GET /audit/sink)"`
}

func (d *Deps) auditSinkGet() sdk.ToolHandlerFor[auditSinkGetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in auditSinkGetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		var raw any
		if in.Name == "" {
			if err := c.Get(ctx, "/audit/sink", &raw); err != nil {
				d.audit("audit_sink_get", c.Name(), false, err.Error())
				return nil, nil, err
			}
		} else {
			if err := validatePathSegment(in.Name); err != nil {
				return nil, nil, fmt.Errorf("name: %w", err)
			}
			if err := c.Get(ctx, "/audit/sinks/"+url.PathEscape(in.Name), &raw); err != nil {
				d.audit("audit_sink_get", c.Name(), false, err.Error())
				return nil, nil, err
			}
		}
		d.audit("audit_sink_get", c.Name(), true, "")
		return nil, map[string]any{
			"target": c.Name(),
			"named":  in.Name != "",
			"sink":   sanitizeAny(raw, 0),
			"note":   "certificate material is named by path and never served",
		}, nil
	}
}

// ---- config_snapshot_get ----

const (
	snapshotDefaultMaxBytes = 256 * 1024
	snapshotHardMaxBytes    = 2 * 1024 * 1024
)

type snapshotGetIn struct {
	Target     string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Components string `json:"components,omitempty" jsonschema:"comma-separated snapshot domains to include; empty for all"`
	MaxBytes   int    `json:"max_bytes,omitempty" jsonschema:"largest document returned inline (default 262144, max 2097152); a larger snapshot is described, not returned"`
}

func (d *Deps) configSnapshotGet() sdk.ToolHandlerFor[snapshotGetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in snapshotGetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		limit := in.MaxBytes
		if limit <= 0 {
			limit = snapshotDefaultMaxBytes
		}
		if limit > snapshotHardMaxBytes {
			limit = snapshotHardMaxBytes
		}
		q := url.Values{}
		if in.Components != "" {
			q.Set("components", in.Components)
		}
		var raw json.RawMessage
		if err := c.GetQ(ctx, "/config/snapshot", q, &raw); err != nil {
			d.audit("config_snapshot_get", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("config_snapshot_get", c.Name(), true, "")
		sum := sha256.Sum256(raw)
		out := map[string]any{
			"target":     c.Name(),
			"size_bytes": len(raw),
			"sha256":     hex.EncodeToString(sum[:]),
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return nil, nil, fmt.Errorf("the snapshot is not a JSON object: %w", err)
		}
		if len(raw) > limit {
			sizes := map[string]int{}
			for k, v := range top {
				sizes[clean(k)] = len(v)
			}
			out["truncated"] = true
			out["sections"] = sizes
			out["note"] = fmt.Sprintf("snapshot of %d bytes exceeds max_bytes %d; the section sizes are listed, raise max_bytes or narrow components to read it", len(raw), limit)
			return nil, out, nil
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, nil, err
		}
		out["truncated"] = false
		out["snapshot"] = guard.RedactDeep(doc)
		out["note"] = "secret-shaped fields are masked; the document is not sanitize-capped so it stays whole"
		return nil, out, nil
	}
}
