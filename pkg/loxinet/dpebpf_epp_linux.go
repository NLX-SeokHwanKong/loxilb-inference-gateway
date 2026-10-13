/*
 * Copyright (c) 2026 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 */
package loxinet

/*
#include <stdint.h>
// sockproxy_http.c: the Endpoint Picker words of the pool(s) a rule
// installed, by the rule number proxy_add_entry stamped as epp_svc_id.
extern int proxy_epp_cfg_get(uint32_t svc_id, uint8_t *mode, uint32_t *timeout_ms);
// sockproxy_http.c: the bounded-load units CHWBL/WRR_HASH hold on one
// endpoint of a rule (-1: unknown rule, or a rule without such units).
extern int proxy_get_chwbl_ep_load(uint32_t id, int epid);
*/
import "C"

// DpEppCfgGet reports what the data plane holds for a rule's Endpoint
// Picker: the number of pools carrying the rule number as epp_svc_id and,
// for the first of them, the mode word (eppDpMode*) and the request-phase
// deadline. A diagnostic read beside the configuration, never replayed
// into a rule: tests and status readers use it to confirm the C pool
// received what the rule stores.
func DpEppCfgGet(ruleNum uint32) (pools int, mode uint8, timeoutMs uint32) {
	var cMode C.uint8_t
	var cMs C.uint32_t
	pools = int(C.proxy_epp_cfg_get(C.uint32_t(ruleNum), &cMode, &cMs))
	return pools, uint8(cMode), uint32(cMs)
}

// DpChwblEpLoad reports the bounded-load units CHWBL/WRR_HASH currently
// record on endpoint `ep` (its index in the rule) of rule `ruleNum`, or -1
// for a rule the data plane does not know or one that keeps no such units.
// A diagnostic read: tests use it to check that every path that pins an
// endpoint outside the selector (the Endpoint Picker, mid-cycle failover)
// charges the unit teardown will give back.
func DpChwblEpLoad(ruleNum uint32, ep int) int {
	return int(C.proxy_get_chwbl_ep_load(C.uint32_t(ruleNum), C.int(ep)))
}
