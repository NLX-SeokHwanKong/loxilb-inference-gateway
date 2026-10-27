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
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// The key store's data-path lookup families: how long a proxy worker waited
// on the store for a cache miss and how it ended, whether the store-health
// circuit is open, and how many round trips are on the store right now.
// Read at scrape time from pkg/aikey's lookup statistics through the source
// set by pkg/loxinet; the key-store package itself imports no Prometheus
// client.

// AIKeyLookupResult is one `result` series of the lookup histogram.
type AIKeyLookupResult struct {
	Result string
	Count  uint64
	Sum    float64 // seconds
	// Buckets maps each upper bound (seconds) to its cumulative count.
	Buckets map[float64]uint64
}

// AIKeyLookupSnapshot is what the source returns on every scrape.
type AIKeyLookupSnapshot struct {
	Results     []AIKeyLookupResult
	CircuitOpen bool
	InFlight    int
}

const aiKeyLookupSecondsHelp = "Time a proxy worker spent on one key-store round trip the data path made on a cache miss, by how it ended: miss_found (a row), miss_invalid (no such key or row), timeout (the LLB_AI_KEY_LOOKUP_TIMEOUT_MS budget expired), refused_circuit (the store-health circuit was open: no store contact), refused_inflight (no slot inside the budget: LLB_AI_KEY_LOOKUP_MAX_INFLIGHT), unavailable (another store failure or not connected). Cache hits are not round trips and are not counted."

const aiKeyCircuitOpenHelp = "1 while the key store's data-path circuit is open or half-open: cache misses are refused without contacting the store, quota reads serve their last store-confirmed value. Opens after LLB_AI_KEY_LOOKUP_CIRCUIT_FAILURES consecutive failures, admits one probe after LLB_AI_KEY_LOOKUP_CIRCUIT_COOLDOWN_MS, and closes on the probe's success. It stays 1 after the store recovers until a cache miss runs that probe; a steady 1 with no miss traffic is not an outage. Independent of store_reachable, which the snapshot refresher reports."

const aiKeyLookupInflightHelp = "Data-path key-store round trips on the store right now, bounded by LLB_AI_KEY_LOOKUP_MAX_INFLIGHT."

var (
	aiKeyLookupSecondsDesc = prometheus.NewDesc(
		"loxilb_ai_key_store_lookup_seconds",
		aiKeyLookupSecondsHelp,
		[]string{"result"}, nil)
	aiKeyCircuitOpenDesc = prometheus.NewDesc(
		"loxilb_ai_key_store_circuit_open",
		aiKeyCircuitOpenHelp,
		nil, nil)
	aiKeyLookupInflightDesc = prometheus.NewDesc(
		"loxilb_ai_key_store_lookup_inflight",
		aiKeyLookupInflightHelp,
		nil, nil)
)

// aiKeyLookupSource returns the snapshot. It is read at scrape time.
var aiKeyLookupSource atomic.Pointer[func() AIKeyLookupSnapshot]

// SetAIKeyLookupSource sets where the three loxilb_ai_key_store_* families
// read their values. Until it is called the families read zero and the
// histogram has no series.
func SetAIKeyLookupSource(fn func() AIKeyLookupSnapshot) {
	aiKeyLookupSource.Store(&fn)
}

type aiKeyLookupCollector struct{}

func (aiKeyLookupCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- aiKeyLookupSecondsDesc
	ch <- aiKeyCircuitOpenDesc
	ch <- aiKeyLookupInflightDesc
}

func (aiKeyLookupCollector) Collect(ch chan<- prometheus.Metric) {
	var snap AIKeyLookupSnapshot
	if fn := aiKeyLookupSource.Load(); fn != nil {
		snap = (*fn)()
	}
	for _, r := range snap.Results {
		ch <- prometheus.MustNewConstHistogram(aiKeyLookupSecondsDesc, r.Count, r.Sum, r.Buckets, r.Result)
	}
	open := 0.0
	if snap.CircuitOpen {
		open = 1
	}
	ch <- prometheus.MustNewConstMetric(aiKeyCircuitOpenDesc, prometheus.GaugeValue, open)
	ch <- prometheus.MustNewConstMetric(aiKeyLookupInflightDesc, prometheus.GaugeValue, float64(snap.InFlight))
}

func init() {
	prometheus.MustRegister(aiKeyLookupCollector{})
}
