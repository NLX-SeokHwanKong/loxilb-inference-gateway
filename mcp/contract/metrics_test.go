/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var metricLitRe = regexp.MustCompile(`loxilb_[a-z0-9_]+`)

// metricFamilies runs tools/metric-manifest against the gateway tree and
// returns name -> type.
func metricFamilies(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "tools", "metric-manifest", "main.go")); err != nil {
		t.Skip("tools/metric-manifest not found; metric reference check skipped")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "./tools/metric-manifest", "-root", ".")
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go run ./tools/metric-manifest: %v", err)
	}
	var defs []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(out, &defs); err != nil {
		t.Fatalf("metric-manifest output: %v", err)
	}
	fam := map[string]string{}
	for _, d := range defs {
		fam[d.Name] = d.Type
	}
	if len(fam) < 100 {
		t.Fatalf("metric-manifest found only %d families", len(fam))
	}
	return fam
}

// bridgeMetricLiterals collects every loxilb_* literal under mcp/internal
// and mcp/deploy, with one file:line each.
func bridgeMetricLiterals(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, dir := range []string{"../internal", "../deploy"} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "_gen.go") {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(raw), "\n") {
				for _, loc := range metricLitRe.FindAllStringIndex(line, -1) {
					// "instance:loxilb_ai_error_ratio:rate5m" is a Prometheus
					// recording rule, not a gateway family.
					if (loc[0] > 0 && line[loc[0]-1] == ':') || (loc[1] < len(line) && line[loc[1]] == ':') {
						continue
					}
					m := line[loc[0]:loc[1]]
					if _, seen := out[m]; !seen {
						out[m] = fmt.Sprintf("%s:%d", path, i+1)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// resolveMetric reports whether a referenced literal names a family: an
// exact name, a histogram or summary series (_bucket, _sum, _count), or a
// prefix (a literal ending in "_" is a glob in a description).
func resolveMetric(lit string, fam map[string]string) bool {
	if _, ok := fam[lit]; ok {
		return true
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(lit, suffix); ok {
			if typ, ok := fam[base]; ok && (typ == "histogram" || typ == "summary") {
				return true
			}
		}
	}
	if strings.HasSuffix(lit, "_") {
		for name := range fam {
			if strings.HasPrefix(name, lit) {
				return true
			}
		}
	}
	return false
}

func TestResolveMetric(t *testing.T) {
	fam := map[string]string{"loxilb_x_seconds": "histogram", "loxilb_y_total": "counter", "loxilb_ai_z": "gauge"}
	for _, ok := range []string{"loxilb_x_seconds", "loxilb_x_seconds_bucket", "loxilb_x_seconds_count", "loxilb_ai_", "loxilb_y_total"} {
		if !resolveMetric(ok, fam) {
			t.Errorf("%s must resolve", ok)
		}
	}
	for _, bad := range []string{"loxilb_y_total_bucket", "loxilb_nope", "loxilb_x_seconds_total", "loxilb_q_"} {
		if resolveMetric(bad, fam) {
			t.Errorf("%s must not resolve", bad)
		}
	}
}

func TestBridgeMetricReferencesExist(t *testing.T) {
	fam := metricFamilies(t)
	lits := bridgeMetricLiterals(t)

	var exceptions struct {
		Version      int `yaml:"version"`
		KnownMissing []struct {
			Name   string `yaml:"name"`
			Reason string `yaml:"reason"`
		} `yaml:"known_missing"`
	}
	raw, err := os.ReadFile(exceptionsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &exceptions); err != nil {
		t.Fatal(err)
	}
	known := map[string]string{}
	for _, k := range exceptions.KnownMissing {
		known[k.Name] = k.Reason
		if resolveMetric(k.Name, fam) {
			t.Errorf("%s: metrics-exceptions.yml lists it as missing but it exists now; drop the exception", k.Name)
		}
		if _, referenced := lits[k.Name]; !referenced {
			t.Errorf("%s: metrics-exceptions.yml lists it but the bridge no longer references it; drop the exception", k.Name)
		}
	}

	referenced := map[string]bool{}
	for _, lit := range sortedKeys(lits) {
		if resolveMetric(lit, fam) {
			// A glob prefix in a description is not a reference to each family.
			if _, ok := fam[lit]; ok {
				referenced[lit] = true
			} else if !strings.HasSuffix(lit, "_") {
				for _, suffix := range []string{"_bucket", "_sum", "_count"} {
					if base, ok := strings.CutSuffix(lit, suffix); ok {
						referenced[base] = true
					}
				}
			}
			continue
		}
		if why, ok := known[lit]; ok {
			summary(t, "> **WARNING** %s references %s, which the gateway does not define: %s", lits[lit], lit, why)
			continue
		}
		t.Errorf("%s: %s is not a gateway metric family (W6-class defect)", lits[lit], lit)
	}

	// Backlog: families the bridge never references, grouped by prefix.
	groups := map[string][]string{}
	for _, name := range sortedKeys(fam) {
		if referenced[name] {
			continue
		}
		groups[metricPrefix(name)] = append(groups[metricPrefix(name)], name)
	}
	var rows []string
	total := 0
	for _, p := range sortedKeys(groups) {
		rows = append(rows, fmt.Sprintf("| %s | %d | %s |", p, len(groups[p]), strings.Join(groups[p], ", ")))
		total += len(groups[p])
	}
	summary(t, "### Metric references\n\nReferenced literals: %d, gateway families: %d, families the bridge never references: %d\n\n<details><summary>Unreferenced families by prefix</summary>\n\n| prefix | families | names |\n|---|---|---|\n%s\n\n</details>",
		len(lits), len(fam), total, strings.Join(rows, "\n"))
}

func metricPrefix(name string) string {
	parts := strings.SplitN(strings.TrimPrefix(name, "loxilb_"), "_", 3)
	if len(parts) >= 2 {
		return "loxilb_" + parts[0] + "_" + parts[1]
	}
	return "loxilb_" + parts[0]
}
