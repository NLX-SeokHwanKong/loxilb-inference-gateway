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
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/client"
	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// trafficReportCaveat is surfaced in every ai_traffic_report result so the
// numbers are read with the right accounting model in mind.
// Values of the outcome label on loxilb_ai_requests_total, as emitted by
// api/prometheus. Absent on gateways older than the label.
const (
	outcomeCompleted = "completed"
	outcomeAborted   = "aborted"
	outcomeDenied    = "denied"
)

// Values of the cause label of an aborted request. client is traffic (a
// cancel, a stall); engine and gateway are errors.
const (
	causeEngine  = "engine"
	causeGateway = "gateway"
)

const trafficReportCaveat = "accounting caveat: read loxilb_ai_requests_total by its outcome " +
	"label - outcome=\"completed\" is traffic answered whole, outcome=\"aborted\" is requests whose " +
	"client did not get the whole answer (cause client = it cancelled or stopped reading, traffic; " +
	"engine or gateway = an error), outcome=\"denied\" is requests the policy gate refused, and " +
	"unfiltered is offered load. error_ratio is (completed 5xx + aborted engine/gateway) / " +
	"(completed + aborted). loxilb_ai_rate_limit_hits_total is not a " +
	"second copy of the denials: it carries WHY they were denied. The bridge reaches gateways of " +
	"any age, so absence of the outcome label means an older build in which the total excludes " +
	"denials altogether, and one older still counts only SSE-terminated streams; when the label " +
	"is absent, cross-check with loxilb_proxy_http_responses_total."

// RegisterAI adds the AI-gateway operation tools. Read tools are
// viewer+, non-destructive
// mutations operator+, ai_apikey_delete admin-only behind the confirm-token
// flow. Raw API-key material never enters model context by default (T5):
// ai_apikey_create writes the key to SecretsDir unless reveal=true.
func RegisterAI(s *sdk.Server, role guard.Role, pol *guard.Policy, deps *Deps) {
	reg := func(meta guard.ToolMeta, add func()) {
		meta.Domain = domainAI
		noteMeta(meta)
		if pol.Permits(role, meta) {
			add()
		}
	}
	ro := func(name string) guard.ToolMeta { return guard.ToolMeta{Name: name} }
	mut := func(name string) guard.ToolMeta { return guard.ToolMeta{Name: name, Mutating: true} }
	dest := func(name string) guard.ToolMeta {
		return guard.ToolMeta{Name: name, Mutating: true, Destructive: true}
	}

	// ---- read tools ----

	reg(ro("ai_apikey_list"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_apikey_list",
			Description: "List AI-gateway API keys, optionally filtered by tenant (GET /config/ai/apikey). Returns key metadata only, never key material.",
			Annotations: roAnnotations("List AI API keys"),
		}, deps.aiApikeyList())
	})
	reg(ro("ai_apikey_get"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_apikey_get",
			Description: "Get one AI-gateway API key summary by key_id (GET /config/ai/apikey/{key_id}). Metadata only, never key material.",
			Annotations: roAnnotations("Get AI API key"),
		}, deps.aiApikeyGet())
	})
	reg(ro("ai_ratelimit_get"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_ratelimit_get",
			Description: "Get the per-tenant AI rate-limit configuration (GET /config/ai/tenant/ratelimit/{tenant_id}): requests/s and LLM tokens/min quotas.",
			Annotations: roAnnotations("Get tenant AI rate limit"),
		}, deps.aiRatelimitGet())
	})
	reg(ro("ai_kv_inventory_get"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "ai_kv_inventory_get",
			Description: "Dump the KV-cache block hash inventory tracked for one endpoint of an AI service " +
				"(GET /config/ai/kv/inventory?service_id&ep_idx). Returns total block count and a bounded hash sample.",
			Annotations: roAnnotations("Get KV-cache inventory"),
		}, deps.aiKvInventoryGet())
	})
	reg(ro("gpu_status"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "gpu_status",
			Description: "Get GPU-aware load-balancing status and statistics (GET /config/gpu/status).",
			Annotations: roAnnotations("Get GPU monitoring status"),
		}, deps.passthrough("gpu_status", "/config/gpu/status"))
	})
	reg(ro("gpu_worker_metrics_get"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "gpu_worker_metrics_get",
			Description: "Get current GPU metrics for all tracked workers as reported by the metrics agent (GET /config/worker/metrics).",
			Annotations: roAnnotations("Get GPU worker metrics"),
		}, deps.passthrough("gpu_worker_metrics_get", "/config/worker/metrics"))
	})
	reg(ro("llamafw_status"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "llamafw_status",
			Description: "Get LlamaFirewall AI-security scanner status: enabled flag, server connection, policy, enabled scanners (GET /config/llamafirewall/status).",
			Annotations: roAnnotations("Get LlamaFirewall status"),
		}, deps.passthrough("llamafw_status", "/config/llamafirewall/status"))
	})
	reg(ro("llamafw_stats"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "llamafw_stats",
			Description: "Get LlamaFirewall scanning statistics: scans, blocks, per-scanner performance and decisions (GET /config/llamafirewall/stats).",
			Annotations: roAnnotations("Get LlamaFirewall stats"),
		}, deps.passthrough("llamafw_stats", "/config/llamafirewall/stats"))
	})
	reg(ro("pii_status"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "pii_status",
			Description: "Get PII-detection (Presidio) configuration and status (GET /config/pii/status).",
			Annotations: roAnnotations("Get PII detection status"),
		}, deps.passthrough("pii_status", "/config/pii/status"))
	})
	reg(ro("pii_stats"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "pii_stats",
			Description: "Get PII-detection statistics: scans, detections, blocks, errors (GET /config/pii/stats).",
			Annotations: roAnnotations("Get PII detection stats"),
		}, deps.passthrough("pii_stats", "/config/pii/stats"))
	})
	reg(ro("ai_objective_list"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_objective_list",
			Description: "List objectives with the source the gateway applies them from: the AI key store, its last read in this process, or the last-known-good file (GET /config/ai/objective).",
			Annotations: roAnnotations("List objectives"),
		}, deps.aiObjectiveList())
	})
	reg(ro("ai_netidentity_list"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_netidentity_list",
			Description: "List network-identity bindings as stored: match (cidr, ingress_if, vlan, vni or ipsec_tunnel), services, tenant, allowed models, enforcement (GET /config/ai/netidentity).",
			Annotations: roAnnotations("List network identities"),
		}, deps.aiNetidentityList())
	})
	reg(ro("ai_jwtprofile_list"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_jwtprofile_list",
			Description: "List JWT auth profiles: issuer, JWKS URL, audiences, algorithms, claim mapping (GET /config/ai/jwtauthprofile). Desired configuration only, no secrets and no keyset health.",
			Annotations: roAnnotations("List JWT auth profiles"),
		}, deps.aiJwtprofileList())
	})
	reg(ro("ai_user_ratelimit_list"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_user_ratelimit_list",
			Description: "List a tenant's explicit per-user rate-limit rows, without model limits (GET /config/ai/user/ratelimit/{tenant_id}).",
			Annotations: roAnnotations("List user rate limits"),
		}, deps.aiUserRatelimitList())
	})
	reg(ro("ai_user_ratelimit_get"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_user_ratelimit_get",
			Description: "One user's explicit rate-limit row including model limits (GET /config/ai/user/ratelimit/{tenant_id}/{user_id}).",
			Annotations: roAnnotations("Get a user rate limit"),
		}, deps.aiUserRatelimitGet())
	})
	reg(ro("ai_ratelimit_defaults_get"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_ratelimit_defaults_get",
			Description: "The rate-limit defaults row of a scope: global, or rule with rule_ident naming the service (GET /config/ai/ratelimit/defaults/{scope}).",
			Annotations: roAnnotations("Get rate-limit defaults"),
		}, deps.aiRatelimitDefaultsGet())
	})
	reg(ro("ai_model_profile_list"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_model_profile_list",
			Description: "List the published model-prompt profiles of the current registry generation (GET /config/ai/model-profiles); a cache of the publication, not an authority.",
			Annotations: roAnnotations("List model profiles"),
		}, deps.aiModelProfileList())
	})
	reg(ro("ai_model_profile_get"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "ai_model_profile_get",
			Description: "One published model-prompt profile by id (GET /config/ai/model-profiles/{profile_id}).",
			Annotations: roAnnotations("Get a model profile"),
		}, deps.aiModelProfileGet())
	})
	reg(ro("ai_traffic_report"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "ai_traffic_report",
			Description: "Composite AI-gateway traffic report from the loxilb_ai_* metric families: per-model/tenant " +
				"request counts by status, error ratio, rate-limit drops, active streams, latency quantiles " +
				"(request duration, L7 TTFB, PD TTFT) and KV-cache session-affinity counters. Values are " +
				"cumulative counters since process start; use promql_query rate() for rates.",
			Annotations: roAnnotations("AI traffic report"),
		}, deps.aiTrafficReport())
	})

	// ---- mutating, non-destructive (operator+) ----

	reg(mut("ai_apikey_create"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "ai_apikey_create",
			Description: "Create a tenant API key (POST /config/ai/apikey): allowed models, rps/burst/tokens-per-min " +
				"quotas, optional expiry, enabled flag, idempotency handle, priority class and objectives. There is no " +
				"key import: the gateway mints the key. The raw key exists only once: by default it is written to a " +
				"mode-0600 file on the bridge host and only the file path is returned; pass reveal=true to return it " +
				"inline instead (discouraged). Tenant impact. The first call previews the key to be created with a " +
				"confirm_token; repeat with the token, or call once with apply=true, to create it. The executed " +
				"result carries key_id and key_file (or raw_key on reveal); the rollback is ai_apikey_delete with that key_id.",
			Annotations: mutAnnotations("Create AI API key", false),
			InputSchema: apikeyCreateInputSchema(),
		}, deps.aiApikeyCreate())
	})
	reg(mut("ai_apikey_update"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "ai_apikey_update",
			Description: "Update an API key in place (PATCH /config/ai/apikey/{key_id}): allowed_models (an empty list " +
				"clears the restriction), enabled, rate_limit_rps, burst_size, tokens_per_min, priority_class (an " +
				"empty string clears it), default_objective (an empty string clears it), allowed_objectives (an empty " +
				"list clears it). Only the named fields are sent; at least one is required. Object impact. The first call " +
				"pre-reads the key and previews current, proposed, diff and a rollback with the current values plus a " +
				"confirm_token; repeat with the token, or call once with apply=true, to apply. The executed result " +
				"re-reads the key. Disabling a key is reversible; deleting (ai_apikey_delete) is not.",
			Annotations: mutAnnotations("Update AI API key", true),
			InputSchema: apikeyUpdateInputSchema(),
		}, deps.aiApikeyUpdate())
	})
	reg(mut("ai_ratelimit_set"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "ai_ratelimit_set",
			Description: "Create or replace a tenant's AI rate limit (POST /config/ai/tenant/ratelimit): requests/s, " +
				"LLM tokens/min, burst_pct (sent on every call: the row is an upsert), per-model token quotas " +
				"(model_limits; a model with tokens_per_min 0 loses its quota, other models keep theirs), and the " +
				"tenant's priority_class, default_objective and allowed_objectives (sent only when named; an empty " +
				"string or list clears). There is no delete endpoint; quotas of 0 are unlimited. Tenant impact. " +
				"The first call pre-reads the row and previews current, proposed, diff and a rollback plus a " +
				"confirm_token; repeat with the token, or call once with apply=true, to apply. The executed result re-reads the row.",
			Annotations: mutAnnotations("Set tenant AI rate limit", true),
		}, deps.aiRatelimitSet())
	})
	reg(mut("ai_user_ratelimit_set"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "ai_user_ratelimit_set",
			Description: "Create or replace one user's explicit rate-limit row inside a tenant (POST /config/ai/user/ratelimit; " +
				"QoS ladder level 1): rps, burst_size (0 defaults to rps), tokens_per_min and per-model token quotas. " +
				"A zero field constrains nothing and falls through to the defaults; an entry whose limits are all zero " +
				"is refused before any request (the API rejects it; ai_user_ratelimit_delete removes limits). The " +
				"model rows are replaced as a set: a call without model_limits clears them. Tenant impact. The first " +
				"call pre-reads the row and previews current, proposed, diff and a rollback (the current row, or " +
				"ai_user_ratelimit_delete when none exists) plus a confirm_token; repeat with the token, or call once " +
				"with apply=true, to apply. The executed result re-reads the row.",
			Annotations: mutAnnotations("Set a user rate limit", true),
		}, deps.aiUserRatelimitSet())
	})
	reg(mut("ai_user_ratelimit_delete"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "ai_user_ratelimit_delete",
			Description: "Remove one user's explicit rate-limit row and its model rows " +
				"(DELETE /config/ai/user/ratelimit/{tenant_id}/{user_id}); the user falls back to the defaults, then to " +
				"unlimited. Re-creatable: the preview carries the row as the arguments of an ai_user_ratelimit_set " +
				"rollback. Tenant impact. Two-step: the first call pre-reads the row (a missing row is refused) and " +
				"previews it with a confirm_token; repeat with the token to delete. The executed result re-reads the key " +
				"({exists: false} when the row is gone).",
			Annotations: mutAnnotations("Delete a user rate limit", true),
		}, deps.aiUserRatelimitDelete())
	})
	reg(mut("llamafw_health_check"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name:        "llamafw_health_check",
			Description: "Trigger a connectivity/health check of the LlamaFirewall gRPC server (POST /config/llamafirewall/health).",
			Annotations: mutAnnotations("LlamaFirewall health check", true),
		}, deps.postSimple("llamafw_health_check", "/config/llamafirewall/health", llamafwStatus, "server_url", "connected", "last_health_check"))
	})

	// ---- admin-confirm: global-scope changes and deletes ----
	// Every tool here previews first (current, proposed, diff, rollback) and
	// applies only with the single-use confirm_token the preview issued.

	reg(dest("gpu_mode_set"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "gpu_mode_set",
			Description: "Enable or disable GPU-aware load balancing (POST /config/gpu/enable | /config/gpu/disable). Disabling " +
				"reverts to standard CHWBL routing. Global: changes the routing of every GPU-aware service. Two-step: preview " +
				"(current, proposed, rollback) with confirm_token, then repeat with the token to apply.",
			Annotations: destAnnotations("Set GPU-aware mode"),
		}, deps.gpuModeSet())
	})
	reg(dest("gpu_conversations_cleanup"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "gpu_conversations_cleanup",
			Description: "Remove stale GPU conversation mappings older than max_age_hours (POST /config/gpu/conversations/cleanup). " +
				"Global and not reversible. Two-step: preview with confirm_token, then repeat with the token to apply.",
			Annotations: destAnnotations("Clean up GPU conversations"),
		}, deps.gpuConversationsCleanup())
	})
	reg(dest("llamafw_enable_set"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "llamafw_enable_set",
			Description: "Enable or disable LlamaFirewall AI-security scanning (POST /config/llamafirewall/enable). Global. " +
				"Two-step: preview (current flag, proposed, rollback) with confirm_token, then repeat with the token to apply.",
			Annotations: destAnnotations("Toggle LlamaFirewall"),
		}, deps.enableFlagTool("llamafw_enable_set", "/config/llamafirewall/enable", llamafwStatus, "global"))
	})
	reg(dest("llamafw_configure"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "llamafw_configure",
			Description: "Configure LlamaFirewall scanning (POST /config/llamafirewall/configure): server URL, timeout, " +
				"fail policy, block threshold, caching, scan/skip URL patterns. Only supplied fields are sent. Global. " +
				"Two-step: preview (current, proposed, diff, rollback) with confirm_token, then repeat with the token to apply.",
			Annotations: destAnnotations("Configure LlamaFirewall"),
		}, deps.llamafwConfigure())
	})
	reg(dest("llamafw_scanners_set"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "llamafw_scanners_set",
			Description: "Enable/disable individual LlamaFirewall scanners (POST /config/llamafirewall/scanners): " +
				"prompt_guard, code_shield, regex, hidden_ascii, agent_alignment, pii_detection. Only supplied fields are sent. " +
				"Global. Two-step: preview (current, proposed, diff, rollback) with confirm_token, then repeat with the token to apply.",
			Annotations: destAnnotations("Set LlamaFirewall scanners"),
		}, deps.llamafwScannersSet())
	})
	reg(dest("pii_enable_set"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "pii_enable_set",
			Description: "Enable or disable PII detection for HTTP/HTTPS traffic (POST /config/pii/enable). Global. " +
				"Two-step: preview (current flag, proposed, rollback) with confirm_token, then repeat with the token to apply.",
			Annotations: destAnnotations("Toggle PII detection"),
		}, deps.enableFlagTool("pii_enable_set", "/config/pii/enable", piiStatus, "global"))
	})
	reg(dest("pii_configure"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "pii_configure",
			Description: "Configure PII detection (POST /config/pii/configure): mode detect|mask|redact|anonymize, " +
				"direction, fail mode, scan mode, Presidio analyzer/anonymizer URLs, score threshold, timeout. Only supplied " +
				"fields are sent. Global. Two-step: preview (current, proposed, diff, rollback) with confirm_token, then repeat " +
				"with the token to apply.",
			Annotations: destAnnotations("Configure PII detection"),
		}, deps.piiConfigure())
	})
	reg(dest("pii_url_patterns_set"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "pii_url_patterns_set",
			Description: "Add, replace, or clear the URL patterns selecting which requests are PII-scanned (POST /config/pii/url-patterns). " +
				"Global. Two-step: preview (current list, proposed list, rollback) with confirm_token, then repeat with the token to apply.",
			Annotations: destAnnotations("Set PII URL patterns"),
		}, deps.piiURLPatternsSet())
	})

	reg(dest("ai_apikey_delete"), func() {
		sdk.AddTool(s, &sdk.Tool{
			Name: "ai_apikey_delete",
			Description: "Permanently delete an API key by key_id (DELETE /config/ai/apikey/{key_id}). Irreversible — " +
				"clients using the key lose access immediately (consider ai_apikey_update enabled=false instead). " +
				"Two-step confirm-token flow: first call previews, second call with confirm_token executes.",
			Annotations: destAnnotations("Delete AI API key"),
		}, deps.aiApikeyDelete())
	})
}

// stripKeyMaterial removes any raw-key-shaped field from decoded REST
// responses before they reach model context (T5 defense in depth; list/get
// responses must never carry key material, but do not rely on it).
// status readers the change gate uses for the toggles and the trigger.
func llamafwStatus(ctx context.Context, c *client.Client) (map[string]any, error) {
	return c.LlamaFirewallStatus(ctx)
}

func piiStatus(ctx context.Context, c *client.Client) (map[string]any, error) {
	return c.PIIStatus(ctx)
}

func stripKeyMaterial(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			lk := strings.ToLower(k)
			if lk == "raw_key" || lk == "rawkey" || lk == "api_key" || lk == "apikey" ||
				lk == "secret" || lk == "token" {
				out[k] = "[REDACTED]"
				continue
			}
			out[k] = stripKeyMaterial(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = stripKeyMaterial(val)
		}
		return out
	default:
		return v
	}
}

// ---- ai_apikey_list / ai_apikey_get ----

type apikeyListIn struct {
	Target   string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	TenantID string `json:"tenant_id,omitempty" jsonschema:"filter keys by tenant id"`
}

func (d *Deps) aiApikeyList() sdk.ToolHandlerFor[apikeyListIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in apikeyListIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		q := url.Values{}
		if in.TenantID != "" {
			q.Set("tenant_id", in.TenantID)
		}
		var raw any
		if err := c.GetQ(ctx, "/config/ai/apikey", q, &raw); err != nil {
			d.audit("ai_apikey_list", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_apikey_list", c.Name(), true, "")
		return nil, map[string]any{
			"target": c.Name(),
			"keys":   stripKeyMaterial(sanitizeAny(raw, 0)),
		}, nil
	}
}

type apikeyGetIn struct {
	Target string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	KeyID  string `json:"key_id" jsonschema:"API key identifier from ai_apikey_list"`
}

func (d *Deps) aiApikeyGet() sdk.ToolHandlerFor[apikeyGetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in apikeyGetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		if err := validatePathSegment(in.KeyID); err != nil {
			return nil, nil, fmt.Errorf("key_id: %w", err)
		}
		var raw any
		if err := c.Get(ctx, "/config/ai/apikey/"+url.PathEscape(in.KeyID), &raw); err != nil {
			d.audit("ai_apikey_get", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_apikey_get", c.Name(), true, "")
		return nil, map[string]any{
			"target": c.Name(),
			"key":    stripKeyMaterial(sanitizeAny(raw, 0)),
		}, nil
	}
}

// ---- ai_apikey_create ----

type apikeyCreateIn struct {
	Target            string   `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	TenantID          string   `json:"tenant_id" jsonschema:"tenant that owns the key"`
	Name              string   `json:"name,omitempty" jsonschema:"human-readable key label"`
	AllowedModels     []string `json:"allowed_models,omitempty" jsonschema:"model identifiers this key may access; empty allows all"`
	RateLimitRPS      int      `json:"rate_limit_rps,omitempty" jsonschema:"max requests per second for this key"`
	BurstSize         int      `json:"burst_size,omitempty" jsonschema:"total request-bucket capacity, not additional capacity above rate_limit_rps; zero uses the per-key RPS; a nonpositive RPS skips this limiter"`
	TokensPerMin      int      `json:"tokens_per_min,omitempty" jsonschema:"max LLM tokens per minute for this key"`
	ExpiresAt         string   `json:"expires_at,omitempty" jsonschema:"optional expiry timestamp (RFC3339)"`
	Enabled           *bool    `json:"enabled,omitempty" jsonschema:"whether the key is active; omitted means enabled"`
	ClientOpID        string   `json:"client_op_id,omitempty" jsonschema:"optional idempotency handle (at most 128 printable non-space ASCII characters, unique per tenant); a retry carrying the same value converges on the row the first create made"`
	PriorityClass     string   `json:"priority_class,omitempty" jsonschema:"the highest priority class a request with this key may have; permitted values come from the REST contract (generated description)"`
	DefaultObjective  string   `json:"default_objective,omitempty" jsonschema:"the objective a request with this key gets when it names none, or one it may not use; counts only when the tenant's allowed_objectives hold it"`
	AllowedObjectives []string `json:"allowed_objectives,omitempty" jsonschema:"the objectives a request with this key may ask for by header, and the body tiers (tier:priority, tier:fast, tier:flex) by service_tier, out of the ones its tenant allows; at most 64"`
	Reveal            bool     `json:"reveal,omitempty" jsonschema:"true to return the raw key inline instead of writing it to a file on the bridge host (discouraged: the key then enters model context)"`
	Apply             bool     `json:"apply,omitempty" jsonschema:"operator convenience: create without the preview step"`
	ConfirmToken      string   `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

// apikeyCreateInputSchema carries the generated priority_class values.
func apikeyCreateInputSchema() *jsonschema.Schema {
	return inputSchemaWithEnums[apikeyCreateIn]("ai_apikey_create", map[string]string{"priority_class": "ApiKeyCreateRequest.priority_class"})
}

func (d *Deps) aiApikeyCreate() sdk.ToolHandlerFor[apikeyCreateIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in apikeyCreateIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		if strings.TrimSpace(in.TenantID) == "" {
			return nil, mutOut{}, fmt.Errorf("tenant_id is required")
		}
		if !in.Reveal && d.SecretsDir == "" {
			return nil, mutOut{}, fmt.Errorf("no secrets_dir configured on the bridge; " +
				"configure one or pass reveal=true to accept the key in the response")
		}
		if in.PriorityClass != "" && !enumAllows("ApiKeyCreateRequest.priority_class", in.PriorityClass) {
			return nil, mutOut{}, fmt.Errorf("priority_class: must be omitted or one of %s", enumValues("ApiKeyCreateRequest.priority_class"))
		}
		if len(in.AllowedObjectives) > 64 {
			return nil, mutOut{}, fmt.Errorf("allowed_objectives: %d exceeds the maximum of 64", len(in.AllowedObjectives))
		}
		body := map[string]any{"tenant_id": in.TenantID}
		if in.Name != "" {
			body["name"] = in.Name
		}
		if len(in.AllowedModels) > 0 {
			body["allowed_models"] = in.AllowedModels
		}
		if in.RateLimitRPS > 0 {
			body["rate_limit_rps"] = in.RateLimitRPS
		}
		if in.BurstSize > 0 {
			body["burst_size"] = in.BurstSize
		}
		if in.TokensPerMin > 0 {
			body["tokens_per_min"] = in.TokensPerMin
		}
		if in.ExpiresAt != "" {
			body["expires_at"] = in.ExpiresAt
		}
		if in.Enabled != nil {
			body["enabled"] = *in.Enabled
		}
		if in.ClientOpID != "" {
			body["client_op_id"] = in.ClientOpID
		}
		if in.PriorityClass != "" {
			body["priority_class"] = in.PriorityClass
		}
		if in.DefaultObjective != "" {
			body["default_objective"] = in.DefaultObjective
		}
		if len(in.AllowedObjectives) > 0 {
			body["allowed_objectives"] = in.AllowedObjectives
		}
		args := in
		args.ConfirmToken = ""
		if !in.Apply || in.ConfirmToken != "" {
			out, stop, err := d.gateChange(ctx, "ai_apikey_create", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
				return changePlan{
					Current:  nil,
					Proposed: body,
					Rollback: &rollbackSpec{Note: "the key does not exist yet: ai_apikey_delete with the key_id the executed result returns removes it (irreversible); ai_apikey_update enabled=false disables it instead"},
					Impact:   "tenant",
				}, nil
			})
			if err != nil || stop {
				return nil, out, err
			}
		}
		var res struct {
			RawKey string `json:"raw_key"`
			KeyID  string `json:"key_id"`
		}
		if err := c.Post(ctx, "/config/ai/apikey", body, &res); err != nil {
			d.auditMut("ai_apikey_create", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		result := map[string]any{"key_id": clean(res.KeyID)}
		if in.Reveal {
			result["raw_key"] = res.RawKey
			result["note"] = "raw key returned inline on explicit request; it is shown ONCE and cannot be retrieved again"
		} else {
			path, werr := writeSecretFile(d.SecretsDir, "apikey-"+sanitizeFileToken(res.KeyID), res.RawKey)
			if werr != nil {
				d.auditMut("ai_apikey_create", c.Name(), args, false, "secret file: "+werr.Error())
				return nil, mutOut{}, fmt.Errorf("key %s created but writing the secret file failed: %w; "+
					"delete and re-create the key (the raw key is not recoverable)", clean(res.KeyID), werr)
			}
			result["key_file"] = path
			result["note"] = "raw key written to key_file (mode 0600) on the bridge host; it is stored NOWHERE else and cannot be retrieved again"
		}
		d.auditMut("ai_apikey_create", c.Name(), args, true, "")
		out := mutOut{Target: c.Name(), Action: "executed", Result: result}
		if res.KeyID != "" {
			if key, err := c.APIKey(ctx, res.KeyID); err == nil {
				out.ReadBack = stripKeyMaterial(sanitizeAny(key, 0))
			}
		}
		return nil, out, nil
	}
}

// sanitizeFileToken reduces a server-supplied id to a safe filename fragment.
func sanitizeFileToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}

// writeSecretFile writes content to dir/<name>.key with 0600 perms (dir
// created 0700), refusing to overwrite an existing file.
func writeSecretFile(dir, name, content string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".key")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content + "\n"); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}

// ---- ai_apikey_update ----

type apikeyUpdateIn struct {
	Target            string   `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	KeyID             string   `json:"key_id" jsonschema:"API key identifier"`
	AllowedModels     []string `json:"allowed_models,omitempty" jsonschema:"replacement list of models the key may access; an empty list clears the restriction (all models)"`
	Enabled           *bool    `json:"enabled,omitempty" jsonschema:"enable or disable the key"`
	RateLimitRPS      *int     `json:"rate_limit_rps,omitempty" jsonschema:"max requests per second for this key; 0 skips the limiter"`
	BurstSize         *int     `json:"burst_size,omitempty" jsonschema:"total request-bucket capacity; 0 uses the per-key RPS"`
	TokensPerMin      *int     `json:"tokens_per_min,omitempty" jsonschema:"max LLM tokens per minute for this key; 0 leaves the dimension unlimited"`
	PriorityClass     *string  `json:"priority_class,omitempty" jsonschema:"the highest priority class a request with this key may have; permitted values come from the REST contract (generated description); an empty string clears the cap"`
	DefaultObjective  *string  `json:"default_objective,omitempty" jsonschema:"the objective a request with this key gets when it names none; an empty string clears it"`
	AllowedObjectives []string `json:"allowed_objectives,omitempty" jsonschema:"the objectives and body tiers a request with this key may ask for; an empty list clears the list"`
	Apply             bool     `json:"apply,omitempty" jsonschema:"operator convenience: apply without the preview step"`
	ConfirmToken      string   `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

// apikeyUpdateInputSchema carries the generated priority_class values.
func apikeyUpdateInputSchema() *jsonschema.Schema {
	return inputSchemaWithEnums[apikeyUpdateIn]("ai_apikey_update", map[string]string{"priority_class": "ApiKeyCreateRequest.priority_class"})
}

// apikeyPatchKeys are the eight patchable fields of PATCH /config/ai/apikey/{key_id}.
var apikeyPatchKeys = []string{"allowed_models", "enabled", "rate_limit_rps", "burst_size", "tokens_per_min", "priority_class", "default_objective", "allowed_objectives"}

func (d *Deps) aiApikeyUpdate() sdk.ToolHandlerFor[apikeyUpdateIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in apikeyUpdateIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		if err := validatePathSegment(in.KeyID); err != nil {
			return nil, mutOut{}, fmt.Errorf("key_id: %w", err)
		}
		patch := map[string]any{}
		if in.AllowedModels != nil {
			patch["allowed_models"] = in.AllowedModels
		}
		if in.Enabled != nil {
			patch["enabled"] = *in.Enabled
		}
		for _, f := range []struct {
			name string
			v    *int
		}{{"rate_limit_rps", in.RateLimitRPS}, {"burst_size", in.BurstSize}, {"tokens_per_min", in.TokensPerMin}} {
			if f.v == nil {
				continue
			}
			if *f.v < 0 {
				return nil, mutOut{}, fmt.Errorf("%s: must be >= 0", f.name)
			}
			patch[f.name] = *f.v
		}
		if in.PriorityClass != nil {
			if *in.PriorityClass != "" && !enumAllows("ApiKeyCreateRequest.priority_class", *in.PriorityClass) {
				return nil, mutOut{}, fmt.Errorf("priority_class: must be empty (clears) or one of %s", enumValues("ApiKeyCreateRequest.priority_class"))
			}
			patch["priority_class"] = *in.PriorityClass
		}
		if in.DefaultObjective != nil {
			patch["default_objective"] = *in.DefaultObjective
		}
		if in.AllowedObjectives != nil {
			if len(in.AllowedObjectives) > 64 {
				return nil, mutOut{}, fmt.Errorf("allowed_objectives: %d exceeds the maximum of 64", len(in.AllowedObjectives))
			}
			patch["allowed_objectives"] = in.AllowedObjectives
		}
		if len(patch) == 0 {
			return nil, mutOut{}, fmt.Errorf("no fields to update: name at least one of %s", strings.Join(apikeyPatchKeys, ", "))
		}
		args := in
		args.ConfirmToken = ""
		if !in.Apply || in.ConfirmToken != "" {
			out, stop, err := d.gateChange(ctx, "ai_apikey_update", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
				key, err := c.APIKey(ctx, in.KeyID)
				if err != nil {
					return changePlan{}, fmt.Errorf("read of the key failed, nothing was sent: %w", err)
				}
				current := pick(mapValue(stripKeyMaterial(key)), apikeyPatchKeys...)
				rb := map[string]any{"key_id": in.KeyID}
				for k := range patch {
					if v, ok := current[k]; ok {
						rb[k] = v
						continue
					}
					// A field the summary does not carry is at its zero
					// value: enabled absent means enabled, a list or
					// string absent means cleared, a quota absent means 0.
					switch k {
					case "enabled":
						rb[k] = true
					case "allowed_models", "allowed_objectives":
						rb[k] = []string{}
					case "priority_class", "default_objective":
						rb[k] = ""
					default:
						rb[k] = 0
					}
				}
				return changePlan{
					Current:  current,
					Proposed: overlay(current, patch),
					Rollback: &rollbackSpec{Tool: "ai_apikey_update", Args: rb},
					Impact:   "object",
				}, nil
			})
			if err != nil || stop {
				return nil, out, err
			}
		}
		var res any
		if err := c.Patch(ctx, "/config/ai/apikey/"+url.PathEscape(in.KeyID), patch, &res); err != nil {
			d.auditMut("ai_apikey_update", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("ai_apikey_update", c.Name(), args, true, "")
		after, _ := c.APIKey(ctx, in.KeyID)
		return nil, executed(c.Name(), res, stripKeyMaterial(sanitizeAny(after, 0))), nil
	}
}

// ---- ai_apikey_delete ----

type apikeyDeleteIn struct {
	Target       string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	KeyID        string `json:"key_id" jsonschema:"API key identifier to delete"`
	ConfirmToken string `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

func (d *Deps) aiApikeyDelete() sdk.ToolHandlerFor[apikeyDeleteIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in apikeyDeleteIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		if err := validatePathSegment(in.KeyID); err != nil {
			return nil, mutOut{}, fmt.Errorf("key_id: %w", err)
		}
		args := in
		args.ConfirmToken = ""
		if d.Confirm != nil && in.ConfirmToken == "" {
			preview := d.aiApikeyDeletePreview(ctx, c, in.KeyID)
			out, stop, err := d.gateDestructive("ai_apikey_delete", c.Name(), "", args, preview)
			if err != nil || stop {
				return nil, out, err
			}
		} else {
			if _, _, err := d.gateDestructive("ai_apikey_delete", c.Name(), in.ConfirmToken, args, nil); err != nil {
				return nil, mutOut{}, err
			}
		}
		var res any
		if err := c.Delete(ctx, "/config/ai/apikey/"+url.PathEscape(in.KeyID), &res); err != nil {
			d.auditMut("ai_apikey_delete", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("ai_apikey_delete", c.Name(), args, true, "")
		return nil, mutOut{Target: c.Name(), Action: "executed", Result: sanitizeAny(res, 0)}, nil
	}
}

// aiApikeyDeletePreview best-effort fetches the key summary about to be deleted.
func (d *Deps) aiApikeyDeletePreview(ctx context.Context, c *client.Client, keyID string) any {
	var raw any
	if err := c.Get(ctx, "/config/ai/apikey/"+url.PathEscape(keyID), &raw); err != nil {
		return map[string]any{"error": "preview unavailable: " + clean(err.Error())}
	}
	return map[string]any{"deleting_key": stripKeyMaterial(sanitizeAny(raw, 0))}
}

// ---- ai_ratelimit_get / ai_ratelimit_set ----

type ratelimitGetIn struct {
	Target   string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	TenantID string `json:"tenant_id" jsonschema:"tenant identifier"`
}

func (d *Deps) aiRatelimitGet() sdk.ToolHandlerFor[ratelimitGetIn, map[string]any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ratelimitGetIn) (*sdk.CallToolResult, map[string]any, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, nil, err
		}
		if err := validatePathSegment(in.TenantID); err != nil {
			return nil, nil, fmt.Errorf("tenant_id: %w", err)
		}
		var raw any
		if err := c.Get(ctx, "/config/ai/tenant/ratelimit/"+url.PathEscape(in.TenantID), &raw); err != nil {
			d.audit("ai_ratelimit_get", c.Name(), false, err.Error())
			return nil, nil, err
		}
		d.audit("ai_ratelimit_get", c.Name(), true, "")
		return nil, map[string]any{"target": c.Name(), "ratelimit": sanitizeAny(raw, 0)}, nil
	}
}

// modelLimitIn is one per-model token quota (TenantModelRateLimit,
// UserModelRateLimit on the wire: model, tokens_per_min).
type modelLimitIn struct {
	Model        string `json:"model" jsonschema:"model name the quota applies to"`
	TokensPerMin int    `json:"tokens_per_min" jsonschema:"max LLM tokens per minute for this model; 0 removes the model quota"`
}

func modelLimitsBody(in []modelLimitIn) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(in))
	for i, ml := range in {
		if strings.TrimSpace(ml.Model) == "" {
			return nil, fmt.Errorf("model_limits[%d].model is required", i)
		}
		if ml.TokensPerMin < 0 {
			return nil, fmt.Errorf("model_limits[%d].tokens_per_min must be >= 0", i)
		}
		out = append(out, map[string]any{"model": ml.Model, "tokens_per_min": ml.TokensPerMin})
	}
	return out, nil
}

// modelLimitsView projects a row's model_limits list for a preview or a
// rollback: [{model, tokens_per_min}].
func modelLimitsView(v any) []map[string]any {
	out := []map[string]any{}
	for _, item := range listValue(v) {
		m := mapValue(item)
		out = append(out, map[string]any{"model": stringValue(m["model"]), "tokens_per_min": intValue(m["tokens_per_min"])})
	}
	return out
}

type ratelimitSetIn struct {
	Target            string         `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	TenantID          string         `json:"tenant_id" jsonschema:"tenant identifier"`
	RPS               int            `json:"rps" jsonschema:"max requests per second (0 = unlimited)"`
	TokensPerMin      int            `json:"tokens_per_min" jsonschema:"max LLM tokens per minute (0 = unlimited)"`
	BurstPct          int            `json:"burst_pct,omitempty" jsonschema:"token bucket capacity as a percent of tokens_per_min (0 = server default)"`
	ModelLimits       []modelLimitIn `json:"model_limits,omitempty" jsonschema:"per-model token quotas; a model with tokens_per_min 0 loses its quota, models not named keep theirs"`
	PriorityClass     *string        `json:"priority_class,omitempty" jsonschema:"the highest priority class a request of this tenant may have: standard, critical or sheddable (the REST contract's words); omitted leaves the stored value, an empty string clears it"`
	DefaultObjective  *string        `json:"default_objective,omitempty" jsonschema:"the objective a request of this tenant gets when it names none; omitted leaves the stored value, an empty string clears it"`
	AllowedObjectives []string       `json:"allowed_objectives,omitempty" jsonschema:"the objectives and body tiers (tier:priority, tier:fast, tier:flex) a request of this tenant may raise itself to; at most 64; omitted leaves the stored list, an empty list clears it"`
	Apply             bool           `json:"apply,omitempty" jsonschema:"operator convenience: apply without the preview step"`
	ConfirmToken      string         `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

// tenantRatelimitKeys are the row's fields the preview and the rollback carry.
var tenantRatelimitKeys = []string{"tenant_id", "rps", "tokens_per_min", "burst_pct", "model_limits", "priority_class", "default_objective", "allowed_objectives"}

func (d *Deps) aiRatelimitSet() sdk.ToolHandlerFor[ratelimitSetIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ratelimitSetIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		if strings.TrimSpace(in.TenantID) == "" {
			return nil, mutOut{}, fmt.Errorf("tenant_id is required")
		}
		if err := validatePathSegment(in.TenantID); err != nil {
			return nil, mutOut{}, fmt.Errorf("tenant_id: %w", err)
		}
		if in.RPS < 0 || in.TokensPerMin < 0 {
			return nil, mutOut{}, fmt.Errorf("rps and tokens_per_min must be >= 0")
		}
		if in.BurstPct < 0 {
			return nil, mutOut{}, fmt.Errorf("burst_pct must be >= 0 (0 = server default)")
		}
		if len(in.AllowedObjectives) > 64 {
			return nil, mutOut{}, fmt.Errorf("allowed_objectives: %d exceeds the maximum of 64", len(in.AllowedObjectives))
		}
		// The endpoint is a full upsert of the tenant's rate-limit row, so
		// the three quotas are sent on every call: omitting burst_pct would
		// not preserve a previously configured value, it would just leave
		// the server to write the same zero this sends explicitly. The
		// class and objective fields keep the stored value when omitted.
		body := map[string]any{
			"tenant_id":      in.TenantID,
			"rps":            in.RPS,
			"tokens_per_min": in.TokensPerMin,
			"burst_pct":      in.BurstPct,
		}
		if in.ModelLimits != nil {
			mls, err := modelLimitsBody(in.ModelLimits)
			if err != nil {
				return nil, mutOut{}, err
			}
			body["model_limits"] = mls
		}
		if in.PriorityClass != nil {
			body["priority_class"] = *in.PriorityClass
		}
		if in.DefaultObjective != nil {
			body["default_objective"] = *in.DefaultObjective
		}
		if in.AllowedObjectives != nil {
			body["allowed_objectives"] = in.AllowedObjectives
		}
		args := in
		args.ConfirmToken = ""
		if !in.Apply || in.ConfirmToken != "" {
			out, stop, err := d.gateChange(ctx, "ai_ratelimit_set", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
				row, found, err := c.TenantRateLimit(ctx, in.TenantID)
				if err != nil {
					return changePlan{}, fmt.Errorf("read of the tenant's rate limit failed, nothing was sent: %w", err)
				}
				var current any // nil when no row exists (not an empty object)
				curMap := map[string]any{}
				rb := map[string]any{"tenant_id": in.TenantID, "rps": 0, "tokens_per_min": 0, "burst_pct": 0}
				rollback := &rollbackSpec{Tool: "ai_ratelimit_set", Args: rb}
				curModels := map[string]bool{}
				if found {
					curMap = pick(row, tenantRatelimitKeys...)
					curMap["model_limits"] = modelLimitsView(row["model_limits"])
					current = curMap
					for _, k := range []string{"rps", "tokens_per_min", "burst_pct"} {
						rb[k] = intValue(row[k])
					}
					for _, ml := range modelLimitsView(row["model_limits"]) {
						curModels[stringValue(ml["model"])] = true
					}
					rb["model_limits"] = modelLimitsView(row["model_limits"])
					for _, k := range []string{"priority_class", "default_objective", "allowed_objectives"} {
						if _, changed := body[k]; !changed {
							continue
						}
						if v, ok := row[k]; ok && v != nil {
							rb[k] = v
						} else if k == "allowed_objectives" {
							rb[k] = []string{}
						} else {
							rb[k] = ""
						}
					}
				} else {
					rollback.Note = "no row existed and the API has no delete: the rollback leaves the row with quotas of 0 (unlimited) and removes the model quotas this call sets"
					rb["model_limits"] = []map[string]any{}
					for _, k := range []string{"priority_class", "default_objective"} {
						if _, changed := body[k]; changed {
							rb[k] = ""
						}
					}
					if _, changed := body["allowed_objectives"]; changed {
						rb["allowed_objectives"] = []string{}
					}
				}
				// A model quota this call sets on a model the row did not
				// have is removed by a quota of 0 (the tenant surface
				// upserts per model; 0 deletes that model's row).
				mls, _ := rb["model_limits"].([]map[string]any)
				for _, ml := range in.ModelLimits {
					if !curModels[ml.Model] && ml.TokensPerMin > 0 {
						mls = append(mls, map[string]any{"model": ml.Model, "tokens_per_min": 0})
					}
				}
				rb["model_limits"] = mls
				proposed := overlay(curMap, body)
				if in.ModelLimits == nil {
					proposed["model_limits"] = curMap["model_limits"]
				}
				return changePlan{Current: current, Proposed: proposed, Rollback: rollback, Impact: "tenant"}, nil
			})
			if err != nil || stop {
				return nil, out, err
			}
		}
		var res any
		if err := c.Post(ctx, "/config/ai/tenant/ratelimit", body, &res); err != nil {
			d.auditMut("ai_ratelimit_set", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("ai_ratelimit_set", c.Name(), args, true, "")
		after, _, _ := c.TenantRateLimit(ctx, in.TenantID)
		return nil, executed(c.Name(), res, after), nil
	}
}

// ---- ai_user_ratelimit_set / ai_user_ratelimit_delete ----

type userRatelimitSetIn struct {
	Target       string         `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	TenantID     string         `json:"tenant_id" jsonschema:"tenant of the user"`
	UserID       string         `json:"user_id" jsonschema:"user identifier (the verified identity's subject)"`
	RPS          int            `json:"rps,omitempty" jsonschema:"max requests per second for the user (0 falls through to the defaults)"`
	BurstSize    int            `json:"burst_size,omitempty" jsonschema:"request burst size; 0 defaults to rps"`
	TokensPerMin int            `json:"tokens_per_min,omitempty" jsonschema:"max LLM tokens per minute for the user (0 falls through to the defaults)"`
	ModelLimits  []modelLimitIn `json:"model_limits,omitempty" jsonschema:"per-model token quotas for the user; REPLACED as a set (omitted clears them)"`
	Apply        bool           `json:"apply,omitempty" jsonschema:"operator convenience: apply without the preview step"`
	ConfirmToken string         `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

// userRatelimitKeys are the row's fields the preview and the rollback carry.
var userRatelimitKeys = []string{"tenant_id", "user_id", "rps", "burst_size", "tokens_per_min", "model_limits"}

// userRatelimitRollback rebuilds ai_user_ratelimit_set arguments from a row.
func userRatelimitRollback(row map[string]any) map[string]any {
	return map[string]any{
		"tenant_id": row["tenant_id"], "user_id": row["user_id"],
		"rps": intValue(row["rps"]), "burst_size": intValue(row["burst_size"]), "tokens_per_min": intValue(row["tokens_per_min"]),
		"model_limits": modelLimitsView(row["model_limits"]),
	}
}

func (d *Deps) aiUserRatelimitSet() sdk.ToolHandlerFor[userRatelimitSetIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in userRatelimitSetIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		if err := validatePathSegment(in.TenantID); err != nil {
			return nil, mutOut{}, fmt.Errorf("tenant_id: %w", err)
		}
		if err := validatePathSegment(in.UserID); err != nil {
			return nil, mutOut{}, fmt.Errorf("user_id: %w", err)
		}
		if in.RPS < 0 || in.BurstSize < 0 || in.TokensPerMin < 0 {
			return nil, mutOut{}, fmt.Errorf("rps, burst_size and tokens_per_min must be >= 0")
		}
		mls, err := modelLimitsBody(in.ModelLimits)
		if err != nil {
			return nil, mutOut{}, err
		}
		decides := in.RPS > 0 || in.TokensPerMin > 0
		for _, ml := range in.ModelLimits {
			if ml.TokensPerMin > 0 {
				decides = true
			}
		}
		if !decides {
			return nil, mutOut{}, fmt.Errorf("an entry whose limits are all zero constrains nothing and the API rejects it; nothing was sent. Set rps, tokens_per_min or a model quota above 0, or remove the user's limits with ai_user_ratelimit_delete")
		}
		body := map[string]any{
			"tenant_id": in.TenantID, "user_id": in.UserID,
			"rps": in.RPS, "burst_size": in.BurstSize, "tokens_per_min": in.TokensPerMin,
			"model_limits": mls,
		}
		args := in
		args.ConfirmToken = ""
		if !in.Apply || in.ConfirmToken != "" {
			out, stop, err := d.gateChange(ctx, "ai_user_ratelimit_set", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
				row, found, err := c.UserRateLimit(ctx, in.TenantID, in.UserID)
				if err != nil {
					return changePlan{}, fmt.Errorf("read of the user's rate limit failed, nothing was sent: %w", err)
				}
				var current any // nil when no row exists (not an empty object)
				rollback := &rollbackSpec{Tool: "ai_user_ratelimit_delete", Args: map[string]any{"tenant_id": in.TenantID, "user_id": in.UserID}}
				if found {
					cur := pick(row, userRatelimitKeys...)
					cur["model_limits"] = modelLimitsView(row["model_limits"])
					current = cur
					rollback = &rollbackSpec{Tool: "ai_user_ratelimit_set", Args: userRatelimitRollback(row)}
				}
				return changePlan{Current: current, Proposed: body, Rollback: rollback, Impact: "tenant"}, nil
			})
			if err != nil || stop {
				return nil, out, err
			}
		}
		var res any
		if err := c.Post(ctx, "/config/ai/user/ratelimit", body, &res); err != nil {
			d.auditMut("ai_user_ratelimit_set", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("ai_user_ratelimit_set", c.Name(), args, true, "")
		after, _, _ := c.UserRateLimit(ctx, in.TenantID, in.UserID)
		return nil, executed(c.Name(), res, after), nil
	}
}

type userRatelimitDeleteIn struct {
	Target       string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	TenantID     string `json:"tenant_id" jsonschema:"tenant of the user"`
	UserID       string `json:"user_id" jsonschema:"user whose explicit rate-limit row to remove"`
	ConfirmToken string `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

func (d *Deps) aiUserRatelimitDelete() sdk.ToolHandlerFor[userRatelimitDeleteIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in userRatelimitDeleteIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		if err := validatePathSegment(in.TenantID); err != nil {
			return nil, mutOut{}, fmt.Errorf("tenant_id: %w", err)
		}
		if err := validatePathSegment(in.UserID); err != nil {
			return nil, mutOut{}, fmt.Errorf("user_id: %w", err)
		}
		args := in
		args.ConfirmToken = ""
		out, stop, err := d.gateChange(ctx, "ai_user_ratelimit_delete", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
			row, found, err := c.UserRateLimit(ctx, in.TenantID, in.UserID)
			if err != nil {
				return changePlan{}, fmt.Errorf("read of the user's rate limit failed, nothing was sent: %w", err)
			}
			if !found {
				return changePlan{}, fmt.Errorf("no explicit rate-limit row for %s/%s: nothing to delete (the user already follows the defaults)", in.TenantID, in.UserID)
			}
			current := pick(row, userRatelimitKeys...)
			current["model_limits"] = modelLimitsView(row["model_limits"])
			return changePlan{
				Current:  current,
				Proposed: map[string]any{"exists": false},
				Rollback: &rollbackSpec{Tool: "ai_user_ratelimit_set", Args: userRatelimitRollback(row)},
				Impact:   "tenant",
			}, nil
		})
		if err != nil || stop {
			return nil, out, err
		}
		var res any
		if err := c.Delete(ctx, fmt.Sprintf("/config/ai/user/ratelimit/%s/%s", url.PathEscape(in.TenantID), url.PathEscape(in.UserID)), &res); err != nil {
			d.auditMut("ai_user_ratelimit_delete", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("ai_user_ratelimit_delete", c.Name(), args, true, "")
		readBack := map[string]any{"exists": false}
		if row, found, err := c.UserRateLimit(ctx, in.TenantID, in.UserID); err == nil && found {
			readBack = map[string]any{"exists": true, "user_ratelimit": row}
		}
		return nil, executed(c.Name(), res, readBack), nil
	}
}

// ---- ai_kv_inventory_get ----

type kvInventoryIn struct {
	Target    string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	ServiceID int64  `json:"service_id" jsonschema:"numeric AI service identifier"`
	EpIdx     int    `json:"ep_idx" jsonschema:"endpoint index within the service"`
	MaxBlocks int    `json:"max_blocks,omitempty" jsonschema:"cap on returned block hashes (default 50, max 500); total is always reported"`
}

type kvInventoryOut struct {
	Target    string `json:"target"`
	ServiceID int64  `json:"service_id"`
	EpIdx     int    `json:"ep_idx"`
	HashAlgo  string `json:"hash_algo,omitempty"`
	Admission string `json:"admission,omitempty" jsonschema:"TRT-LLM server-info admission verdict for this endpoint; absent when no verdict exists (ZMQ engines, or a gate awaiting its first answer); not by itself proof of KV-exact readiness"`
	Total     int    `json:"total"`
	Returned  int    `json:"returned"`
	Blocks    []any  `json:"blocks,omitempty"`
}

func (d *Deps) aiKvInventoryGet() sdk.ToolHandlerFor[kvInventoryIn, kvInventoryOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in kvInventoryIn) (*sdk.CallToolResult, kvInventoryOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, kvInventoryOut{}, err
		}
		if in.ServiceID < 0 || in.EpIdx < 0 {
			return nil, kvInventoryOut{}, fmt.Errorf("service_id and ep_idx must be >= 0")
		}
		limit := in.MaxBlocks
		if limit <= 0 {
			limit = 50
		}
		if limit > 500 {
			limit = 500
		}
		q := url.Values{
			"service_id": {strconv.FormatInt(in.ServiceID, 10)},
			"ep_idx":     {strconv.Itoa(in.EpIdx)},
		}
		var env struct {
			HashAlgo  string `json:"hash_algo"`
			Admission string `json:"admission"`
			Blocks    []any  `json:"blocks"`
			Total     int    `json:"total"`
		}
		if err := c.GetQ(ctx, "/config/ai/kv/inventory", q, &env); err != nil {
			d.audit("ai_kv_inventory_get", c.Name(), false, err.Error())
			return nil, kvInventoryOut{}, err
		}
		out := kvInventoryOut{
			Target:    c.Name(),
			ServiceID: in.ServiceID,
			EpIdx:     in.EpIdx,
			HashAlgo:  clean(env.HashAlgo),
			Admission: clean(env.Admission),
			Total:     env.Total,
		}
		if out.Total == 0 {
			out.Total = len(env.Blocks)
		}
		blocks := env.Blocks
		if len(blocks) > limit {
			blocks = blocks[:limit]
		}
		for _, b := range blocks {
			out.Blocks = append(out.Blocks, sanitizeAny(b, 0))
		}
		out.Returned = len(out.Blocks)
		d.audit("ai_kv_inventory_get", c.Name(), true, "")
		return nil, out, nil
	}
}

// ---- gpu_mode_set / gpu_conversations_cleanup ----

type gpuModeIn struct {
	Target       string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Enabled      bool   `json:"enabled" jsonschema:"true to enable GPU-aware routing, false to disable"`
	ConfirmToken string `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

func (d *Deps) gpuModeSet() sdk.ToolHandlerFor[gpuModeIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in gpuModeIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		path := "/config/gpu/disable"
		if in.Enabled {
			path = "/config/gpu/enable"
		}
		args := in
		args.ConfirmToken = ""
		out, stop, err := d.gateChange(ctx, "gpu_mode_set", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
			st, err := c.GPUStatus(ctx)
			if err != nil {
				return changePlan{}, fmt.Errorf("read of the GPU status failed, nothing was sent: %w", err)
			}
			current := pick(st, "enabled", "routing_mode")
			return changePlan{
				Current:  current,
				Proposed: map[string]any{"enabled": in.Enabled},
				Rollback: &rollbackSpec{Tool: "gpu_mode_set", Args: map[string]any{"enabled": boolValue(current["enabled"])}},
				Impact:   "global",
			}, nil
		})
		if err != nil || stop {
			return nil, out, err
		}
		var res any
		if err := c.Post(ctx, path, nil, &res); err != nil {
			d.auditMut("gpu_mode_set", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("gpu_mode_set", c.Name(), args, true, "")
		after, _ := c.GPUStatus(ctx)
		return nil, executed(c.Name(), res, pick(after, "enabled", "routing_mode")), nil
	}
}

type gpuCleanupIn struct {
	Target       string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	MaxAgeHours  int    `json:"max_age_hours,omitempty" jsonschema:"delete conversation mappings older than this (default 1)"`
	ConfirmToken string `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

func (d *Deps) gpuConversationsCleanup() sdk.ToolHandlerFor[gpuCleanupIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in gpuCleanupIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		path := "/config/gpu/conversations/cleanup"
		maxAge := 1
		if in.MaxAgeHours > 0 {
			maxAge = in.MaxAgeHours
			path += "?max_age_hours=" + strconv.Itoa(in.MaxAgeHours)
		}
		args := in
		args.ConfirmToken = ""
		out, stop, err := d.gateChange(ctx, "gpu_conversations_cleanup", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
			st, err := c.GPUStatus(ctx)
			if err != nil {
				return changePlan{}, fmt.Errorf("read of the GPU status failed, nothing was sent: %w", err)
			}
			current := pick(st, "enabled", "routing_mode", "worker_count")
			current["stale_conversation_mappings"] = "kept"
			proposed := overlay(current, map[string]any{"stale_conversation_mappings": fmt.Sprintf("removed when older than %d h", maxAge)})
			return changePlan{
				Current:  current,
				Proposed: proposed,
				Rollback: &rollbackSpec{Note: "not reversible: removed conversation-to-worker mappings are rebuilt only by new traffic"},
				Impact:   "global",
			}, nil
		})
		if err != nil || stop {
			return nil, out, err
		}
		var res any
		if err := c.Post(ctx, path, nil, &res); err != nil {
			d.auditMut("gpu_conversations_cleanup", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("gpu_conversations_cleanup", c.Name(), args, true, "")
		after, _ := c.GPUStatus(ctx)
		return nil, executed(c.Name(), res, pick(after, "enabled", "routing_mode", "worker_count")), nil
	}
}

// ---- shared simple mutators ----

// statusReader fetches the object a toggle or trigger acts on.
type statusReader func(ctx context.Context, c *client.Client) (map[string]any, error)

type enableFlagIn struct {
	Target       string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Enabled      bool   `json:"enabled" jsonschema:"true to enable, false to disable"`
	ConfirmToken string `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

// enableFlagTool POSTs {"enabled": bool} to a fixed path behind the change
// gate: the preview shows the flag as read now against the proposed value.
func (d *Deps) enableFlagTool(tool, path string, read statusReader, impact string) sdk.ToolHandlerFor[enableFlagIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in enableFlagIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		args := in
		args.ConfirmToken = ""
		out, stop, err := d.gateChange(ctx, tool, c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
			st, err := read(ctx, c)
			if err != nil {
				return changePlan{}, fmt.Errorf("read of the current state failed, nothing was sent: %w", err)
			}
			current := pick(st, "enabled")
			return changePlan{
				Current:  current,
				Proposed: map[string]any{"enabled": in.Enabled},
				Rollback: &rollbackSpec{Tool: tool, Args: map[string]any{"enabled": boolValue(current["enabled"])}},
				Impact:   impact,
			}, nil
		})
		if err != nil || stop {
			return nil, out, err
		}
		var res any
		if err := c.Post(ctx, path, map[string]any{"enabled": in.Enabled}, &res); err != nil {
			d.auditMut(tool, c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut(tool, c.Name(), args, true, "")
		after, _ := read(ctx, c)
		return nil, executed(c.Name(), res, pick(after, "enabled")), nil
	}
}

type postSimpleIn struct {
	Target       string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Apply        bool   `json:"apply,omitempty" jsonschema:"operator convenience: run the trigger without the preview step (the tool changes no configuration)"`
	ConfirmToken string `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

// postSimple POSTs an empty body to a fixed path (trigger-style endpoints)
// behind the change gate. The preview shows the object the trigger acts on;
// at operator tier apply=true runs it without the token (§3.6 item 1).
func (d *Deps) postSimple(tool, path string, read statusReader, keys ...string) sdk.ToolHandlerFor[postSimpleIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in postSimpleIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		args := in
		args.ConfirmToken = ""
		if !in.Apply || in.ConfirmToken != "" {
			out, stop, err := d.gateChange(ctx, tool, c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
				st, err := read(ctx, c)
				if err != nil {
					return changePlan{}, fmt.Errorf("read of the current state failed, nothing was sent: %w", err)
				}
				current := pick(st, keys...)
				proposed := overlay(current, map[string]any{"last_health_check": "re-run on apply"})
				return changePlan{
					Current:  current,
					Proposed: proposed,
					Rollback: &rollbackSpec{Note: "a probe changes no configuration; there is nothing to roll back"},
					Impact:   "none",
				}, nil
			})
			if err != nil || stop {
				return nil, out, err
			}
		}
		var res any
		if err := c.Post(ctx, path, nil, &res); err != nil {
			d.auditMut(tool, c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut(tool, c.Name(), args, true, "")
		after, _ := read(ctx, c)
		return nil, executed(c.Name(), res, pick(after, keys...)), nil
	}
}

// ---- llamafw_configure / llamafw_scanners_set ----

type llamafwConfigureIn struct {
	Target             string   `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	ServerURL          string   `json:"server_url,omitempty" jsonschema:"LlamaFirewall gRPC server URL, e.g. localhost:50052"`
	TimeoutSec         int      `json:"timeout_sec,omitempty" jsonschema:"request timeout seconds (1-300)"`
	FailClosed         *bool    `json:"fail_closed,omitempty" jsonschema:"true blocks on scanner error, false allows"`
	BlockThreshold     *float64 `json:"block_threshold,omitempty" jsonschema:"minimum confidence score to block (0.0-1.0)"`
	CacheEnabled       *bool    `json:"cache_enabled,omitempty" jsonschema:"cache identical scan results"`
	CacheTTLSec        int      `json:"cache_ttl_sec,omitempty" jsonschema:"cache TTL seconds"`
	ConnectionPoolSize int      `json:"connection_pool_size,omitempty" jsonschema:"reusable gRPC connections (1-100)"`
	ScanPatterns       []string `json:"scan_patterns,omitempty" jsonschema:"URL patterns to scan (empty = all)"`
	SkipPatterns       []string `json:"skip_patterns,omitempty" jsonschema:"URL patterns to skip"`
	ConfirmToken       string   `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

// llamafwStatusKeys are the configuration fields GET /config/llamafirewall/status
// exposes; timeout_sec and connection_pool_size are write-only there.
var llamafwStatusKeys = []string{"server_url", "fail_closed", "block_threshold", "cache_enabled", "cache_ttl_sec", "scan_patterns", "skip_patterns"}

func (d *Deps) llamafwConfigure() sdk.ToolHandlerFor[llamafwConfigureIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in llamafwConfigureIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		body := map[string]any{}
		if in.ServerURL != "" {
			body["server_url"] = in.ServerURL
		}
		if in.TimeoutSec > 0 {
			body["timeout_sec"] = in.TimeoutSec
		}
		if in.FailClosed != nil {
			body["fail_closed"] = *in.FailClosed
		}
		if in.BlockThreshold != nil {
			if *in.BlockThreshold < 0 || *in.BlockThreshold > 1 {
				return nil, mutOut{}, fmt.Errorf("block_threshold: %g out of range 0.0-1.0", *in.BlockThreshold)
			}
			body["block_threshold"] = *in.BlockThreshold
		}
		if in.CacheEnabled != nil {
			body["cache_enabled"] = *in.CacheEnabled
		}
		if in.CacheTTLSec > 0 {
			body["cache_ttl_sec"] = in.CacheTTLSec
		}
		if in.ConnectionPoolSize > 0 {
			body["connection_pool_size"] = in.ConnectionPoolSize
		}
		if in.ScanPatterns != nil {
			body["scan_patterns"] = in.ScanPatterns
		}
		if in.SkipPatterns != nil {
			body["skip_patterns"] = in.SkipPatterns
		}
		if len(body) == 0 {
			return nil, mutOut{}, fmt.Errorf("no fields to set")
		}
		args := in
		args.ConfirmToken = ""
		out, stop, err := d.gateChange(ctx, "llamafw_configure", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
			st, err := c.LlamaFirewallStatus(ctx)
			if err != nil {
				return changePlan{}, fmt.Errorf("read of the LlamaFirewall status failed, nothing was sent: %w", err)
			}
			current := pick(st, llamafwStatusKeys...)
			rb := &rollbackSpec{Tool: "llamafw_configure", Args: rollbackArgs(current, body)}
			if _, ok := body["timeout_sec"]; ok {
				rb.Note = "timeout_sec and connection_pool_size are not readable from the status; restore them by hand"
			} else if _, ok := body["connection_pool_size"]; ok {
				rb.Note = "timeout_sec and connection_pool_size are not readable from the status; restore them by hand"
			}
			return changePlan{Current: current, Proposed: overlay(current, body), Rollback: rb, Impact: "global"}, nil
		})
		if err != nil || stop {
			return nil, out, err
		}
		var res any
		if err := c.Post(ctx, "/config/llamafirewall/configure", body, &res); err != nil {
			d.auditMut("llamafw_configure", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("llamafw_configure", c.Name(), args, true, "")
		after, _ := c.LlamaFirewallStatus(ctx)
		return nil, executed(c.Name(), res, pick(after, llamafwStatusKeys...)), nil
	}
}

// rollbackArgs returns, for every key the patch sets, the value current
// holds for it (the arguments that undo the patch when names coincide).
func rollbackArgs(current, patch map[string]any) map[string]any {
	out := map[string]any{}
	for k := range patch {
		if v, ok := current[k]; ok {
			out[k] = v
		}
	}
	return out
}

type llamafwScannersIn struct {
	Target         string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	PromptGuard    *bool  `json:"prompt_guard,omitempty" jsonschema:"ML-based prompt injection detection"`
	CodeShield     *bool  `json:"code_shield,omitempty" jsonschema:"insecure code pattern detection"`
	Regex          *bool  `json:"regex,omitempty" jsonschema:"credential/API key leak detection"`
	HiddenASCII    *bool  `json:"hidden_ascii,omitempty" jsonschema:"zero-width/invisible character detection"`
	AgentAlignment *bool  `json:"agent_alignment,omitempty" jsonschema:"AI agent misalignment detection"`
	PIIDetection   *bool  `json:"pii_detection,omitempty" jsonschema:"PII detection (complementary to Presidio)"`
	ConfirmToken   string `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

func (d *Deps) llamafwScannersSet() sdk.ToolHandlerFor[llamafwScannersIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in llamafwScannersIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		body := map[string]any{}
		set := func(k string, v *bool) {
			if v != nil {
				body[k] = *v
			}
		}
		set("prompt_guard", in.PromptGuard)
		set("code_shield", in.CodeShield)
		set("regex", in.Regex)
		set("hidden_ascii", in.HiddenASCII)
		set("agent_alignment", in.AgentAlignment)
		set("pii_detection", in.PIIDetection)
		if len(body) == 0 {
			return nil, mutOut{}, fmt.Errorf("no scanners to set")
		}
		args := in
		args.ConfirmToken = ""
		out, stop, err := d.gateChange(ctx, "llamafw_scanners_set", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
			st, err := c.LlamaFirewallStatus(ctx)
			if err != nil {
				return changePlan{}, fmt.Errorf("read of the LlamaFirewall status failed, nothing was sent: %w", err)
			}
			current := mapValue(st["scanners"])
			return changePlan{
				Current:  current,
				Proposed: overlay(current, body),
				Rollback: &rollbackSpec{Tool: "llamafw_scanners_set", Args: rollbackArgs(current, body)},
				Impact:   "global",
			}, nil
		})
		if err != nil || stop {
			return nil, out, err
		}
		var res any
		if err := c.Post(ctx, "/config/llamafirewall/scanners", body, &res); err != nil {
			d.auditMut("llamafw_scanners_set", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("llamafw_scanners_set", c.Name(), args, true, "")
		after, _ := c.LlamaFirewallStatus(ctx)
		return nil, executed(c.Name(), res, mapValue(after["scanners"])), nil
	}
}

// ---- pii_configure / pii_url_patterns_set ----

type piiConfigureIn struct {
	Target         string   `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Mode           string   `json:"mode,omitempty" jsonschema:"detect|mask|redact|anonymize"`
	Direction      string   `json:"direction,omitempty" jsonschema:"both|request|response"`
	FailMode       string   `json:"fail_mode,omitempty" jsonschema:"open|closed (behavior when Presidio is unreachable)"`
	ScanMode       string   `json:"scan_mode,omitempty" jsonschema:"full|truncate (large body handling)"`
	AnalyzerURL    string   `json:"analyzer_url,omitempty" jsonschema:"Presidio analyzer gRPC endpoint"`
	AnonymizerURL  string   `json:"anonymizer_url,omitempty" jsonschema:"Presidio anonymizer gRPC endpoint"`
	ScoreThreshold *float64 `json:"score_threshold,omitempty" jsonschema:"minimum PII confidence score (0.0-1.0)"`
	TimeoutMs      int      `json:"timeout_ms,omitempty" jsonschema:"Presidio request timeout milliseconds"`
	ConfirmToken   string   `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

// piiConfigKeys are the configuration fields GET /config/pii/status exposes
// that pii_configure can set.
var piiConfigKeys = []string{"mode", "direction", "fail_mode", "scan_mode", "analyzer_url", "anonymizer_url", "score_threshold", "timeout_ms"}

func (d *Deps) piiConfigure() sdk.ToolHandlerFor[piiConfigureIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in piiConfigureIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		oneOf := func(field, v string, allowed ...string) error {
			if v == "" {
				return nil
			}
			for _, a := range allowed {
				if strings.EqualFold(v, a) {
					return nil
				}
			}
			return fmt.Errorf("%s: %q (want %s)", field, v, strings.Join(allowed, "|"))
		}
		if err := oneOf("mode", in.Mode, "detect", "mask", "redact", "anonymize"); err != nil {
			return nil, mutOut{}, err
		}
		if err := oneOf("direction", in.Direction, "both", "request", "response"); err != nil {
			return nil, mutOut{}, err
		}
		if err := oneOf("fail_mode", in.FailMode, "open", "closed"); err != nil {
			return nil, mutOut{}, err
		}
		if err := oneOf("scan_mode", in.ScanMode, "full", "truncate"); err != nil {
			return nil, mutOut{}, err
		}
		body := map[string]any{}
		setS := func(k, v string) {
			if v != "" {
				body[k] = strings.ToLower(v)
			}
		}
		setS("mode", in.Mode)
		setS("direction", in.Direction)
		setS("fail_mode", in.FailMode)
		setS("scan_mode", in.ScanMode)
		if in.AnalyzerURL != "" {
			body["analyzer_url"] = in.AnalyzerURL
		}
		if in.AnonymizerURL != "" {
			body["anonymizer_url"] = in.AnonymizerURL
		}
		if in.ScoreThreshold != nil {
			if *in.ScoreThreshold < 0 || *in.ScoreThreshold > 1 {
				return nil, mutOut{}, fmt.Errorf("score_threshold: %g out of range 0.0-1.0", *in.ScoreThreshold)
			}
			body["score_threshold"] = *in.ScoreThreshold
		}
		if in.TimeoutMs > 0 {
			body["timeout_ms"] = in.TimeoutMs
		}
		if len(body) == 0 {
			return nil, mutOut{}, fmt.Errorf("no fields to set")
		}
		args := in
		args.ConfirmToken = ""
		out, stop, err := d.gateChange(ctx, "pii_configure", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
			st, err := c.PIIStatus(ctx)
			if err != nil {
				return changePlan{}, fmt.Errorf("read of the PII status failed, nothing was sent: %w", err)
			}
			current := pick(st, piiConfigKeys...)
			return changePlan{
				Current:  current,
				Proposed: overlay(current, body),
				Rollback: &rollbackSpec{Tool: "pii_configure", Args: rollbackArgs(current, body)},
				Impact:   "global",
			}, nil
		})
		if err != nil || stop {
			return nil, out, err
		}
		var res any
		if err := c.Post(ctx, "/config/pii/configure", body, &res); err != nil {
			d.auditMut("pii_configure", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("pii_configure", c.Name(), args, true, "")
		after, _ := c.PIIStatus(ctx)
		return nil, executed(c.Name(), res, pick(after, piiConfigKeys...)), nil
	}
}

type piiURLPatternIn struct {
	Pattern string `json:"pattern" jsonschema:"URL pattern (e.g. /api/v1/chat*)"`
	Exclude bool   `json:"exclude,omitempty" jsonschema:"true to exclude matching URLs from scanning"`
}

type piiURLPatternsIn struct {
	Target       string            `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
	Mode         string            `json:"mode" jsonschema:"add|replace|clear"`
	Patterns     []piiURLPatternIn `json:"patterns,omitempty" jsonschema:"URL patterns (max 64; not needed for mode=clear)"`
	ConfirmToken string            `json:"confirm_token,omitempty" jsonschema:"single-use token from the preview step; omit to preview"`
}

func (d *Deps) piiURLPatternsSet() sdk.ToolHandlerFor[piiURLPatternsIn, mutOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in piiURLPatternsIn) (*sdk.CallToolResult, mutOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, mutOut{}, err
		}
		mode := strings.ToLower(in.Mode)
		switch mode {
		case "add", "replace", "clear":
		default:
			return nil, mutOut{}, fmt.Errorf("mode: %q (want add|replace|clear)", in.Mode)
		}
		if mode != "clear" && len(in.Patterns) == 0 {
			return nil, mutOut{}, fmt.Errorf("patterns: at least one pattern is required for mode %s", mode)
		}
		if len(in.Patterns) > 64 {
			return nil, mutOut{}, fmt.Errorf("patterns: %d exceeds the maximum of 64", len(in.Patterns))
		}
		body := map[string]any{"mode": mode}
		var pats []map[string]any
		if len(in.Patterns) > 0 {
			pats = make([]map[string]any, 0, len(in.Patterns))
			for i, p := range in.Patterns {
				if p.Pattern == "" {
					return nil, mutOut{}, fmt.Errorf("patterns[%d].pattern is required", i)
				}
				pat := map[string]any{"pattern": p.Pattern}
				if p.Exclude {
					pat["exclude"] = true
				}
				pats = append(pats, pat)
			}
			body["patterns"] = pats
		}
		args := in
		args.ConfirmToken = ""
		out, stop, err := d.gateChange(ctx, "pii_url_patterns_set", c.Name(), in.ConfirmToken, args, func() (changePlan, error) {
			st, err := c.PIIStatus(ctx)
			if err != nil {
				return changePlan{}, fmt.Errorf("read of the PII status failed, nothing was sent: %w", err)
			}
			cur := listValue(st["url_patterns"])
			var proposed []any
			switch mode {
			case "add":
				proposed = append(append([]any{}, cur...), anySlice(pats)...)
			case "replace":
				proposed = anySlice(pats)
			case "clear":
				proposed = []any{}
			}
			rb := &rollbackSpec{Tool: "pii_url_patterns_set", Args: map[string]any{"mode": "clear"}}
			if len(cur) > 0 {
				var rbPats []map[string]any
				for _, p := range cur {
					pm := mapValue(p)
					rp := map[string]any{"pattern": pm["pattern"]}
					if boolValue(pm["is_exclude"]) || boolValue(pm["exclude"]) {
						rp["exclude"] = true
					}
					rbPats = append(rbPats, rp)
				}
				rb.Args = map[string]any{"mode": "replace", "patterns": rbPats}
			}
			return changePlan{
				Current:  map[string]any{"url_patterns": cur},
				Proposed: map[string]any{"url_patterns": proposed},
				Rollback: rb,
				Impact:   "global",
			}, nil
		})
		if err != nil || stop {
			return nil, out, err
		}
		var res any
		if err := c.Post(ctx, "/config/pii/url-patterns", body, &res); err != nil {
			d.auditMut("pii_url_patterns_set", c.Name(), args, false, err.Error())
			return nil, mutOut{}, err
		}
		d.auditMut("pii_url_patterns_set", c.Name(), args, true, "")
		after, _ := c.PIIStatus(ctx)
		return nil, executed(c.Name(), res, pick(after, "url_patterns", "url_pattern_count")), nil
	}
}

// anySlice widens a slice of objects for list operations.
func anySlice(in []map[string]any) []any {
	out := make([]any, 0, len(in))
	for _, m := range in {
		out = append(out, m)
	}
	return out
}

// ---- ai_traffic_report ----

type trafficReportIn struct {
	Target string `json:"target,omitempty" jsonschema:"target loxilb instance name; omit for default"`
}

type latencySummary struct {
	Count float64 `json:"count"`
	SumS  float64 `json:"sum_seconds"`
	P50S  float64 `json:"p50_seconds"`
	P95S  float64 `json:"p95_seconds"`
	P99S  float64 `json:"p99_seconds"`
}

type trafficReportOut struct {
	Target string `json:"target"`
	// Requests: cumulative loxilb_ai_requests_total broken down by
	// model/tenant/status/outcome/cause, plus the derived error ratio.
	//
	// RequestsTotal is offered load, so it includes the requests the policy
	// gate refused. ErrorRatio deliberately does NOT: it is (completed 5xx +
	// aborted by the engine or the gateway) over completed + aborted, because a
	// denial is non-2xx by construction and folding denials in would make a
	// rate-limit storm read as a backend fault -- to an operator, and to an
	// agent acting on this report. RequestsDenied carries that half
	// separately. An aborted request is not completed: RequestsAborted counts
	// it, RequestsAbortedErrors the engine and gateway causes among them; a
	// client cause (a cancel) is in the denominator only. RequestsNon2xx stays
	// non-2xx over completed traffic, for gateways read before the aborted
	// outcome existed.
	RequestsByModelTenantStatus []map[string]any `json:"requests_by_model_tenant_status,omitempty"`
	RequestsTotal               float64          `json:"requests_total"`
	RequestsCompleted           float64          `json:"requests_completed"`
	RequestsAborted             float64          `json:"requests_aborted"`
	RequestsAbortedErrors       float64          `json:"requests_aborted_errors"`
	RequestsDenied              float64          `json:"requests_denied"`
	RequestsNon2xx              float64          `json:"requests_non_2xx"`
	Requests5xx                 float64          `json:"requests_5xx"`
	ErrorRatio                  float64          `json:"error_ratio"`
	RateLimitDrops              []map[string]any `json:"rate_limit_drops,omitempty"`
	RateLimitDropsTotal         float64          `json:"rate_limit_drops_total"`
	ModelNotAllowed             []map[string]any `json:"model_not_allowed,omitempty"`
	ActiveStreams               []map[string]any `json:"active_streams,omitempty"`
	RequestDuration             *latencySummary  `json:"request_duration,omitempty"`
	ProxyTTFB                   *latencySummary  `json:"proxy_ttfb,omitempty"`
	PDDecodeTTFT                *latencySummary  `json:"pd_decode_ttft,omitempty"`
	PDDecodeDuration            *latencySummary  `json:"pd_decode_duration,omitempty"`
	PDPrefillFallback           []map[string]any `json:"pd_prefill_fallback,omitempty"`
	KVParamsFound               float64          `json:"kv_params_found"`
	KVParamsMissing             float64          `json:"kv_params_missing"`
	PDSessionHits               float64          `json:"pd_session_hits"`
	NormalSessionHits           float64          `json:"normal_session_hits"`
	Caveats                     []string         `json:"caveats"`
}

func (d *Deps) aiTrafficReport() sdk.ToolHandlerFor[trafficReportIn, trafficReportOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in trafficReportIn) (*sdk.CallToolResult, trafficReportOut, error) {
		c, err := d.Resolve(in.Target)
		if err != nil {
			return nil, trafficReportOut{}, err
		}
		text, err := c.MetricsText(ctx)
		if err != nil {
			d.audit("ai_traffic_report", c.Name(), false, err.Error())
			return nil, trafficReportOut{}, err
		}
		fams, err := client.ParseFamilies(text, []string{
			"loxilb_ai_*", "loxilb_proxy_http_ttfb_seconds",
		}, 2000)
		if err != nil {
			return nil, trafficReportOut{}, err
		}
		byName := map[string]client.Family{}
		for _, f := range fams {
			byName[f.Name] = f
		}

		out := trafficReportOut{
			Target: c.Name(),
			Caveats: []string{
				trafficReportCaveat,
				"all values are cumulative since loxilb start; for rates use promql_query with rate()",
			},
		}
		if f, ok := byName["loxilb_ai_requests_total"]; ok {
			for _, s := range f.Samples {
				out.RequestsTotal += s.Value
				status := s.Labels["status"]
				// A gateway old enough to predate the outcome label emitted
				// this family for backend-answered traffic only, so an absent
				// label means completed. Defaulting the other way would move
				// every old build's whole series out of the error ratio and
				// report 0 for a gateway that is failing.
				outcome := s.Labels["outcome"]
				if outcome == "" {
					outcome = outcomeCompleted
				}
				cause := s.Labels["cause"]
				switch outcome {
				case outcomeDenied:
					out.RequestsDenied += s.Value
				case outcomeAborted:
					out.RequestsAborted += s.Value
					if cause == causeEngine || cause == causeGateway {
						out.RequestsAbortedErrors += s.Value
					}
				default:
					out.RequestsCompleted += s.Value
					if !strings.HasPrefix(status, "2") {
						out.RequestsNon2xx += s.Value
					}
					if strings.HasPrefix(status, "5") {
						out.Requests5xx += s.Value
					}
				}
				row := map[string]any{
					"model": clean(s.Labels["model"]), "tenant": clean(s.Labels["tenant"]),
					"status": clean(status), "outcome": clean(outcome), "count": s.Value,
				}
				if cause != "" {
					row["cause"] = clean(cause)
				}
				out.RequestsByModelTenantStatus = append(out.RequestsByModelTenantStatus, row)
			}
			// Over completed + aborted traffic only -- see the field comments above.
			if served := out.RequestsCompleted + out.RequestsAborted; served > 0 {
				out.ErrorRatio = (out.Requests5xx + out.RequestsAbortedErrors) / served
			}
		}
		if f, ok := byName["loxilb_ai_rate_limit_hits_total"]; ok {
			for _, s := range f.Samples {
				out.RateLimitDropsTotal += s.Value
				out.RateLimitDrops = append(out.RateLimitDrops, map[string]any{
					"tenant": clean(s.Labels["tenant"]), "reason": clean(s.Labels["reason"]),
					"count": s.Value,
				})
			}
		}
		if f, ok := byName["loxilb_ai_model_not_allowed_total"]; ok {
			for _, s := range f.Samples {
				out.ModelNotAllowed = append(out.ModelNotAllowed, map[string]any{
					"model": clean(s.Labels["model"]), "tenant": clean(s.Labels["tenant"]),
					"count": s.Value,
				})
			}
		}
		if f, ok := byName["loxilb_ai_active_streams"]; ok {
			for _, s := range f.Samples {
				out.ActiveStreams = append(out.ActiveStreams, map[string]any{
					"model": clean(s.Labels["model"]), "streams": s.Value,
				})
			}
		}
		out.RequestDuration = summarizeHistogram(byName["loxilb_ai_request_duration_seconds"])
		out.ProxyTTFB = summarizeHistogram(byName["loxilb_proxy_http_ttfb_seconds"])
		// TTFT is dispatch to the first streamed event (streamed requests
		// only); the whole decode leg is the duration family.
		out.PDDecodeTTFT = summarizeHistogram(byName["loxilb_ai_pd_decode_ttft_seconds"])
		out.PDDecodeDuration = summarizeHistogram(byName["loxilb_ai_pd_decode_duration_seconds"])
		if f, ok := byName["loxilb_ai_pd_prefill_fallback_total"]; ok {
			for _, s := range f.Samples {
				out.PDPrefillFallback = append(out.PDPrefillFallback, map[string]any{
					"model": clean(s.Labels["model"]), "reason": clean(s.Labels["reason"]),
					"count": s.Value,
				})
			}
		}
		out.KVParamsFound = sumFamily(byName["loxilb_ai_pd_kv_params_found_total"])
		out.KVParamsMissing = sumFamily(byName["loxilb_ai_pd_kv_params_missing_total"])
		out.PDSessionHits = sumFamily(byName["loxilb_ai_pd_session_hits_total"])
		out.NormalSessionHits = sumFamily(byName["loxilb_ai_normal_session_hits_total"])

		d.audit("ai_traffic_report", c.Name(), true, "")
		return nil, out, nil
	}
}

func sumFamily(f client.Family) float64 {
	var total float64
	for _, s := range f.Samples {
		total += s.Value
	}
	return total
}

// summarizeHistogram aggregates a parsed histogram family (possibly many
// label sets) into overall count/sum and estimated p50/p95/p99 via linear
// interpolation over the merged cumulative buckets. Returns nil when the
// family is absent or empty.
func summarizeHistogram(f client.Family) *latencySummary {
	if len(f.Samples) == 0 {
		return nil
	}
	buckets := map[float64]float64{} // le -> cumulative count (summed over label sets)
	var count, sum float64
	for _, s := range f.Samples {
		if le, ok := s.Labels["le"]; ok {
			bound, err := strconv.ParseFloat(le, 64) // handles "+Inf"
			if err != nil {
				continue
			}
			buckets[bound] += s.Value
			continue
		}
		switch s.Labels["series"] {
		case "count":
			count += s.Value
		case "sum":
			sum += s.Value
		}
	}
	if count == 0 || len(buckets) == 0 {
		return nil
	}
	bounds := make([]float64, 0, len(buckets))
	for b := range buckets {
		bounds = append(bounds, b)
	}
	sort.Float64s(bounds)
	quantile := func(q float64) float64 {
		target := q * count
		prevBound, prevCum := 0.0, 0.0
		for _, b := range bounds {
			cum := buckets[b]
			if cum >= target {
				if math.IsInf(b, 1) { // +Inf bucket: no upper bound information
					return prevBound
				}
				if cum == prevCum {
					return b
				}
				return prevBound + (b-prevBound)*(target-prevCum)/(cum-prevCum)
			}
			prevBound, prevCum = b, cum
		}
		return prevBound
	}
	return &latencySummary{
		Count: count,
		SumS:  sum,
		P50S:  quantile(0.50),
		P95S:  quantile(0.95),
		P99S:  quantile(0.99),
	}
}
