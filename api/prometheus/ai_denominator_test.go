/*
 * Copyright (c) 2025 LoxiLB Authors
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

package prometheus

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// TestRecordAIRequestDeniedIsADenominator states the contract the outcome label
// exists to provide: denials land under outcome="denied", completions under
// outcome="completed", the two never overlap, and summing them reproduces the
// unfiltered total a consumer uses as its denominator.
func TestRecordAIRequestDeniedIsADenominator(t *testing.T) {
	const tenant = "test-tenant-denominator"
	const model = "m-denominator"

	completed := func(status string) float64 {
		return getCounterValue(aiRequestsTotal, model, tenant, status, AIOutcomeCompleted, "")
	}
	denied := func(status string) float64 {
		return getCounterValue(aiRequestsTotal, model, tenant, status, AIOutcomeDenied, "")
	}

	before200, before429c, before429d := completed("200"), completed("429"), denied("429")

	RecordAIRequest(tenant, model, 200, 12, "")
	RecordAIRequestDenied(tenant, model, 429)

	if d := completed("200") - before200; d != 1.0 {
		t.Errorf("completed 200: want +1, got %f", d)
	}
	if d := denied("429") - before429d; d != 1.0 {
		t.Errorf("denied 429: want +1, got %f", d)
	}
	// The whole point of the label: a gate denial must not be readable as a
	// backend that answered 429 by itself.
	if d := completed("429") - before429c; d != 0.0 {
		t.Errorf("a denial leaked into outcome=completed: got %f", d)
	}

	total := (completed("200") - before200) +
		(completed("429") - before429c) +
		(denied("429") - before429d)
	if total != 2.0 {
		t.Errorf("sum by(outcome) must reproduce the total: want 2, got %f", total)
	}
}

// TestRecordAIRequestDeniedLeavesLatencyAlone pins the deliberate omission.
//
// aiRequestDurationSeconds measures SSE activation to stream completion. A
// denial has no such interval, and observing a gate-decision latency there
// would drag the served-latency quantiles toward zero exactly when denials
// spike -- an outage would look like a latency improvement.
func TestRecordAIRequestDeniedLeavesLatencyAlone(t *testing.T) {
	const tenant = "test-tenant-denial-latency"
	const model = "m-denial-latency"

	count := func() uint64 {
		m := &dto.Metric{}
		obs := aiRequestDurationSeconds.WithLabelValues(model, tenant)
		if err := obs.(interface{ Write(*dto.Metric) error }).Write(m); err != nil {
			t.Fatalf("reading histogram: %v", err)
		}
		return m.GetHistogram().GetSampleCount()
	}

	before := count()
	RecordAIRequestDenied(tenant, model, 503)
	RecordAIRequestDenied(tenant, model, 401)
	if got := count(); got != before {
		t.Errorf("denials must not be observed in the duration histogram: %d -> %d", before, got)
	}
}

// TestRecordAIRequestDeniedTenantIsEmptyNotAPlaceholder pins the documented
// label value for the arms that deny before a credential resolves. A consumer
// grouping by tenant has to be able to tell "denied before we knew who it was"
// from a tenant literally named "unknown".
func TestRecordAIRequestDeniedTenantIsEmptyNotAPlaceholder(t *testing.T) {
	const model = "m-anonymous-denial"

	before := getCounterValue(aiRequestsTotal, model, "", "401", AIOutcomeDenied, "")
	RecordAIRequestDenied("", model, 401)
	if d := getCounterValue(aiRequestsTotal, model, "", "401", AIOutcomeDenied, "") - before; d != 1.0 {
		t.Errorf("empty tenant must be recorded as the empty label value, got delta %f", d)
	}
}

// gateExports are the CGO exports through which the AI Gateway policy gate
// refuses a request. The C gate calls each from inside its decision
// (ai_gw_admit_decide) and turns a non-zero return into a refusal, which it
// records once at its verdict frame through llb_ai_record_deny. That record
// is where the request is counted, not the export.
var gateExports = map[string]bool{
	"llb_ai_validate_key":        true,
	"llb_ai_validate_bearer":     true,
	"llb_ai_ratelimit_check":     true,
	"llb_ai_token_quota_reserve": true,
}

// advisoryDecisionExports write a decision that never reaches a client, so
// counting one would inflate the total with a request nobody was refused.
//
// The value is the reason, and it is required rather than decorative: this is
// the map where a genuine denial could hide behind an assertion that it is not
// one, so the reason has to be re-checkable by whoever reads it next.
var advisoryDecisionExports = map[string]string{
	// Called at response completion to charge the tokens a served response
	// actually used, and every C call site passes result=NULL -- the branch
	// that writes a decision here is unreachable through them. Going over
	// quota while a response is in flight does not interrupt it; it latches
	// the quota, and the NEXT request is refused by llb_ai_ratelimit_check,
	// which is a gate export. Counting here would count that one request
	// twice, once when it was served and once when a later one was refused
	// for it.
	"llb_ai_token_quota_consume": "charges a served response; C passes result=NULL and the latch denies the next request at the rate-limit gate",
}

// deniedWriters are the recorders of loxilb_ai_requests_total{outcome="denied"}.
var deniedWriters = map[string]bool{
	"RecordAIRequestDenied":         true,
	"RecordAIRequestDeniedUnserved": true,
}

// TestDeniedOutcomeHasOneWriter asserts that a refusal is counted in exactly
// one place: recordAIDeny, reached only from the deny-record export
// llb_ai_record_deny, which the data plane calls once per refusal whatever
// stage refused (the gate's verdict frame, capacity, route, framing). A gate
// export that counted its own refusals as well would put every gate refusal
// in loxilb_ai_requests_total twice.
//
// It also classifies every function that writes a non-zero decision, from
// the source, so a new deny-writing export cannot appear unclassified.
//
// It is here, in a package with no cgo, on purpose: pkg/loxinet cannot be test-
// built without the eBPF datapath archive, so a pin that lived there would run
// on the testbed and nowhere else. go/parser needs no build at all.
func TestDeniedOutcomeHasOneWriter(t *testing.T) {
	const dir = "../../pkg/loxinet"

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}

	denyArms := map[string]int{}
	writers := map[string]int{}           // function -> calls of a denied-outcome recorder
	recordDenyCallers := map[string]int{} // function -> calls of recordAIDeny
	gateDenialRecorder := false

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				name := fn.Name.Name
				// The gate's decisions are written in the gate file; other
				// files write unrelated structs' decision fields.
				gateFile := strings.HasSuffix(fset.Position(fn.Pos()).Filename, "ai_gateway_dp.go")
				if name == "recordGateDenial" {
					gateDenialRecorder = true
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch node := n.(type) {
					case *ast.AssignStmt:
						for i, lhs := range node.Lhs {
							sel, ok := lhs.(*ast.SelectorExpr)
							if !ok || sel.Sel.Name != "decision" || i >= len(node.Rhs) {
								continue
							}
							if gateFile && !isZeroDecision(node.Rhs[i]) {
								denyArms[name]++
							}
						}
					case *ast.CallExpr:
						switch f := node.Fun.(type) {
						case *ast.SelectorExpr:
							if deniedWriters[f.Sel.Name] {
								writers[name]++
							}
						case *ast.Ident:
							if f.Name == "recordAIDeny" {
								recordDenyCallers[name]++
							}
						}
					}
					return true
				})
			}
		}
	}

	if gateDenialRecorder {
		t.Errorf("recordGateDenial is back: the gate exports count nothing; the deny record counts every refusal")
	}
	for fn, n := range writers {
		if fn != "recordAIDeny" {
			t.Errorf("%s writes outcome=\"denied\" (%d call(s)); only recordAIDeny may, or a refusal is counted twice", fn, n)
		}
	}
	if writers["recordAIDeny"] == 0 {
		t.Errorf("recordAIDeny writes no outcome=\"denied\": refusals are not counted at all")
	}
	if len(recordDenyCallers) != 1 || recordDenyCallers["llb_ai_record_deny"] != 1 {
		t.Errorf("recordAIDeny must be reached once, from llb_ai_record_deny; callers: %v", recordDenyCallers)
	}

	for export := range gateExports {
		if denyArms[export] == 0 {
			t.Errorf("gateExports names %s, which writes no decision -- stale entry", export)
		}
		if writers[export] != 0 || recordDenyCallers[export] != 0 {
			t.Errorf("%s counts its own refusal; the C gate's deny record already does", export)
		}
	}
	for name := range advisoryDecisionExports {
		if denyArms[name] == 0 {
			t.Errorf("advisoryDecisionExports names %s, which writes no decision -- stale entry", name)
		}
	}
	// Any OTHER function that writes a decision is an arm this test does not
	// know about. Fail rather than pass silently.
	for name, arms := range denyArms {
		if gateExports[name] {
			continue
		}
		if reason, advisory := advisoryDecisionExports[name]; advisory {
			if reason == "" {
				t.Errorf("%s is listed as advisory with no reason -- state why "+
					"its decision never reaches a client", name)
			}
			continue
		}
		t.Errorf("%s writes %d non-zero decision(s) and is unclassified. Either "+
			"the C gate calls it inside its decision, in which case add it to "+
			"gateExports, or its decision never reaches a client, in which "+
			"case add it to advisoryDecisionExports with the reason.", name, arms)
	}
}

// isZeroDecision reports whether an expression assigned to result.decision is
// the allow verdict. Both the literal 0 and the named constant appear in the
// tree, and C.int(decision) -- a value only known at runtime -- is treated as
// potentially denying, which is the safe direction for this check.
func isZeroDecision(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Kind == token.INT && v.Value == "0"
	case *ast.Ident:
		return v.Name == "aiDecisionAllow"
	}
	return false
}
