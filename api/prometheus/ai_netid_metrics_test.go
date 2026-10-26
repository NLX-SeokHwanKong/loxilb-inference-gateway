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

// The resolver-fault counter is per service and carries the data plane's
// "VIP:port" as it is, so it joins the admission families on their service
// label.
func TestRecordNetIDResolverFault(t *testing.T) {
	const a, b = "192.0.2.60:2060", "192.0.2.60:2061"
	beforeA := getCounterValue(aiNetIDResolverFaultsTotal, a)
	beforeB := getCounterValue(aiNetIDResolverFaultsTotal, b)

	RecordNetIDResolverFault(a)
	RecordNetIDResolverFault(a)

	if got := getCounterValue(aiNetIDResolverFaultsTotal, a); got != beforeA+2 {
		t.Fatalf("service %s = %v, want %v", a, got, beforeA+2)
	}
	if got := getCounterValue(aiNetIDResolverFaultsTotal, b); got != beforeB {
		t.Fatalf("service %s moved to %v with no fault of its own", b, got)
	}
	if got := admissionServiceLabel(0x3c02_00c0, 2060); got != a {
		t.Fatalf("the admission families would label this service %q, the counter %q", got, a)
	}
}
