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

// logLinesDroppedLevels is the closed set of the level label, in the order
// the series are emitted.
var logLinesDroppedLevels = []string{"error", "warn", "notice", "info", "debug", "trace"}

const logLinesDroppedHelp = "Operational log lines dropped because the log's in-memory queue was full, by the line's level (error, warn, notice, info, debug, trace). The gateway's log and the data-plane log are counted together. A log call never waits for the log's file or terminal: when their device is slower than the lines arrive, lines are dropped here and requests are not held. Every level is reported from start, at zero."

var logLinesDroppedDesc = prometheus.NewDesc(
	"loxilb_log_lines_dropped_total",
	logLinesDroppedHelp,
	[]string{"level"}, nil)

// logLinesDroppedSource returns the dropped lines by level. It is read at
// scrape time; nothing on a log call touches a metric.
var logLinesDroppedSource atomic.Pointer[func() map[string]uint64]

// SetLogLinesDroppedSource sets where loxilb_log_lines_dropped_total reads
// its values. Until it is called every level reads zero.
func SetLogLinesDroppedSource(fn func() map[string]uint64) {
	logLinesDroppedSource.Store(&fn)
}

type logLinesDroppedCollector struct{}

func (logLinesDroppedCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- logLinesDroppedDesc
}

func (logLinesDroppedCollector) Collect(ch chan<- prometheus.Metric) {
	var got map[string]uint64
	if fn := logLinesDroppedSource.Load(); fn != nil {
		got = (*fn)()
	}
	for _, level := range logLinesDroppedLevels {
		ch <- prometheus.MustNewConstMetric(logLinesDroppedDesc, prometheus.CounterValue, float64(got[level]), level)
	}
}

func init() {
	prometheus.MustRegister(logLinesDroppedCollector{})
}
