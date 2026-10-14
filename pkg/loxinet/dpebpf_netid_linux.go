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
void ai_gw_netid_ifmatch_set(int on);
int  ai_gw_netid_ifmatch(void);
void ai_gw_class_on_set(int on);
int  ai_gw_class_on(void);
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
	// The services to name: each one bound by name, and each one that
	// forwards while the others do not.
	names := make([]string, 0, len(nb.svcs)+len(nb.fwd))
	for svc := range nb.svcs {
		names = append(names, svc)
	}
	for svc := range nb.fwd {
		if _, listed := nb.svcs[svc]; !listed {
			names = append(names, svc)
		}
	}
	n := len(names)
	if n == 0 {
		C.ai_gw_netid_bound_set(nil, nil, 0, all)
		return
	}
	sort.Strings(names)

	ptrs := (**C.char)(C.calloc(C.size_t(n), C.size_t(unsafe.Sizeof((*C.char)(nil)))))
	flags := (*C.uint8_t)(C.calloc(C.size_t(n), 1))
	if ptrs == nil || flags == nil {
		// Nothing to build the set from. Bind every service instead: the
		// resolver is asked more often and no binding is skipped. A service
		// that fails closed keeps doing so only if all do; one that forwards
		// identity must keep dropping what a client sends, so all do.
		C.free(unsafe.Pointer(ptrs))
		C.free(unsafe.Pointer(flags))
		fallback := C.int(netidBoundBit)
		if len(nb.fwd) > 0 {
			fallback |= netidFwdBit
		}
		C.ai_gw_netid_bound_set(nil, nil, 0, fallback)
		return
	}
	pv := unsafe.Slice(ptrs, n)
	fv := unsafe.Slice(flags, n)
	for i, svc := range names {
		pv[i] = C.CString(svc)
		if nb.svcs[svc] {
			fv[i] = netidFailClosedBit
		}
		if nb.fwd[svc] {
			fv[i] |= netidFwdBit
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

// netidIfMatchPublish tells the data plane whether any binding matches on an
// interface.
func netidIfMatchPublish(on bool) {
	v := C.int(0)
	if on {
		v = 1
	}
	C.ai_gw_netid_ifmatch_set(v)
}

// netidIfMatchRead is the switch as the accept path reads it.
func netidIfMatchRead() bool {
	return C.ai_gw_netid_ifmatch() != 0
}

// aiClassOnPublish tells the data plane whether any class is configured.
func aiClassOnPublish(on bool) {
	v := C.int(0)
	if on {
		v = 1
	}
	C.ai_gw_class_on_set(v)
}

// aiClassOnRead reads the switch back from the data plane.
func aiClassOnRead() bool { return C.ai_gw_class_on() != 0 }
