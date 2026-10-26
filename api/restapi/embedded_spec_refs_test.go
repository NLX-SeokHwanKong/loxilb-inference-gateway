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

package restapi

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// Every local reference in the embedded specs names something the same
// document defines. The server builds its router from the flattened
// document at start-up and panics on a reference it cannot resolve, so a
// spec that fails this does not serve a single request: the process never
// gets as far as listening.
func TestEmbeddedSpecReferencesResolve(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{"SwaggerJSON": SwaggerJSON, "FlatSwaggerJSON": FlatSwaggerJSON} {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		missing := map[string]bool{}
		refs := 0
		var walk func(v any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if ref, ok := x["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
					refs++
					var at any = doc
					for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
						part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
						m, isMap := at.(map[string]any)
						if !isMap {
							at = nil
							break
						}
						at = m[part]
					}
					if at == nil {
						missing[ref] = true
					}
				}
				for _, val := range x {
					walk(val)
				}
			case []any:
				for _, val := range x {
					walk(val)
				}
			}
		}
		walk(doc)
		if refs == 0 {
			t.Errorf("%s: no reference found; the walk is not reading the document", name)
		}
		if len(missing) > 0 {
			names := make([]string, 0, len(missing))
			for ref := range missing {
				names = append(names, ref)
			}
			sort.Strings(names)
			t.Errorf("%s: %d reference(s) to something the document does not define: %s",
				name, len(names), strings.Join(names, ", "))
		}
	}
}
