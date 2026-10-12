/*
 * Copyright (c) 2026 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package common

import "testing"

func TestAIDefaultReservationRuleMapping(t *testing.T) {
	for word, want := range map[string]uint8{
		"":        AIDefaultReservationRuleUnset,
		"inherit": AIDefaultReservationRuleUnset,
		"off":     AIDefaultReservationRuleOff,
		"on":      AIDefaultReservationRuleOn,
	} {
		got, err := AIDefaultReservationToRule(word)
		if err != nil || got != want {
			t.Errorf("AIDefaultReservationToRule(%q) = %d, %v; want %d", word, got, err, want)
		}
	}
	for _, word := range []string{"yes", "ON", "true", "1"} {
		if _, err := AIDefaultReservationToRule(word); err == nil {
			t.Errorf("AIDefaultReservationToRule(%q) was accepted", word)
		}
	}
	// The read-back omits unset, so a rule that declared nothing reads
	// back as it was written.
	for mode, want := range map[uint8]string{
		AIDefaultReservationRuleUnset: "",
		AIDefaultReservationRuleOff:   "off",
		AIDefaultReservationRuleOn:    "on",
		9:                             "",
	} {
		if got := AIDefaultReservationFromRule(mode); got != want {
			t.Errorf("AIDefaultReservationFromRule(%d) = %q, want %q", mode, got, want)
		}
	}
}

func TestAIDefaultMaxTokensBound(t *testing.T) {
	for _, v := range []uint32{0, 1, AIDefaultMaxTokensBuiltin, AIDefaultMaxTokensMax} {
		if err := AIDefaultMaxTokensError(v); err != nil {
			t.Errorf("AIDefaultMaxTokensError(%d) = %v, want accepted", v, err)
		}
	}
	if err := AIDefaultMaxTokensError(AIDefaultMaxTokensMax + 1); err == nil {
		t.Error("a value above the bound was accepted")
	}
}
