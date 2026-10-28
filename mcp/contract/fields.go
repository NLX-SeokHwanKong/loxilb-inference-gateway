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

package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Field labels of fields.yml.
const (
	LabelTyped     = "typed"          // a typed tool argument sends the property
	LabelExtraOnly = "extra-only"     // reachable only through service_extra
	LabelReadBack  = "read-back"      // returned on GET, never sent
	LabelExcluded  = "excluded-field" // never sent, with a reason
	LabelPending   = "pending"        // not typed yet; phase names the PR
)

var labels = map[string]bool{
	LabelTyped: true, LabelExtraOnly: true, LabelReadBack: true, LabelExcluded: true, LabelPending: true,
}

// Field is one labelled swagger property.
type Field struct {
	Label      string `yaml:"label"`
	Arg        string `yaml:"arg,omitempty"`    // typed: the tool argument (json tag)
	Wire       string `yaml:"wire,omitempty"`   // typed: body key sent when it differs from the property name
	Reason     string `yaml:"reason,omitempty"` // excluded-field
	Phase      string `yaml:"phase,omitempty"`  // pending
	DescSHA256 string `yaml:"desc_sha256,omitempty"`
}

// Model is one request model of fields.yml.
type Model struct {
	Source string           `yaml:"source"`
	Fields map[string]Field `yaml:"fields"`
}

// Fields is fields.yml.
type Fields struct {
	Version int              `yaml:"version"`
	Models  map[string]Model `yaml:"models"`
}

// LoadFields parses fields.yml and checks each entry's shape.
func LoadFields(path string) (*Fields, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Fields
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, err
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("version %d: want 1", f.Version)
	}
	for mname, m := range f.Models {
		for fname, fl := range m.Fields {
			if err := fl.check(); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", mname, fname, err)
			}
		}
	}
	return &f, nil
}

func (f Field) check() error {
	if !labels[f.Label] {
		return fmt.Errorf("label %q is not one of typed|extra-only|read-back|excluded-field|pending", f.Label)
	}
	switch f.Label {
	case LabelTyped:
		if f.Arg == "" {
			return fmt.Errorf("a typed field needs arg:")
		}
	case LabelExcluded:
		if f.Reason == "" {
			return fmt.Errorf("an excluded-field needs reason:")
		}
	case LabelPending:
		if !Phases[f.Phase] {
			return fmt.Errorf("pending needs phase: one of P0|P1a|P1b|P2|P3|P4")
		}
	}
	if f.Label != LabelTyped && (f.Arg != "" || f.Wire != "") {
		return fmt.Errorf("arg: and wire: belong to typed fields only")
	}
	return nil
}

// WireName is the body key the bridge sends for a typed property.
func (f Field) WireName(prop string) string {
	if f.Wire != "" {
		return f.Wire
	}
	return prop
}

// ModelNames returns the models sorted.
func (f *Fields) ModelNames() []string {
	out := make([]string, 0, len(f.Models))
	for n := range f.Models {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Excluded returns model -> property -> reason for every excluded-field.
func (f *Fields) Excluded() map[string]map[string]string {
	out := map[string]map[string]string{}
	for mname, m := range f.Models {
		for fname, fl := range m.Fields {
			if fl.Label == LabelExcluded {
				if out[mname] == nil {
					out[mname] = map[string]string{}
				}
				out[mname][fname] = fl.Reason
			}
		}
	}
	return out
}

// DescHash is the fingerprint stored in desc_sha256.
func DescHash(desc string) string {
	sum := sha256.Sum256([]byte(desc))
	return hex.EncodeToString(sum[:])
}
