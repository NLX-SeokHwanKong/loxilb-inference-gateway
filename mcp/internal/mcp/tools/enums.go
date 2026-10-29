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
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// The swagger-derived tables (swaggerProps, swaggerEnum, swaggerEnumDoc,
// swaggerExcludedField) live in zz_swagger_enums_gen.go and are produced
// from api/swagger.yml and mcp/contract/fields.yml. CI regenerates them and
// fails on a difference, so the bridge cannot drift from the REST contract
// for the properties these tables cover.
//
//go:generate go run ../../../contract/gen-enums -api ../../../../api -fields ../../../contract/fields.yml -out zz_swagger_enums_gen.go

// enumAllows reports whether v is one of the permitted values the swagger
// contract lists for model.property. A property without a generated enum
// allows everything (the gateway validates it).
func enumAllows(key, v string) bool {
	vals, ok := swaggerEnum[key]
	if !ok {
		return true
	}
	for _, x := range vals {
		if x == v {
			return true
		}
	}
	return false
}

// enumValues returns the permitted values of model.property joined for an
// error message or a description.
func enumValues(key string) string {
	return strings.Join(swaggerEnum[key], ", ")
}

// enumDescription returns the swagger description of model.property
// prefixed with its permitted values, for a tool argument's schema.
func enumDescription(key string) string {
	doc := swaggerEnumDoc[key]
	if vals := swaggerEnum[key]; len(vals) > 0 {
		return "one of " + strings.Join(vals, ", ") + ". " + doc
	}
	return doc
}

// lbCreateInputSchema derives lb_create's input schema from lbCreateIn and
// replaces the descriptions of the enumerated serviceArguments properties
// with the generated swagger text (sel, mode, api_key_auth, fc_mode,
// eppFailureMode, ai_default_reservation, security), so the model reads
// the REST contract's own words and permitted values. The handler
// validates the same values; the string arguments carry the enum so an
// invalid value is refused before the handler runs.
func lbCreateInputSchema() *jsonschema.Schema {
	return inputSchemaWithEnums[lbCreateIn]("lb_create", map[string]string{
		"sel":                    "serviceArguments.sel",
		"mode":                   "serviceArguments.mode",
		"api_key_auth":           "serviceArguments.api_key_auth",
		"fc_mode":                "serviceArguments.fc_mode",
		"epp_failure_mode":       "serviceArguments.eppFailureMode",
		"ai_default_reservation": "serviceArguments.ai_default_reservation",
		"security":               "serviceArguments.security",
	})
}

// inputSchemaWithEnums derives a tool's input schema from its input struct
// and, for each argument named in enums (argument -> model.property),
// replaces the description with the generated swagger text; a string
// argument also carries the permitted values as an enum, so an unknown
// value is refused before the handler runs.
func inputSchemaWithEnums[T any](tool string, enums map[string]string) *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Errorf("%s input schema: %w", tool, err))
	}
	args := make([]string, 0, len(enums))
	for arg := range enums {
		args = append(args, arg)
	}
	sort.Strings(args)
	for _, arg := range args {
		key := enums[arg]
		p, ok := s.Properties[arg]
		if !ok {
			panic(fmt.Errorf("%s input schema: no property %q", tool, arg))
		}
		if doc := enumDescription(key); doc != "" {
			p.Description = doc
		}
		if p.Type == "string" {
			for _, v := range swaggerEnum[key] {
				p.Enum = append(p.Enum, v)
			}
		}
	}
	return s
}

// intInEnum reports whether an integer argument is one of the generated
// permitted values of model.property.
func intInEnum(key string, v int) bool {
	return enumAllows(key, strconv.Itoa(v))
}

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
