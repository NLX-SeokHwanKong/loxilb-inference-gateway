/*
 * Copyright (c) 2026 LoxiLB Authors
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

import "testing"

// One series per binding, tenant and outcome; an outcome outside the closed
// set lands on denied instead of making a series of its own.
func TestRecordNetIDRequest(t *testing.T) {
	const binding, tenant = "line-wp5", "tenant-wp5"
	get := func(outcome string) float64 {
		return getCounterValue(aiNetIDRequestsTotal, binding, tenant, outcome)
	}
	admitted, matched, denied := get(NetIDOutcomeAdmitted), get(NetIDOutcomeMatched), get(NetIDOutcomeDenied)

	RecordNetIDRequest(binding, tenant, NetIDOutcomeAdmitted)
	RecordNetIDRequest(binding, tenant, NetIDOutcomeAdmitted)
	RecordNetIDRequest(binding, tenant, NetIDOutcomeMatched)
	RecordNetIDRequest(binding, tenant, NetIDOutcomeDenied)
	RecordNetIDRequest(binding, tenant, "something-else")

	if got := get(NetIDOutcomeAdmitted); got != admitted+2 {
		t.Errorf("admitted = %v, want %v", got, admitted+2)
	}
	if got := get(NetIDOutcomeMatched); got != matched+1 {
		t.Errorf("matched = %v, want %v", got, matched+1)
	}
	if got := get(NetIDOutcomeDenied); got != denied+2 {
		t.Errorf("denied = %v, want %v: the unknown outcome belongs here", got, denied+2)
	}
	if got := getCounterValue(aiNetIDRequestsTotal, binding, tenant, "something-else"); got != 0 {
		t.Errorf("an outcome outside the set made its own series (%v)", got)
	}

	// Names are sanitized as tenant labels are everywhere else.
	RecordNetIDRequest("line/1 a", "ten ant", NetIDOutcomeAdmitted)
	if got := getCounterValue(aiNetIDRequestsTotal, "line_1_a", "ten_ant", NetIDOutcomeAdmitted); got < 1 {
		t.Errorf("sanitized series = %v, want at least 1", got)
	}
}
