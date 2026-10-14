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

// The counter is per service and carries the data plane's "VIP:port" as it
// is, so it joins the admission families on their service label.
func TestRecordStreamEarlyBytesClosed(t *testing.T) {
	const a, b = "192.0.2.60:2071", "192.0.2.60:2072"
	beforeA := getCounterValue(aiStreamEarlyBytesClosedTotal, a)
	beforeB := getCounterValue(aiStreamEarlyBytesClosedTotal, b)

	RecordStreamEarlyBytesClosed(a)

	if got := getCounterValue(aiStreamEarlyBytesClosedTotal, a); got != beforeA+1 {
		t.Fatalf("service %s = %v, want %v", a, got, beforeA+1)
	}
	if got := getCounterValue(aiStreamEarlyBytesClosedTotal, b); got != beforeB {
		t.Fatalf("service %s moved to %v with no close of its own", b, got)
	}
	if got := admissionServiceLabel(0x3c02_00c0, 2071); got != a {
		t.Fatalf("the admission families would label this service %q, the counter %q", got, a)
	}
}
