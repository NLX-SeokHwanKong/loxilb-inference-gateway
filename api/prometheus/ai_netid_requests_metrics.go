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

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// What a network-identity binding did for a request whose source it
// matched. The set is closed: the outcome label never carries anything
// else.
const (
	// NetIDOutcomeAdmitted: the binding decided the request's tenant and
	// the request was admitted.
	NetIDOutcomeAdmitted = "admitted"
	// NetIDOutcomeMatched: a credential decided the tenant and the request
	// was admitted; the binding matched beside it.
	NetIDOutcomeMatched = "matched"
	// NetIDOutcomeDenied: the request was refused, by the binding or by any
	// later stage.
	NetIDOutcomeDenied = "denied"
)

// aiNetIDRequestsTotal counts the requests whose source matched a
// network-identity binding, by what became of them. It is fed from the two
// places a request's record is written, so it counts what the audit trail
// records: an admitted request is counted when its response completes, a
// refused one when it is refused.
var aiNetIDRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "loxilb_ai_netid_requests_total",
		Help: "Total AI Gateway requests whose source matched a network-identity binding, by binding, by the tenant the request was metered as, and by outcome: admitted (the binding decided the tenant; counted when the response completes), matched (a credential decided the tenant and the binding matched beside it; counted when the response completes), denied (refused at admission or by the capacity gate, whatever refused it; counted when it is refused). admitted and matched are counted when the request completes, so a request still in flight is in neither. Absent until a request matches a binding.",
	},
	[]string{"binding", "tenant", "outcome"},
)

// RecordNetIDRequest increments loxilb_ai_netid_requests_total. binding is
// the binding's name and tenant the tenant the request was metered as: the
// binding's own when it decided, the credential's otherwise. Both are
// operator-chosen names, sanitized as every tenant label is. outcome is one
// of the NetIDOutcome values; anything else is counted as denied, so a
// caller's mistake cannot mint a series.
func RecordNetIDRequest(binding, tenant, outcome string) {
	switch outcome {
	case NetIDOutcomeAdmitted, NetIDOutcomeMatched, NetIDOutcomeDenied:
	default:
		outcome = NetIDOutcomeDenied
	}
	aiNetIDRequestsTotal.WithLabelValues(sanitizeLabel(binding), sanitizeLabel(tenant), outcome).Inc()
}
