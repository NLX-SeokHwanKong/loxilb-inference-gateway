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
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// The store write-outcome family: how many management writes were handed
// to a store and not answered within the budget, by plane. Each one was
// answered 504 and may have been applied by the store afterwards; the audit
// trail carries a result record with reason store_outcome_unknown for each.
// Read at scrape time from pkg/pgstore's counters through the source set by
// pkg/loxinet; the store packages import no Prometheus client.

const aiStoreWriteOutcomeUnknownHelp = "Management-plane store writes whose statement was handed to the store and not answered within LLB_AI_STORE_OP_TIMEOUT_MS, by plane (aikey: the API-key store; mgmt: the management credential store). Each was answered 504 ai_key_store_outcome_unknown (management-plane result store_outcome_unknown) and may have been applied by the store after the caller gave up; the audit trail holds a result record with reason store_outcome_unknown for each, carrying the request identity to reconcile it by. Writes the store never received are not counted: they are 503 and safe to retry."

var aiStoreWriteOutcomeUnknownDesc = prometheus.NewDesc(
	"loxilb_ai_store_write_outcome_unknown_total",
	aiStoreWriteOutcomeUnknownHelp,
	[]string{"plane"}, nil)

// AIStoreWriteOutcomeSnapshot is what the source returns on every scrape:
// the count per plane, every plane present so the series exist from the
// start.
type AIStoreWriteOutcomeSnapshot struct {
	UnknownByPlane map[string]uint64
}

var aiStoreWriteOutcomeSource atomic.Pointer[func() AIStoreWriteOutcomeSnapshot]

// SetAIStoreWriteOutcomeSource sets where the family reads its values.
// Until it is called the family has no series.
func SetAIStoreWriteOutcomeSource(fn func() AIStoreWriteOutcomeSnapshot) {
	aiStoreWriteOutcomeSource.Store(&fn)
}

type aiStoreWriteOutcomeCollector struct{}

func (aiStoreWriteOutcomeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- aiStoreWriteOutcomeUnknownDesc
}

func (aiStoreWriteOutcomeCollector) Collect(ch chan<- prometheus.Metric) {
	fn := aiStoreWriteOutcomeSource.Load()
	if fn == nil {
		return
	}
	snap := (*fn)()
	for plane, n := range snap.UnknownByPlane {
		ch <- prometheus.MustNewConstMetric(aiStoreWriteOutcomeUnknownDesc, prometheus.CounterValue, float64(n), plane)
	}
}

func init() {
	prometheus.MustRegister(aiStoreWriteOutcomeCollector{})
}
