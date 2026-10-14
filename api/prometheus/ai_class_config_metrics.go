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

const aiClassLKGIntervalsHelp = "Refresh intervals (LLB_AI_OBJECTIVE_REFRESH_SEC, default 10 s) in which this gateway served objectives and tenant and key classes from its last-known-good file because the AI key store has not answered since the gateway started. It moves only while fc_effective.config_source reads lkg; any increase is a warning: classes are as the store had them at the file's source_timestamp, and class and objective writes are refused with 503. Zero on a gateway whose store answered, and on one with no key store."

var aiClassLKGIntervalsDesc = prometheus.NewDesc(
	"loxilb_ai_class_config_lkg_intervals_total",
	aiClassLKGIntervalsHelp,
	nil, nil)

// aiClassLKGIntervalsSource returns the count. It is read at scrape time.
var aiClassLKGIntervalsSource atomic.Pointer[func() uint64]

// SetAIClassLKGIntervalsSource sets where
// loxilb_ai_class_config_lkg_intervals_total reads its value. Until it is
// called the family reads zero.
func SetAIClassLKGIntervalsSource(fn func() uint64) {
	aiClassLKGIntervalsSource.Store(&fn)
}

type aiClassLKGIntervalsCollector struct{}

func (aiClassLKGIntervalsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- aiClassLKGIntervalsDesc
}

func (aiClassLKGIntervalsCollector) Collect(ch chan<- prometheus.Metric) {
	var n uint64
	if fn := aiClassLKGIntervalsSource.Load(); fn != nil {
		n = (*fn)()
	}
	ch <- prometheus.MustNewConstMetric(aiClassLKGIntervalsDesc, prometheus.CounterValue, float64(n))
}

func init() {
	prometheus.MustRegister(aiClassLKGIntervalsCollector{})
}
