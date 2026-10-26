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

package loxinet

/*
// The data-plane log's controls, as loxilb_libdp.h declares them.
void loxilb_log_rotate(unsigned long long max_bytes);
void loxilb_log_stop(void);
unsigned long long loxilb_log_dropped(int level);
*/
import "C"

// The data plane's log levels (log.h) a line can be dropped at.
const (
	dpLogTrace = iota
	dpLogDebug
	dpLogInfo
	dpLogWarn
	dpLogError
)

// dpLogRotate tells the data-plane log's writer thread the size at which it
// renames the log and opens it anew; 0 turns that off.
func dpLogRotate(maxBytes uint64) {
	C.loxilb_log_rotate(C.ulonglong(maxBytes))
}

// dpLogStop has the data-plane log write what it holds; later lines are
// written by their callers.
func dpLogStop() {
	C.loxilb_log_stop()
}

// dpLogDropped returns the data-plane log lines dropped on a full ring.
func dpLogDropped(level int) uint64 {
	return uint64(C.loxilb_log_dropped(C.int(level)))
}
