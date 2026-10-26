/*
 * Copyright (c) 2026 NetLOX Inc
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
 */
package restapi

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBFDSpecAvailabilityAndDeleteContract(t *testing.T) {
	type response struct {
		Description string `json:"description" yaml:"description"`
	}
	type operation struct {
		Description string              `json:"description" yaml:"description"`
		Responses   map[string]response `json:"responses" yaml:"responses"`
	}
	type spec struct {
		Paths map[string]map[string]operation `json:"paths" yaml:"paths"`
	}
	raw, err := os.ReadFile("../swagger.yml")
	if err != nil {
		t.Fatal(err)
	}
	var source spec
	if err := yaml.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}
	variants := map[string]spec{"source": source}
	for name, raw := range map[string]json.RawMessage{"SwaggerJSON": SwaggerJSON, "FlatSwaggerJSON": FlatSwaggerJSON} {
		var doc spec
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		variants[name] = doc
	}
	for name, doc := range variants {
		t.Run(name, func(t *testing.T) {
			for path, verb := range map[string]string{"/config/bfd/all": "get", "/config/bfd": "post", "/config/bfd/remoteIP/{remote_ip}": "delete"} {
				op := doc.Paths[path][verb]
				expected := source.Paths[path][verb]
				if op.Description != expected.Description {
					t.Errorf("%s operation description drift", path)
				}
				for code, phrase := range map[string]string{"400": "BGP-only mode", "503": "BFD subsystem unavailable"} {
					response, ok := op.Responses[code]
					if !ok || !strings.Contains(response.Description, phrase) {
						t.Errorf("%s missing %s contract: %+v", path, code, response)
					}
					if response.Description != expected.Responses[code].Description {
						t.Errorf("%s response %s drift", path, code)
					}
				}
				if !strings.Contains(op.Description, "BGP-only mode returns 400") {
					t.Errorf("%s must identify mode refusal as 400", path)
				}
			}
			desc := doc.Paths["/config/bfd/remoteIP/{remote_ip}"]["delete"].Description
			if strings.Contains(desc, "even if other sessions remain") || !strings.Contains(desc, "only after the last session is removed") {
				t.Error("DELETE running hint contract is stale")
			}
		})
	}
}
