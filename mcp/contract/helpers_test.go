/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package contract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	mcp "github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp"
	"github.com/loxilb-io/loxilb-inference-gateway/mcp/internal/mcp/guard"
)

const (
	manifestPath   = "coverage.yml"
	fieldsPath     = "fields.yml"
	exceptionsPath = "metrics-exceptions.yml"
	toolsDir       = "../internal/mcp/tools"
	clientDir      = "../internal/mcp/client"
	alertRulesPath = "../deploy/monitoring/prometheus/rules/loxilb-alerts.yml"
)

// apiDir returns the gateway api/ directory or skips the test: the mcp
// module must stay testable outside the monorepo (campaign rule 9).
func apiDir(t *testing.T) string {
	t.Helper()
	d := FindAPIDir()
	if d == "" {
		t.Skip("gateway api/ directory not found beside the mcp module; contract gate skipped")
	}
	return d
}

func loadSpec(t *testing.T) *Spec {
	t.Helper()
	spec, err := LoadSwagger(apiDir(t))
	if err != nil {
		t.Fatalf("load swagger: %v", err)
	}
	return spec
}

func loadManifest(t *testing.T) *Manifest {
	t.Helper()
	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("load %s: %v", manifestPath, err)
	}
	return m
}

// summary appends Markdown to the GitHub step summary when running in CI
// and logs it otherwise.
var summaryMu sync.Mutex

func summary(t *testing.T, format string, args ...any) {
	t.Helper()
	text := fmt.Sprintf(format, args...)
	t.Log(text)
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return
	}
	summaryMu.Lock()
	defer summaryMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(text + "\n")
}

// codeTier is what the bridge enforces for a registered tool, read from the
// MCP annotations the registration helpers set: read-only -> viewer,
// destructive (confirm-token gated) -> admin-confirm, otherwise operator.
func codeTier(tool *sdk.Tool) Tier {
	a := tool.Annotations
	if a == nil {
		return ""
	}
	if a.ReadOnlyHint {
		return TierViewer
	}
	if a.DestructiveHint != nil && *a.DestructiveHint {
		return TierAdminConfirm
	}
	return TierOperator
}

// registeredTools builds an admin bridge with every optional backend and
// config_import enabled and lists its tools with the tier each enforces.
func registeredTools(t *testing.T) map[string]Tier {
	t.Helper()
	cfg := &mcp.Config{
		DefaultTarget:   "t1",
		Targets:         map[string]mcp.Target{"t1": {URL: "http://127.0.0.1:11111"}},
		PrometheusURL:   "http://127.0.0.1:9090",
		AlertmanagerURL: "http://127.0.0.1:9093",
	}
	if _, err := os.Stat(alertRulesPath); err == nil {
		cfg.AlertRulesPath = alertRulesPath
	}
	b, err := mcp.NewBridge(cfg, &guard.Policy{}, nil)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	b.AllowImport = true
	srv := b.BuildServer(guard.RoleAdmin)

	ctx := context.Background()
	ct, st := sdk.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cl := sdk.NewClient(&sdk.Implementation{Name: "contract-test", Version: "0"}, nil)
	cs, err := cl.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	out := map[string]Tier{}
	for _, tool := range res.Tools {
		out[tool.Name] = codeTier(tool)
	}
	if len(out) == 0 {
		t.Fatal("no tools registered")
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Clean(filepath.Join(apiDir(t), ".."))
}

func joinSorted(m map[string]bool) string {
	return strings.Join(sortedKeys(m), ", ")
}
