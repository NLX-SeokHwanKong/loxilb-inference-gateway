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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The reason label of loxilb_ai_reservation_clamped_total.
const (
	// ReservationClampBurst: the default beside the prompt is more than a
	// quota bucket can ever hold. Persistent until the default or the
	// quota changes.
	ReservationClampBurst = "burst"
	// ReservationClampBudget: a bucket's budget was already taken.
	// Transient: it passes as the window refills.
	ReservationClampBudget = "budget"
)

// aiReservationClampedTotal counts requests that declared no completion
// ceiling on a service with a default reservation and were admitted with
// less than the default held, because a quota bucket could not cover all of
// it. Per service only: the tenant is on the refusal series, and this one
// answers "is the default sized for the budgets on this service".
var aiReservationClampedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "loxilb_ai_reservation_clamped_total",
		Help: "Total AI Gateway requests without a declared max_tokens whose default token reservation was admitted smaller than the configured default. Never a refusal. By service (VIP:port) and reason: burst = the default beside the prompt is more than a quota bucket can ever hold (a configuration to fix: it is clamped on an idle bucket too), budget = the bucket's budget was already taken (transient). A request both apply to counts as burst. Absent until a service with a default reservation clamps one.",
	},
	[]string{"service", "reason"},
)

// RecordReservationClamped increments loxilb_ai_reservation_clamped_total
// for the service whose default reservation was admitted smaller than
// configured. Called by llb_ai_token_quota_reserve, once per such request.
//
// service is the data plane's "VIP:port" identity of the listener, built
// from the rule and never from the request, and is used as it is: the same
// value the loxilb_ai_admission_* families carry in their service label.
// reason is ReservationClampBurst or ReservationClampBudget.
func RecordReservationClamped(service, reason string) {
	aiReservationClampedTotal.WithLabelValues(service, reason).Inc()
}
