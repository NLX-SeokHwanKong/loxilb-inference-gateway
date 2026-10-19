/* SPDX-License-Identifier: Apache-2.0
 * Copyright (c) 2026 NetLOX Inc.
 */
package loxinlp

import "testing"

// The filter must answer without a netlink handle (--nonlp) and apply the
// configured lists the same way in both modes.
func TestIntfFilterWithoutNetlink(t *testing.T) {
	prevNl, prevBL, prevWL := nNl, intfBlackList, intfWhiteList
	t.Cleanup(func() { nNl, intfBlackList, intfWhiteList = prevNl, prevBL, prevWL })
	nNl = nil

	NlpSetIntfFilter("none", "none")
	for name, want := range map[string]bool{"lo": true, "eth0": false, "llb-rule1": false} {
		if got := NlpIsBlackListedIntf(name, 0); got != want {
			t.Errorf("defaults: %q excluded=%v, want %v", name, got, want)
		}
	}

	NlpSetIntfFilter("^docker", "none")
	for name, want := range map[string]bool{"lo": true, "eth0": false, "docker0": true} {
		if got := NlpIsBlackListedIntf(name, 0); got != want {
			t.Errorf("blacklist: %q excluded=%v, want %v", name, got, want)
		}
	}

	NlpSetIntfFilter("^docker", "^eth0$")
	for name, want := range map[string]bool{"eth0": false, "eth1": true, "lo": true} {
		if got := NlpIsBlackListedIntf(name, 0); got != want {
			t.Errorf("whitelist: %q excluded=%v, want %v", name, got, want)
		}
	}
}
