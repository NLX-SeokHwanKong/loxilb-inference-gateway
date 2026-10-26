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

package prometheus

import "testing"

// The clamped-reservation counter is per service and reason, and carries
// the data plane's "VIP:port" as it is, so it joins the admission families
// on their service label.
func TestRecordReservationClamped(t *testing.T) {
	const a, b = "192.0.2.50:2050", "192.0.2.50:2051"
	beforeBurst := getCounterValue(aiReservationClampedTotal, a, ReservationClampBurst)
	beforeBudget := getCounterValue(aiReservationClampedTotal, a, ReservationClampBudget)
	beforeB := getCounterValue(aiReservationClampedTotal, b, ReservationClampBurst) +
		getCounterValue(aiReservationClampedTotal, b, ReservationClampBudget)

	RecordReservationClamped(a, ReservationClampBurst)
	RecordReservationClamped(a, ReservationClampBurst)
	RecordReservationClamped(a, ReservationClampBudget)

	if got := getCounterValue(aiReservationClampedTotal, a, ReservationClampBurst); got != beforeBurst+2 {
		t.Fatalf("service %s reason burst = %v, want %v", a, got, beforeBurst+2)
	}
	if got := getCounterValue(aiReservationClampedTotal, a, ReservationClampBudget); got != beforeBudget+1 {
		t.Fatalf("service %s reason budget = %v, want %v", a, got, beforeBudget+1)
	}
	if got := getCounterValue(aiReservationClampedTotal, b, ReservationClampBurst) +
		getCounterValue(aiReservationClampedTotal, b, ReservationClampBudget); got != beforeB {
		t.Fatalf("service %s moved to %v with no clamp of its own", b, got)
	}
	if got := admissionServiceLabel(0x3202_00c0, 2050); got != a {
		t.Fatalf("the admission families would label this service %q, the counter %q", got, a)
	}
}
