/*
 * Copyright (c) 2026 NetLOX Inc
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
package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loxilb-io/loxilb/api/restapi/operations"
	cmn "github.com/loxilb-io/loxilb/common"
)

// TestVersionAdvertisesFeatures: a controller decides whether to send
// api_key_auth / jwt_auth_profile on a rule by this list (a build without the
// token would drop the field and install the rule unprotected), so the list
// must be on the wire under "features", name both fields, and carry no
// duplicate a set-based reader could trip on.
func TestVersionAdvertisesFeatures(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/netlox/v1/version", nil)
	code, body := renderResponder(t, ConfigGetVersion(operations.GetVersionParams{HTTPRequest: req}))
	if code != http.StatusOK {
		t.Fatalf("GET /version: status %d, body %s", code, body)
	}
	var got struct {
		Product  string   `json:"product"`
		Features []string `json:"features"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("GET /version: not JSON: %v (%s)", err, body)
	}
	if got.Product != cmn.Product {
		t.Errorf("product = %q, want %q", got.Product, cmn.Product)
	}
	seen := map[string]int{}
	for _, f := range got.Features {
		seen[f]++
	}
	for _, want := range []string{cmn.FeatureAPIKeyAuth, cmn.FeatureJWTAuthProfile} {
		switch seen[want] {
		case 0:
			t.Errorf("features %v lack %q", got.Features, want)
		case 1:
		default:
			t.Errorf("features %v list %q %d times", got.Features, want, seen[want])
		}
	}
}
