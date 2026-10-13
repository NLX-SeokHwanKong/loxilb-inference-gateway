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

// aiNetIDResolverFaultsTotal counts the requests whose network-identity
// binding could not be looked up: the resolver had nothing to read, or
// failed. Such a request is decided as if its service had no binding (it is
// never admitted by its network, and a strict binding does not hold its
// credential), unless the service fails closed, where it is refused. Either
// way the binding was not applied, which is why it is counted.
var aiNetIDResolverFaultsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "loxilb_ai_netid_resolver_faults_total",
		Help: "Total AI Gateway requests on a service with a network-identity binding for which the binding could not be resolved (no snapshot, or an internal error). The request was decided as if the service had no binding: not admitted by its network, and not held to a strict binding. On a service whose binding sets fail_mode closed it was refused with 503 netid_unavailable instead. By service (VIP:port). Absent until a fault occurs; any value above zero needs attention.",
	},
	[]string{"service"},
)

// RecordNetIDResolverFault increments loxilb_ai_netid_resolver_faults_total
// for the service a request was being resolved on. Called by the resolver
// behind llb_ai_netid_resolve, once per request it could not answer.
//
// service is the data plane's "VIP:port" identity of the listener, built
// from the rule and never from the request: the same value the
// loxilb_ai_admission_* families carry in their service label.
func RecordNetIDResolverFault(service string) {
	aiNetIDResolverFaultsTotal.WithLabelValues(service).Inc()
}
