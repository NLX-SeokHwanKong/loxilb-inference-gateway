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

func TestProcessSpecNativeCollectionContract(t *testing.T) {
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
	expected := source.Paths["/status/process"]["get"]
	for name, doc := range variants {
		t.Run(name, func(t *testing.T) {
			op := doc.Paths["/status/process"]["get"]
			if op.Description != expected.Description || op.Responses["500"] != expected.Responses["500"] {
				t.Error("process contract drift")
			}
			for _, phrase := range []string{"without top or ps", "process lifetime", "decimal KiB", "15 bytes", "first matching passwd", "PID records are skipped", "returns 500"} {
				if !strings.Contains(op.Description, phrase) {
					t.Errorf("missing process contract %q", phrase)
				}
			}
			if strings.Contains(op.Description, "assembled from top") {
				t.Error("stale command-parser contract")
			}
		})
	}
}
