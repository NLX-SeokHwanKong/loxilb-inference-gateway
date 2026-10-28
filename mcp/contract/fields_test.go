/*
 * Copyright (c) 2026 NetLOX Inc
 * SPDX-License-Identifier: Apache-2.0
 */

package contract

import (
	"fmt"
	"strings"
	"testing"
)

func TestFieldsShapeIsEnforced(t *testing.T) {
	for i, f := range []Field{
		{Label: "weird"},
		{Label: LabelTyped},              // no arg
		{Label: LabelExcluded},           // no reason
		{Label: LabelPending},            // no phase
		{Label: LabelReadBack, Arg: "x"}, // arg on a non-typed field
		{Label: LabelPending, Phase: "P1a", Wire: "X"},
	} {
		if err := f.check(); err == nil {
			t.Errorf("case %d accepted: %+v", i, f)
		}
	}
	for i, f := range []Field{
		{Label: LabelTyped, Arg: "x"},
		{Label: LabelTyped, Arg: "x", Wire: "X"},
		{Label: LabelExtraOnly},
		{Label: LabelReadBack},
		{Label: LabelExcluded, Reason: "r"},
		{Label: LabelPending, Phase: "P2"},
	} {
		if err := f.check(); err != nil {
			t.Errorf("case %d refused: %v", i, err)
		}
	}
}

// Every swagger property of every model fields.yml covers carries a label;
// a typed property has a tool argument with that json tag and the wire key
// appears in the tools source; a changed description is a warning.
func TestFieldsLabelEverySwaggerProperty(t *testing.T) {
	spec := loadSpec(t)
	fields, err := LoadFields(fieldsPath)
	if err != nil {
		t.Fatalf("load %s: %v", fieldsPath, err)
	}
	tags, err := ScanJSONTags(toolsDir)
	if err != nil {
		t.Fatal(err)
	}
	inputArgs := map[string]bool{}
	for _, tg := range tags {
		if tg.Input {
			inputArgs[tg.JSON] = true
		}
	}
	literals, err := ScanStringLiterals(toolsDir)
	if err != nil {
		t.Fatal(err)
	}

	var drifted []string
	counts := map[string]map[string]int{}
	for _, mname := range fields.ModelNames() {
		model := fields.Models[mname]
		props, ok := spec.Model(mname)
		if !ok {
			t.Errorf("fields.yml model %s does not exist in swagger", mname)
			continue
		}
		counts[mname] = map[string]int{}
		inSwagger := map[string]Prop{}
		for _, p := range props {
			inSwagger[p.Name] = p
			fl, labelled := model.Fields[p.Name]
			if !labelled {
				t.Errorf("%s.%s: unlabeled swagger property - add it to fields.yml", mname, p.Name)
				continue
			}
			counts[mname][fl.Label]++
			if fl.Label == LabelTyped {
				if !inputArgs[fl.Arg] {
					t.Errorf("%s.%s: typed with arg %q but no tool input has that json tag", mname, p.Name, fl.Arg)
				}
				if !literals[fl.WireName(p.Name)] {
					t.Errorf("%s.%s: typed but the tools source never sends %q", mname, p.Name, fl.WireName(p.Name))
				}
			}
			if fl.DescSHA256 != "" && fl.DescSHA256 != DescHash(p.Description) {
				drifted = append(drifted, fmt.Sprintf("%s.%s (%s)", mname, p.Name, fl.Label))
			}
		}
		for fname := range model.Fields {
			if _, ok := inSwagger[fname]; !ok {
				t.Errorf("%s.%s: labelled in fields.yml but not a swagger property any more", mname, fname)
			}
		}
	}
	var rows []string
	for _, mname := range sortedKeys(counts) {
		for _, label := range sortedKeys(counts[mname]) {
			rows = append(rows, fmt.Sprintf("| %s | %s | %d |", mname, label, counts[mname][label]))
		}
	}
	summary(t, "### Field contract\n\n| model | label | fields |\n|---|---|---|\n%s\n", strings.Join(rows, "\n"))
	if len(drifted) > 0 {
		summary(t, "> **WARNING** swagger description changed for %d labelled field(s); re-read them and refresh desc_sha256: %s",
			len(drifted), strings.Join(drifted, ", "))
	}
}
