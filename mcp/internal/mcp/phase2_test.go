/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 *
 * Gate tests: role visibility of management CRUD tools,
 * the confirm-token preview/execute flow end-to-end (T4), config_export
 * secret masking (T5), and filename traversal rejection (T10).
 */

package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

// mockLoxilb is a minimal loxilb REST stand-in that records mutating requests.
type mockLoxilb struct {
	mu       sync.Mutex
	requests []string // "METHOD path"
	bodies   []map[string]any
	srv      *httptest.Server
	// metricsText overrides the exposition the AI mock serves. Empty means
	// aiMetricsText -- the default fixture, which carries no outcome label and
	// therefore stands in for a gateway older than it.
	metricsText string
	// securityRateJSON overrides the one-element securityrateAttr entry GET
	// /config/securityrate/all serves. Empty means the default fixture: SYN
	// protection on, the other two off (their flags omitted, as the gateway
	// omits every zero-valued field).
	securityRateJSON string
}

// mockRuleJSON is the one rule the mock gateway holds (1.2.3.4:80/tcp, a
// typed AI rule), served by GET /all and by the composite-key GET.
const mockRuleJSON = `{"serviceArguments":{"externalIP":"1.2.3.4","port":80,"protocol":"tcp","name":"web","mode":4,"host":"models.example","path_prefix":"/v1","model_name":"llama3","sse_mode":true,"pd_disagg_mode":true,"pd_cache_aware_mode":true,"api_key_auth":"required","kvExactMode":1,"kvEngineType":"vllm","id":"rule-7","adminStateUp":false,"security":1,"jwt_auth_profile":"corp-oidc","eppEndpoint":"10.0.0.9:9002","kvExactApiMode":"chat","kvModelProfile":"qwen3-8b","fc_effective":{"mode":"enforce","max_outstanding":64},"backend_tls_effective":{"enabled":false},"half_close_effective":"relay"},"endpoints":[{"endpointIP":"10.0.0.1","targetPort":8000,"weight":1,"state":"active","ep_role":1,"nixl_port":9001}]}`

func newMockLoxilb(t *testing.T) *mockLoxilb {
	t.Helper()
	m := &mockLoxilb{}
	mux := http.NewServeMux()
	mux.HandleFunc("/netlox/v1/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.requests = append(m.requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				m.bodies = append(m.bodies, body)
			}
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/netlox/v1/config/loadbalancer/all":
			w.Write([]byte(`{"lbAttr":[` + mockRuleJSON + `]}`))
		case "/netlox/v1/config/loadbalancer/externalipaddress/1.2.3.4/port/80/protocol/tcp":
			w.Write([]byte(mockRuleJSON)) // the composite-key pre-read of lb_create
		case "/netlox/v1/config/securityrate/all":
			// Zero-valued fields (connRateEnabled, udpEnabled, udpBlocked) are omitted, as the gateway does.
			entry := m.securityRateJSON
			if entry == "" {
				entry = `{"synEnabled":true,"synThreshold":1000,"cookieThreshold":500,"ratePerSec":500,"udpPktThreshold":3000,"udpBandwidthMB":10,"whitelistIps":null,"synBlocked":7,"connBlocked":3}`
			}
			w.Write([]byte(`{"securityrateAttr":[` + entry + `]}`))
		case "/netlox/v1/config/loadbalancer/id/rule-7":
			w.Write([]byte(mockRuleJSON)) // the opaque-id read of lb_get
		case "/netlox/v1/config/loadbalancer/externalipaddress/1.2.3.4/port/80/protocol/tcp/status":
			w.Write([]byte(`{"adminStateUp":true,"operatingStatus":"ONLINE","lastUpdated":"2026-10-28T10:00:00Z"}`))
		case "/netlox/v1/config/loadbalancer/externalipaddress/1.2.3.4/port/80/protocol/tcp/stats":
			w.Write([]byte(`{"activeConnections":3,"bytesIn":1024,"bytesOut":4096,"totalConnections":42}`))
		case "/netlox/v1/config/loadbalancer/externalipaddress/1.2.3.4/port/80/protocol/tcp/kvexactstatus":
			w.Write([]byte(`{"kvExactStatusAttr":[{"ruleIdentity":"1.2.3.4:80/tcp#llama3","modelName":"llama3","engineFamily":"vllm","apiMode":"chat","modelProfileId":"qwen3-8b","modelProfileGen":4,"bindingGen":7,"bindingDigest":"sha256:abc","hashContractId":"hc-1","desiredState":"enforced","enforcedState":"enforced","reasonCodes":[],"enforcement":{"desired":"enforced","enforced":"enforced","lastAckAt":"2026-10-28T10:00:00Z","fault":"","goFenced":false}}]}`))
		case "/netlox/v1/config/ai/objective":
			w.Write([]byte(`{"objectiveAttr":[{"name":"latency","services":["1.2.3.4:80/tcp"],"priority":10,"updated_at":"2026-10-28T09:00:00Z"}],"source":"store","source_timestamp":"2026-10-28T09:30:00Z","store_reachable":true,"store_last_read":"2026-10-28T09:30:00Z"}`))
		case "/netlox/v1/config/ai/netidentity":
			w.Write([]byte(`{"netIdentityAttr":[{"name":"lab","match":{"cidr":"10.0.0.0/24"},"services":["1.2.3.4:80/tcp"],"tenant":"lab","allowed_models":["llama3"],"enforce":"tenant","credential":"none","forward_identity":false,"priority_class":"standard","fail_mode":"closed"}]}`))
		case "/netlox/v1/config/ai/jwtauthprofile":
			w.Write([]byte(`{"jwtAuthProfileAttr":[{"name":"corp-oidc","issuer":"https://issuer.example","jwks_url":"https://issuer.example/jwks.json","audiences":["gw"],"algs":["RS256"],"leeway_sec":30,"refresh_sec":300,"tenant_claim":"tid","user_claim":"sub","token":"should-not-matter"}]}`))
		case "/netlox/v1/config/ai/user/ratelimit/lab":
			w.Write([]byte(`[{"tenant_id":"lab","user_id":"alice","rps":5,"burst_size":10,"tokens_per_min":1000,"updated_at":"2026-10-28T09:00:00Z"}]`))
		case "/netlox/v1/config/ai/user/ratelimit/lab/alice":
			w.Write([]byte(`{"tenant_id":"lab","user_id":"alice","rps":5,"burst_size":10,"tokens_per_min":1000,"model_limits":[{"model":"llama3","tokens_per_min":500}],"updated_at":"2026-10-28T09:00:00Z"}`))
		case "/netlox/v1/config/ai/ratelimit/defaults/global":
			w.Write([]byte(`{"scope":"global","default_user_rps":2,"default_user_tpm":500,"default_tenant_rps":20,"default_tenant_tpm":5000,"vip_shared_rps":0,"vip_shared_tpm":0,"updated_at":"2026-10-28T09:00:00Z"}`))
		case "/netlox/v1/config/ai/ratelimit/defaults/rule":
			w.Write([]byte(`{"scope":"rule","rule_ident":"` + r.URL.Query().Get("rule_ident") + `","default_user_rps":1,"updated_at":"2026-10-28T09:00:00Z"}`))
		case "/netlox/v1/config/ai/model-profiles":
			w.Write([]byte(`{"registryGeneration":4,"setDigest":"sha256:set","profiles":[{"profileId":"qwen3-8b","gen":4,"baseModel":"Qwen/Qwen3-8B","aliasPolicy":"exact","allowedAliases":[],"supportedApis":["chat"],"supportedFeatures":["tools"],"excludedFeatures":[],"tokenizerRevision":"main","tokenizerSha256":"sha256:tok","templateSha256":"sha256:tpl","templateContentFormat":"jinja","rendererEngine":"loxilb","rendererVersion":"1","oracleEngine":"vllm","oracleVersion":"0.31.0"}]}`))
		case "/netlox/v1/config/ai/model-profiles/qwen3-8b":
			w.Write([]byte(`{"profileId":"qwen3-8b","gen":4,"baseModel":"Qwen/Qwen3-8B","supportedApis":["chat"]}`))
		case "/netlox/v1/status/capabilities":
			w.Write([]byte(`{"capabilities":[{"name":"kv_exact","ready":true,"reason_code":"","reason":""},{"name":"pii","ready":false,"reason_code":"analyzer_unreachable","reason":"analyzer not configured","limit":0,"in_use":0}]}`))
		case "/netlox/v1/maintenance":
			w.Write([]byte(`{"state":"idle","operation_id":"","refusing_new_config":false,"refusing_new_inference":false,"in_flight_streams":0,"in_flight_requests":0,"cancellable":false}`))
		case "/netlox/v1/config/halfclose":
			w.Write([]byte(`{"allow":true,"capSeconds":240,"defaultMode":"relay"}`))
		case "/netlox/v1/audit/status":
			w.Write([]byte(`{"available":true,"running":true,"boot_id":"b-1","seq_high":120,"last_write":"2026-10-28T10:00:00Z","write_failures":0,"sealed_bytes":4096,"orphaned_intents":0,"compliance_sink":false,"sinks":[]}`))
		case "/netlox/v1/audit/policy":
			w.Write([]byte(`{"max_segment_bytes":1048576,"max_segment_age_seconds":3600,"retention_max_age_seconds":2592000,"retention_max_bytes":104857600,"retention_reserve_bytes":1048576,"retention_max_prune_per_pass":8}`))
		case "/netlox/v1/audit/sink":
			w.Write([]byte(`{"enabled":true,"address":"siem.example:6514","ca_bundle_path":"/etc/loxilb/ca.pem","server_name":"siem.example","client_cert_path":"/etc/loxilb/c.pem","client_key_path":"/etc/loxilb/k.pem","max_frame_bytes":8192,"facility":13,"connected":true,"submitted":100,"truncated":0,"write_errors":0,"last_error":""}`))
		case "/netlox/v1/audit/sinks/second":
			w.Write([]byte(`{"name":"second","address":"siem2.example:6514","state":"connected","submitted":7,"filtered":1,"last_error":""}`))
		case "/netlox/v1/config/snapshot":
			w.Header().Set("X-Snapshot-Checksum", "sha256:snap")
			w.Write([]byte(`{"schema":"1.12","checksum":"sha256:snap","loadbalancers":[{"name":"web"}],"auth":{"password":"hunter2","apiKey":"sk-secret-123"},"components":"` + r.URL.Query().Get("components") + `"}`))
		case "/netlox/v1/config/gpu/status":
			w.Write([]byte(`{"enabled":false,"routing_mode":"chwbl","worker_count":2,"ebpf_map_loaded":true}`))
		case "/netlox/v1/config/llamafirewall/status":
			w.Write([]byte(`{"enabled":false,"server_url":"localhost:50052","connected":false,"fail_closed":true,"block_threshold":0.8,"scanners":{"prompt_guard":true,"code_shield":false,"regex":true,"hidden_ascii":false,"agent_alignment":false,"pii_detection":false},"cache_enabled":true,"cache_ttl_sec":60,"scan_patterns":["/v1/*"],"skip_patterns":[],"last_health_check":"never"}`))
		case "/netlox/v1/config/pii/status":
			w.Write([]byte(`{"enabled":false,"mode":"detect","direction":"both","fail_mode":"open","scan_mode":"full","analyzer_url":"localhost:3000","anonymizer_url":"localhost:3001","score_threshold":0.5,"timeout_ms":1000,"url_patterns":[{"pattern":"/v1/chat*","is_exclude":false}],"url_pattern_count":1}`))
		case "/netlox/v1/config/params":
			w.Write([]byte(`{"logLevel":"info"}`))
		case "/netlox/v1/config/bgp/neigh/all":
			w.Write([]byte(`{"bgpNeiAttr":[{"ipAddress":"10.0.0.2","remoteAs":65001,"state":"established","remotePort":179,"multiHop":false}]}`))
		case "/netlox/v1/version":
			w.Write([]byte(`{"version":"v0.9.9-ci","buildInfo":"ci","product":"loxilb-inference-gateway","features":["epp","api_key_auth","jwt_auth_profile"]}`))
		case "/netlox/v1/status/ready":
			// The not-ready verdict travels as a 503 with the same body.
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"ready":false,"reasons":["boot replay pending"]}`))
		case "/netlox/v1/config/export":
			w.Write([]byte(`{"loadbalancers":[{"name":"web"}],"auth":{"password":"hunter2","apiKey":"sk-secret-123"}}`))
		default:
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/netlox/v1/config/loadbalancer/externalipaddress/") {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"code":404,"message":"no such rule"}`))
				return
			}
			w.Write([]byte(`{"result":"Success"}`))
		}
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockLoxilb) lastBody() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		return nil
	}
	return m.bodies[len(m.bodies)-1]
}

func (m *mockLoxilb) sawMutation(prefix string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.requests {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

// session connects an in-memory MCP client to a per-role server.
func session(t *testing.T, b *Bridge, role guard.Role) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	if _, err := b.BuildServer(role).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callOut unmarshals a tool result's structured content into a map.
func callOut(t *testing.T, res *sdk.CallToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// T3: tools/list per role — viewer sees no mutating tools, operator no
// destructive tools, admin everything; config_import only with AllowImport.
func TestPhase2RoleVisibility(t *testing.T) {
	b := newTestBridge(t, testConfig("http://127.0.0.1:11111"))

	viewer, _ := listNames(t, b, guard.RoleViewer)
	for _, want := range []string{"endpoint_list", "fw_list", "ipfilter_list", "secrate_get",
		"net_route_list", "net_vlan_list", "net_vxlan_list", "net_neighbor_list",
		"net_ip_list", "net_port_list", "bgp_neigh_list", "bgp_policy_list",
		"session_list", "session_ulcl_list", "config_export", "config_params_get",
		"log_archive_get"} {
		if !viewer[want] {
			t.Errorf("viewer: read tool %s missing", want)
		}
	}
	for name := range viewer {
		switch name {
		case "lb_create", "lb_delete", "fw_create", "fw_delete", "ipfilter_set",
			"secrate_set", "secrate_reset", "net_route_create", "net_route_delete",
			"bgp_neigh_set", "bgp_global_set", "bgp_policy_apply",
			"endpoint_host_state_set", "config_params_set", "config_import":
			t.Errorf("viewer: mutating tool %s visible", name)
		}
	}

	operator, _ := listNames(t, b, guard.RoleOperator)
	for _, want := range []string{"lb_create", "fw_create", "ipfilter_set",
		"net_route_create", "endpoint_host_state_set"} {
		if !operator[want] {
			t.Errorf("operator: mutating tool %s missing", want)
		}
	}
	// Global/instance-scope changes are admin-confirm since PR-A2 (D13):
	// operator sessions do not list them.
	for _, absent := range []string{"lb_delete", "fw_delete", "net_route_delete", "config_import",
		"secrate_set", "secrate_reset", "bgp_neigh_set", "bgp_global_set", "bgp_policy_apply",
		"config_params_set"} {
		if operator[absent] {
			t.Errorf("operator: admin-confirm tool %s visible", absent)
		}
	}

	admin, _ := listNames(t, b, guard.RoleAdmin)
	for _, want := range []string{"lb_delete", "fw_delete", "net_route_delete",
		"secrate_set", "secrate_reset", "bgp_neigh_set", "bgp_global_set", "bgp_policy_apply",
		"config_params_set"} {
		if !admin[want] {
			t.Errorf("admin: destructive tool %s missing", want)
		}
	}
	if admin["config_import"] {
		t.Error("config_import visible without --allow-import")
	}

	b.AllowImport = true
	adminImp, _ := listNames(t, b, guard.RoleAdmin)
	if !adminImp["config_import"] {
		t.Error("config_import missing with --allow-import for admin")
	}
}

// T3: read-only mode hides mutating tools even from admin.
func TestPhase2ReadOnlyHidesMutating(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:11111")
	b, err := NewBridge(cfg, &guard.Policy{ReadOnly: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := listNames(t, b, guard.RoleAdmin)
	for _, absent := range []string{"lb_create", "lb_delete", "fw_create", "fw_delete"} {
		if admin[absent] {
			t.Errorf("read-only mode: %s visible", absent)
		}
	}
	if !admin["lb_list"] || !admin["fw_list"] {
		t.Error("read-only mode removed read tools")
	}
}

// T3: a viewer session calling a mutating tool by name fails (not registered).
func TestPhase2ViewerCannotCallMutating(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "lb_create",
		Arguments: map[string]any{"external_ip": "9.9.9.9", "port": 80, "protocol": "tcp",
			"endpoints": []map[string]any{{"ip": "10.0.0.1", "port": 8080}}},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("viewer executed lb_create")
	}
	if mock.sawMutation("POST /netlox/v1/config/loadbalancer") {
		t.Fatal("mutating request reached loxilb from viewer session")
	}
}

// T4 end-to-end: preview issues a token and performs no delete; execute with
// the token deletes; replay and arg-swap both fail.
func TestPhase2ConfirmFlow(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleAdmin)
	ctx := context.Background()

	args := map[string]any{"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp"}

	// Step 1: preview.
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_delete", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("preview call failed: %v %v", err, res)
	}
	out := callOut(t, res)
	if out["action"] != "preview" {
		t.Fatalf("want preview, got %v", out["action"])
	}
	token, _ := out["confirm_token"].(string)
	if token == "" {
		t.Fatal("no confirm_token in preview")
	}
	if prev, ok := out["preview"].(map[string]any); !ok || prev["match_count"] != float64(1) {
		t.Errorf("preview did not surface the matching rule: %v", out["preview"])
	}
	if mock.sawMutation("DELETE ") {
		t.Fatal("preview performed a DELETE")
	}

	// Step 2a: token with different args → rejected, nothing deleted.
	swapped := map[string]any{"external_ip": "1.2.3.4", "port": 81, "protocol": "tcp",
		"confirm_token": token}
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_delete", Arguments: swapped})
	if err == nil && !res.IsError {
		t.Fatal("swapped-args execute succeeded")
	}
	if mock.sawMutation("DELETE ") {
		t.Fatal("swapped-args execute performed a DELETE")
	}

	// The mismatch burned the token; get a fresh one.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_delete", Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("second preview failed: %v", err)
	}
	token, _ = callOut(t, res)["confirm_token"].(string)

	// Step 2b: correct token + identical args → executes.
	exec := map[string]any{"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp",
		"confirm_token": token}
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_delete", Arguments: exec})
	if err != nil || res.IsError {
		t.Fatalf("execute failed: %v %+v", err, res)
	}
	if callOut(t, res)["action"] != "executed" {
		t.Fatalf("want executed, got %v", callOut(t, res)["action"])
	}
	if !mock.sawMutation("DELETE /netlox/v1/config/loadbalancer/externalipaddress/1.2.3.4") {
		t.Fatal("no DELETE reached loxilb on execute")
	}

	// Step 3: replay of the consumed token → rejected.
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_delete", Arguments: exec})
	if err == nil && !res.IsError {
		t.Fatal("replayed token succeeded")
	}
}

// --no-confirm mode: destructive tools execute directly (CI).
func TestPhase2NoConfirmExecutesDirectly(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	b.SetNoConfirm()
	cs := session(t, b, guard.RoleAdmin)

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lb_delete",
		Arguments: map[string]any{"external_ip": "1.2.3.4", "port": 80, "protocol": "tcp"}})
	if err != nil || res.IsError {
		t.Fatalf("no-confirm delete failed: %v", err)
	}
	if callOut(t, res)["action"] != "executed" {
		t.Fatal("no-confirm delete did not execute")
	}
	if !mock.sawMutation("DELETE ") {
		t.Fatal("no DELETE reached loxilb")
	}
}

// T5: config_export masks secret-shaped fields.
func TestPhase2ConfigExportMasksSecrets(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "config_export",
		Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("config_export failed: %v", err)
	}
	raw, _ := json.Marshal(callOut(t, res))
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "sk-secret-123") {
		t.Fatalf("config_export leaked secrets: %s", raw)
	}
	if !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatalf("config_export did not mask: %s", raw)
	}
	if !strings.Contains(string(raw), "loadbalancers") {
		t.Fatal("config_export dropped non-secret content")
	}
}

// T10: traversal-shaped filenames are rejected bridge-side, before any
// request reaches loxilb.
func TestPhase2LogArchiveTraversal(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)

	for _, bad := range []string{"../../etc/passwd", "a/b", "..", "x%2f..%2fetc", ""} {
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
			Name: "log_archive_get", Arguments: map[string]any{"filename": bad}})
		if err == nil && (res == nil || !res.IsError) {
			t.Errorf("filename %q accepted", bad)
		}
	}
	if mock.sawMutation("GET /netlox/v1/log-archives/") {
		t.Fatal("traversal request reached loxilb")
	}
}

// Guard sanity: lb_create validates inputs before any REST call.
func TestPhase2LBCreateValidation(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleOperator)
	ctx := context.Background()

	bad := []map[string]any{
		{"external_ip": "not-an-ip", "port": 80, "protocol": "tcp",
			"endpoints": []map[string]any{{"ip": "10.0.0.1", "port": 80}}},
		{"external_ip": "9.9.9.9", "port": 99999, "protocol": "tcp",
			"endpoints": []map[string]any{{"ip": "10.0.0.1", "port": 80}}},
		{"external_ip": "9.9.9.9", "port": 80, "protocol": "bogus",
			"endpoints": []map[string]any{{"ip": "10.0.0.1", "port": 80}}},
		{"external_ip": "9.9.9.9", "port": 80, "protocol": "tcp"},
	}
	for i, args := range bad {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: args})
		if err == nil && (res == nil || !res.IsError) {
			t.Errorf("bad input %d accepted", i)
		}
	}
	if mock.sawMutation("POST /netlox/v1/config/loadbalancer") {
		t.Fatal("invalid lb_create reached loxilb")
	}

	good := map[string]any{"external_ip": "9.9.9.9", "port": 80, "protocol": "tcp",
		"endpoints": []map[string]any{{"ip": "10.0.0.1", "port": 8080}}}
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: good})
	if err != nil || res.IsError {
		t.Fatalf("valid lb_create failed: %v %+v", err, res)
	}
	if !mock.sawMutation("POST /netlox/v1/config/loadbalancer") {
		t.Fatal("valid lb_create never reached loxilb")
	}

	advanced := map[string]any{
		"external_ip": "9.9.9.10", "port": 9003, "protocol": "tcp", "mode": 4,
		"host": "models.example", "path_prefix": "/v1", "model_name": "llama3",
		"sse_mode": true, "pd_disagg_mode": true, "pd_cache_aware_mode": true,
		"api_key_auth": "disabled", "kv_exact_mode": 1, "kv_engine_type": "vllm",
		"kv_zmq_port": 5557, "kv_block_size": 16,
		"endpoints": []map[string]any{
			{"ip": "10.0.0.1", "port": 8000, "ep_role": 1, "nixl_port": 9001},
			{"ip": "10.0.0.2", "port": 8000, "ep_role": 2, "nixl_port": 9002},
		},
	}
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: advanced})
	if err != nil || res.IsError {
		t.Fatalf("advanced lb_create failed: %v %+v", err, res)
	}
	body := mock.lastBody()
	svc, _ := body["serviceArguments"].(map[string]any)
	eps, _ := body["endpoints"].([]any)
	if svc["api_key_auth"] != "disabled" || svc["pd_disagg_mode"] != true || svc["kvExactMode"] != float64(1) {
		t.Fatalf("typed AI service contract not forwarded: %#v", svc)
	}
	if len(eps) != 2 {
		t.Fatalf("advanced endpoints=%d, want 2", len(eps))
	}
	first, _ := eps[0].(map[string]any)
	if first["ep_role"] != float64(1) || first["nixl_port"] != float64(9001) {
		t.Fatalf("typed P/D endpoint contract not forwarded: %#v", first)
	}

	badPolicy := advanced
	badPolicy["api_key_auth"] = "optional"
	res, err = cs.CallTool(ctx, &sdk.CallToolParams{Name: "lb_create", Arguments: badPolicy})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatal("invalid api_key_auth accepted by lb_create")
	}
}

func TestPhase2LBListObservesAIRoutingContract(t *testing.T) {
	mock := newMockLoxilb(t)
	b := newTestBridge(t, testConfig(mock.srv.URL))
	cs := session(t, b, guard.RoleViewer)

	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "lb_list", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("lb_list failed: %v %+v", err, res)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		`"api_key_auth":"required"`, `"pd_disagg_mode":true`, `"pd_cache_aware_mode":true`,
		`"kv_exact_mode":1`, `"kv_engine_type":"vllm"`, `"ep_role":1`, `"nixl_port":9001`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("lb_list omitted %s: %s", want, text)
		}
	}
}
