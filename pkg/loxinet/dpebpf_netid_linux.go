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
#include <stdint.h>
#include <stdlib.h>

// The bound set of the admission gate, as sockproxy_ai_admit.h declares it.
// That header is not included: it also declares the gate's exports, which
// this package defines.
void ai_gw_netid_bound_set(const char *const *svc_idents, const uint8_t *flags,
                           int n, int all);
int  ai_gw_netid_bound(const char *svc_ident);
*/
import "C"

import (
	"sort"
	"unsafe"
)

// netidBoundPublish hands the bound set to the data plane, which replaces
// the one it had whole.
func netidBoundPublish(nb netidBound) {
	all := C.int(0)
	if nb.all {
		all = netidBoundBit
		if nb.allClosed {
			all |= netidFailClosedBit
		}
	}
	n := len(nb.svcs)
	if n == 0 {
		C.ai_gw_netid_bound_set(nil, nil, 0, all)
		return
	}
	names := make([]string, 0, n)
	for svc := range nb.svcs {
		names = append(names, svc)
	}
	sort.Strings(names)

	ptrs := (**C.char)(C.calloc(C.size_t(n), C.size_t(unsafe.Sizeof((*C.char)(nil)))))
	flags := (*C.uint8_t)(C.calloc(C.size_t(n), 1))
	if ptrs == nil || flags == nil {
		// Nothing to build the set from. Bind every service instead: the
		// resolver is asked more often and no binding is skipped. A service
		// that fails closed keeps doing so only if all do.
		C.free(unsafe.Pointer(ptrs))
		C.free(unsafe.Pointer(flags))
		C.ai_gw_netid_bound_set(nil, nil, 0, netidBoundBit)
		return
	}
	pv := unsafe.Slice(ptrs, n)
	fv := unsafe.Slice(flags, n)
	for i, svc := range names {
		pv[i] = C.CString(svc)
		if nb.svcs[svc] {
			fv[i] = netidFailClosedBit
		}
	}
	C.ai_gw_netid_bound_set(ptrs, flags, C.int(n), all)
	for i := range pv {
		C.free(unsafe.Pointer(pv[i]))
	}
	C.free(unsafe.Pointer(ptrs))
	C.free(unsafe.Pointer(flags))
}

// netidBoundRead is the data plane's answer for one service, as a request
// on it would read it: the bound and fail-closed bits.
func netidBoundRead(svc string) int {
	cs := C.CString(svc)
	defer C.free(unsafe.Pointer(cs))
	return int(C.ai_gw_netid_bound(cs))
}
