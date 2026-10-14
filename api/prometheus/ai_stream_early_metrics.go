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

// aiStreamEarlyBytesClosedTotal counts the HTTP/1.1 connections the gateway
// closed because their client sent bytes while a streamed answer was still
// running on them. Those bytes belong to no admitted request: relayed, they
// would reach the backend behind the request being answered, past the
// admission gate. HTTP/1.1 cannot answer them before the running answer
// ends, so the connection is closed and nothing is relayed.
var aiStreamEarlyBytesClosedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "loxilb_ai_stream_early_bytes_closed_total",
		Help: "Total HTTP/1.1 client connections closed because the client sent bytes while a streamed (SSE) answer was still running on the connection. Sending a request before the previous streamed answer has ended is not supported: the bytes are not relayed, the connection is closed and the running stream ends as one its client left. By service (VIP:port). Absent until it happens; a steady rate points at a client that pipelines requests.",
	},
	[]string{"service"},
)

// RecordStreamEarlyBytesClosed increments
// loxilb_ai_stream_early_bytes_closed_total for the service the connection
// was on. Called by llb_ai_record_stream_early_close, once per connection:
// the close is what it counts.
//
// service is the data plane's "VIP:port" identity of the listener, the same
// value the loxilb_ai_admission_* families carry in their service label.
func RecordStreamEarlyBytesClosed(service string) {
	aiStreamEarlyBytesClosedTotal.WithLabelValues(service).Inc()
}
