/*
 * Copyright (c) 2025 LoxiLB Authors
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

import (
	"errors"
	"testing"

	"github.com/loxilb-io/loxilb/pkg/pgstore"
)

// The snapshot carries every plane from the start, so a first 504 shows in
// increase(), and follows the store packages' counters.
func TestAIStoreWriteOutcomeSnapshotSeedsEveryPlane(t *testing.T) {
	snap := aiStoreWriteOutcomeSnapshot()
	for _, p := range pgstore.Planes {
		if _, ok := snap.UnknownByPlane[string(p)]; !ok {
			t.Fatalf("plane %s missing from %v", p, snap.UnknownByPlane)
		}
	}
	before := snap.UnknownByPlane["aikey"]
	_ = pgstore.ClassifyWriteErrFor(pgstore.PlaneAIKey, errors.New("unanswered after send"))
	if after := aiStoreWriteOutcomeSnapshot().UnknownByPlane["aikey"]; after != before+1 {
		t.Fatalf("aikey %d -> %d, want +1", before, after)
	}
}
