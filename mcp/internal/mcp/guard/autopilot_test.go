/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package guard

import "testing"

func TestAutopilotAllowedExactName(t *testing.T) {
	pol := &Policy{Autopilot: []string{"lb_delete", " fw_delete "}}
	if !pol.AutopilotAllowed("lb_delete") || !pol.AutopilotAllowed("fw_delete") {
		t.Error("listed tool must be autopilot-eligible")
	}
	if pol.AutopilotAllowed("lb_create") {
		t.Error("unlisted tool must not be autopilot-eligible")
	}
	if (*Policy)(nil).AutopilotAllowed("lb_delete") {
		t.Error("nil policy has no autopilot tools")
	}
}

func TestNeverAutopilotOverridesConfig(t *testing.T) {
	names := []string{"maintenance_set", "config_import", "gpu_mode_set", "ai_objective_set"}
	pol := &Policy{Autopilot: append([]string{"lb_delete"}, names...)}
	for _, n := range names {
		if _, ok := NeverAutopilot[n]; !ok {
			t.Errorf("%s missing from NeverAutopilot", n)
		}
		if pol.AutopilotAllowed(n) {
			t.Errorf("AutopilotAllowed(%s) = true although it is in NeverAutopilot", n)
		}
	}
	if !pol.AutopilotAllowed("lb_delete") {
		t.Error("a listed tool outside NeverAutopilot must stay eligible")
	}
}

// Every name the campaign guide's change-gate requirement 6 lists.
func TestNeverAutopilotCoversPlannedTools(t *testing.T) {
	for _, n := range []string{
		"maintenance_set", "halfclose_release", "lb_sockmap_reset",
		"ai_netidentity_set", "ai_netidentity_delete",
		"ai_jwtprofile_set", "ai_jwtprofile_delete",
		"ai_ratelimit_defaults_set", "ai_ratelimit_defaults_delete",
		"ai_objective_set", "ai_objective_delete",
		"trace_otlp_set", "opa_watcher_set", "opa_watcher_delete", "config_import",
	} {
		if _, ok := NeverAutopilot[n]; !ok {
			t.Errorf("%s missing from NeverAutopilot", n)
		}
	}
}
