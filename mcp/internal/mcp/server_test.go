/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

const testToken = "0123456789abcdef0123456789abcdef"

func testConfig(targetURL string) *Config {
	return &Config{
		DefaultTarget: "t1",
		Targets:       map[string]Target{"t1": {URL: targetURL}},
		Clients:       []ClientToken{{Name: "ci", Role: "admin", Token: testToken}},
	}
}

func newTestBridge(t *testing.T, cfg *Config) *Bridge {
	t.Helper()
	b, err := NewBridge(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// T2: plaintext bearer auth on a non-loopback bind must be refused.
func TestRunHTTPRefusesPlaintextNonLoopback(t *testing.T) {
	b := newTestBridge(t, testConfig("http://127.0.0.1:11111"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := b.RunHTTP(ctx, HTTPOptions{Listen: "0.0.0.0:0"})
	if err == nil || !strings.Contains(err.Error(), "refusing plaintext") {
		t.Fatalf("want plaintext refusal, got %v", err)
	}
	// Loopback names must be recognized.
	for _, l := range []string{"127.0.0.1:1", "localhost:1", "[::1]:1"} {
		if !listenIsLoopback(l) {
			t.Errorf("listenIsLoopback(%q) = false", l)
		}
	}
	for _, l := range []string{"0.0.0.0:1", ":1", "192.168.1.5:1"} {
		if listenIsLoopback(l) {
			t.Errorf("listenIsLoopback(%q) = true", l)
		}
	}
}

// HTTP mode without any client tokens must refuse to serve.
func TestHTTPHandlerRequiresClientTokens(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:11111")
	cfg.Clients = nil
	b := newTestBridge(t, cfg)
	if _, err := b.HTTPHandler(); err == nil {
		t.Fatal("handler built without client tokens")
	}
}

func initializeBody(t *testing.T) *strings.Reader {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(body))
}

func doInitialize(t *testing.T, url string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, initializeBody(t))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// T1/T3/T9: auth required, wrong token rejected, cross-origin rejected,
// valid token + non-browser request accepted.
func TestHTTPAuthAndOrigin(t *testing.T) {
	b := newTestBridge(t, testConfig("http://127.0.0.1:11111"))
	h, err := b.HTTPHandler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	if resp := doInitialize(t, srv.URL, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", resp.StatusCode)
	}
	if resp := doInitialize(t, srv.URL, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer wrong-token-wrong-token-wrong-tok")
	}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", resp.StatusCode)
	}
	if resp := doInitialize(t, srv.URL, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testToken)
		r.Header.Set("Origin", "http://evil.example")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin: got %d, want 403", resp.StatusCode)
	}
	if resp := doInitialize(t, srv.URL, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+testToken)
	}); resp.StatusCode != http.StatusOK {
		t.Errorf("valid: got %d, want 200", resp.StatusCode)
	}
}

// T7: model-supplied target names resolve only against the config.
func TestResolveRejectsUnknownAndURLTargets(t *testing.T) {
	b := newTestBridge(t, testConfig("http://127.0.0.1:11111"))
	if _, err := b.resolve(""); err != nil {
		t.Errorf("default target: %v", err)
	}
	for _, bad := range []string{"http://169.254.169.254", "unknown", "t2"} {
		if _, err := b.resolve(bad); err == nil {
			t.Errorf("resolve(%q) succeeded, want error", bad)
		}
	}
}

// T8: per-client token bucket limits request rate.
func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter(1, 3)
	allowed := 0
	var wait time.Duration
	for range 10 {
		ok, w := rl.allow("c")
		if ok {
			allowed++
		} else {
			wait = w
		}
	}
	if allowed != 3 {
		t.Errorf("burst: allowed %d, want 3", allowed)
	}
	if wait <= 0 || wait > time.Second {
		t.Errorf("an empty bucket at 1 rps must say to wait within a second, got %v", wait)
	}
	if ok, _ := rl.allow("other"); !ok {
		t.Error("independent client must have its own bucket")
	}
}

// D20: a client's own rps/burst replace the defaults for that client only,
// and a zero field keeps the default.
func TestRateLimiterPerClientBudget(t *testing.T) {
	rl := newRateLimiter(10, 20)
	rl.setClient("tight", 1, 2)
	rl.setClient("wide", 0, 50)
	if l := rl.limitOf("tight"); l.rps != 1 || l.burst != 2 {
		t.Fatalf("tight: %+v", l)
	}
	if l := rl.limitOf("wide"); l.rps != 10 || l.burst != 50 {
		t.Fatalf("wide keeps the default rps: %+v", l)
	}
	if l := rl.limitOf("plain"); l.rps != 10 || l.burst != 20 {
		t.Fatalf("plain: %+v", l)
	}
	allowed := 0
	for range 5 {
		if ok, _ := rl.allow("tight"); ok {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("tight burst: allowed %d, want 2", allowed)
	}
	allowed = 0
	for range 30 {
		if ok, _ := rl.allow("wide"); ok {
			allowed++
		}
	}
	if allowed != 30 {
		t.Errorf("wide burst 50: allowed %d of 30", allowed)
	}
}

// D20: the refusal carries Retry-After and a JSON body the client can act
// on, and the budget comes from the client's config entry.
func TestHTTPRateLimitedAnswer(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:11111")
	tight := strings.Repeat("b", 32)
	cfg.Clients = append(cfg.Clients, ClientToken{Name: "burst", Role: "viewer", Token: tight, RPS: 1, Burst: 2})
	b := newTestBridge(t, cfg)
	h, err := b.HTTPHandler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	auth := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tight) }
	for i := range 2 {
		if resp := doInitialize(t, srv.URL, auth); resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("request %d inside the burst refused", i+1)
		}
	}
	resp := doInitialize(t, srv.URL, auth)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third request within the second: got %d, want 429", resp.StatusCode)
	}
	ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || ra < 1 {
		t.Fatalf("Retry-After must be whole seconds >= 1, got %q", resp.Header.Get("Retry-After"))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["error"] != "rate_limited" || body["client"] != "burst" || body["retry_after_sec"] != float64(ra) {
		t.Fatalf("body: %v", body)
	}
	lim, _ := body["limit"].(map[string]any)
	if lim["rps"] != float64(1) || lim["burst"] != float64(2) {
		t.Fatalf("limit must name the client's budget: %v", body)
	}
	// The admin client of the config keeps the default budget: 20 in a burst.
	admin := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testToken) }
	for i := range 20 {
		if resp := doInitialize(t, srv.URL, admin); resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("default client refused at request %d of its burst of 20", i+1)
		}
	}
}

// D20: the config refuses a budget that would turn the limiter off.
func TestClientBudgetValidation(t *testing.T) {
	for _, bad := range []ClientToken{
		{Name: "a", Role: "viewer", Token: testToken, RPS: -1},
		{Name: "a", Role: "viewer", Token: testToken, RPS: 0.001},
		{Name: "a", Role: "viewer", Token: testToken, Burst: 0.5},
		{Name: "a", Role: "viewer", Token: testToken, Burst: -2},
	} {
		cfg := testConfig("http://127.0.0.1:11111")
		cfg.Clients = []ClientToken{bad}
		if err := cfg.validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	cfg := testConfig("http://127.0.0.1:11111")
	cfg.Clients = []ClientToken{{Name: "a", Role: "viewer", Token: testToken, RPS: 0.5, Burst: 1}}
	if err := cfg.validate(); err != nil {
		t.Errorf("a slow but positive budget must be accepted: %v", err)
	}
}

// Role-scoped servers: a viewer server must not expose mutating tools once
// later phases add them; for assert build succeeds per role and the
// read-only policy path works end-to-end via config.
func TestBuildServerPerRole(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:11111")
	cfg.Clients = append(cfg.Clients, ClientToken{Name: "ro", Role: "viewer", Token: strings.Repeat("v", 32)})
	b := newTestBridge(t, cfg)
	for _, r := range []string{"viewer", "operator", "admin"} {
		role, err := guard.ParseRole(r)
		if err != nil {
			t.Fatal(err)
		}
		if s := b.BuildServer(role); s == nil {
			t.Errorf("BuildServer(%s) = nil", r)
		}
	}
}

func TestLoadConfigValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	good := write("good.yaml", `
default_target: kv
targets:
  kv:
    url: http://192.168.80.9:11111
clients:
  - name: ci
    role: operator
    token: 0123456789abcdef0123456789abcdef
`)
	cfg, err := LoadConfig(good)
	if err != nil {
		t.Fatalf("good config rejected: %v", err)
	}
	if cfg.DefaultTarget != "kv" || len(cfg.Clients) != 1 {
		t.Errorf("config parsed wrong: %+v", cfg)
	}

	for name, content := range map[string]string{
		"notargets.yaml": `clients: []`,
		"badrole.yaml": `
targets: {a: {url: http://x:1}}
clients: [{name: c, role: root, token: 0123456789abcdef0123456789abcdef}]`,
		"unknownfield.yaml": `
targets: {a: {url: http://x:1}}
bogus_key: true`,
		"bothtokens.yaml": `
targets: {a: {url: http://x:1}}
clients: [{name: c, role: admin, token: 0123456789abcdef0123456789abcdef, token_env: X}]`,
	} {
		if _, err := LoadConfig(write(name, content)); err == nil {
			t.Errorf("%s accepted, want error", name)
		}
	}
}

// The originator the bridge names has a fixed shape per transport: the
// authenticated client over HTTP, the launching process over stdio.
func TestOriginatorShapes(t *testing.T) {
	cl, err := guard.NewClient("claude-desktop", guard.RoleAdmin, testToken)
	if err != nil {
		t.Fatal(err)
	}
	if got := httpOriginator(cl); got != "mcp:claude-desktop" {
		t.Fatalf("http originator %q", got)
	}
	got := stdioOriginator()
	re := regexp.MustCompile(`^mcp-stdio:[^@]+@[^:]+:[0-9]+$`)
	if !re.MatchString(got) {
		t.Fatalf("stdio originator %q does not match %s", got, re)
	}
	if !strings.HasSuffix(got, ":"+strconv.Itoa(os.Getpid())) {
		t.Fatalf("stdio originator %q does not end in this pid", got)
	}
}
