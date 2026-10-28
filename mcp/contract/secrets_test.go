/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package contract

import (
	"strings"
	"testing"
)

// secretTag reports why a json field name is refused by the secrets policy
// (campaign rule 14): no tool argument or result carries key material or a
// raw credential. Names are compared by underscore-separated segment so
// "tokens_per_min" (segment "tokens") passes while "token" fails.
// "api_key" is refused on inputs only (an import argument); results never
// carry it either, but the create tool's raw key goes to a file by design
// and is named raw_key.
func secretTag(name string, input bool) string {
	if _, ok := secretAllowlist[name]; ok {
		return ""
	}
	segs := strings.Split(name, "_")
	for i, s := range segs {
		switch s {
		case "pem", "secret", "password":
			return "carries secret material (" + s + ")"
		case "token":
			return "carries a credential (token)"
		case "key":
			if i > 0 && (segs[i-1] == "private" || (input && segs[i-1] == "api")) {
				return "carries key material (" + segs[i-1] + "_key)"
			}
		}
	}
	return ""
}

// secretAllowlist names fields whose name matches the denylist but whose
// content is not a secret. Each needs a reason a reviewer can check.
var secretAllowlist = map[string]string{
	"confirm_token": "the single-use preview token of the confirm flow; bound to the arguments, not a credential",
	"api_key_auth":  "an enum selecting the data-plane X-Api-Key policy; never key material",
}

func TestSecretTagClassifier(t *testing.T) {
	for name, input := range map[string]bool{
		"pem": true, "cert_pem": false, "private_key": true, "api_key": true,
		"client_secret": false, "secret": true, "bearer_token": false, "token": true, "password": true,
	} {
		if secretTag(name, input) == "" {
			t.Errorf("%q (input=%v) must be refused", name, input)
		}
	}
	for name, input := range map[string]bool{
		"confirm_token": true, "api_key_auth": true, "tokens_per_min": true, "key_id": true,
		"key_file": false, "raw_key": false, "api_key": false, "cert_file": true,
	} {
		if why := secretTag(name, input); why != "" {
			t.Errorf("%q (input=%v) must pass, got %q", name, input, why)
		}
	}
}

func TestNoToolFieldCarriesASecret(t *testing.T) {
	tags, err := ScanJSONTags(toolsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) < 100 {
		t.Fatalf("only %d json tags found; the scanner lost its target", len(tags))
	}
	for _, tg := range tags {
		if why := secretTag(tg.JSON, tg.Input); why != "" {
			t.Errorf("%s:%d: %s.%s json:%q %s", tg.File, tg.Line, tg.Struct, tg.Field, tg.JSON, why)
		}
	}
}
