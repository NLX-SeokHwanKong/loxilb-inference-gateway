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

package tools

import (
	"fmt"
	"sort"
	"strings"
)

// The swagger-derived tables (swaggerProps, swaggerEnum, swaggerEnumDoc,
// swaggerExcludedField) live in zz_swagger_enums_gen.go and are produced
// from api/swagger.yml and mcp/contract/fields.yml. CI regenerates them and
// fails on a difference, so the bridge cannot drift from the REST contract
// for the properties these tables cover.
//
//go:generate go run ../../../contract/gen-enums -api ../../../../api -fields ../../../contract/fields.yml -out zz_swagger_enums_gen.go

// readBackSuffix marks serviceArguments properties the data plane resolves
// and the API returns on GET; a client never sends them.
const readBackSuffix = "_effective"

// validateServiceExtra refuses a service_extra key before any request is
// built when it is not a serviceArguments property, when it is a read-back
// value, or when fields.yml excludes it. Keys are checked in sorted order
// so the first refusal is deterministic.
func validateServiceExtra(extra map[string]any) error {
	if len(extra) == 0 {
		return nil
	}
	known := make(map[string]bool, len(swaggerProps["serviceArguments"]))
	for _, p := range swaggerProps["serviceArguments"] {
		known[p] = true
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case !known[k]:
			return fmt.Errorf("service_extra: %q is not a serviceArguments property of the REST contract", k)
		case strings.HasSuffix(k, readBackSuffix):
			return fmt.Errorf("service_extra: %q is read-back only (resolved by the data plane, returned on GET); it cannot be set", k)
		}
		if reason, excluded := swaggerExcludedField["serviceArguments."+k]; excluded {
			return fmt.Errorf("service_extra: %q is excluded from the bridge: %s", k, reason)
		}
	}
	return nil
}
