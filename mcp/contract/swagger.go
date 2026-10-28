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

// Package contract holds the bridge <-> REST API contract: the operation
// manifest (coverage.yml), the field contract (fields.yml), the swagger
// loader they are checked against, and the generator of the enum / property
// tables the tools package compiles in. The tests in this package are the
// CI contract gate; they skip, not fail, when the gateway's api/ directory is
// not beside the mcp/ module (tarball and go install builds).
package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SwaggerFiles are the specification files the contract covers, relative to
// the gateway's api/ directory. Operations present in both are de-duplicated.
var SwaggerFiles = []string{"swagger.yml", "swagger-extras.yml"}

// Op is one REST operation: upper-case method and the path with its
// {param} names verbatim and the basePath stripped.
type Op struct {
	Method  string
	Path    string
	Summary string
}

// Key is the manifest spelling of an operation: "METHOD /path".
func (o Op) Key() string { return o.Method + " " + o.Path }

// Prop is one property of a swagger request model.
type Prop struct {
	Name        string
	Type        string
	Description string
	Enum        []string // enum values rendered as strings; nil when not enumerated
}

// Spec is the union of the swagger files.
type Spec struct {
	// BasePath is the prefix every operation path is served under.
	BasePath string
	ops      map[string]Op
	models   map[string][]Prop // request model name -> properties in swagger order
}

// FindAPIDir locates the gateway's api/ directory from the mcp/contract
// package directory. It returns "" when the directory is absent, which the
// tests translate into a skip.
func FindAPIDir() string {
	for _, cand := range []string{"../../api", "../api"} {
		if st, err := os.Stat(filepath.Join(cand, SwaggerFiles[0])); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

var httpMethods = map[string]bool{
	"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true,
}

// LoadSwagger reads every file of SwaggerFiles under dir.
func LoadSwagger(dir string) (*Spec, error) {
	s := &Spec{ops: map[string]Op{}, models: map[string][]Prop{}}
	for _, f := range SwaggerFiles {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		var doc struct {
			BasePath    string                          `yaml:"basePath"`
			Paths       map[string]map[string]yaml.Node `yaml:"paths"`
			Definitions map[string]map[string]any       `yaml:"definitions"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for p, item := range doc.Paths {
			if s.BasePath == "" {
				s.BasePath = doc.BasePath
			}
			p = strings.TrimPrefix(p, doc.BasePath)
			for m, node := range item {
				lm := strings.ToLower(m)
				if !httpMethods[lm] {
					continue
				}
				op := Op{Method: strings.ToUpper(lm), Path: p}
				var body struct {
					Summary string `yaml:"summary"`
				}
				_ = node.Decode(&body)
				op.Summary = body.Summary
				if _, seen := s.ops[op.Key()]; !seen {
					s.ops[op.Key()] = op
				}
			}
		}
		if err := s.collectModels(doc.Definitions); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	return s, nil
}

// collectModels records the request models the bridge builds. Two of them
// are inline sub-objects of LoadbalanceEntry rather than named definitions.
func (s *Spec) collectModels(defs map[string]map[string]any) error {
	add := func(name string, props any) {
		if _, seen := s.models[name]; seen || props == nil {
			return
		}
		s.models[name] = parseProps(props)
	}
	for name, def := range defs {
		add(name, def["properties"])
	}
	if lb, ok := defs["LoadbalanceEntry"]; ok {
		props, _ := lb["properties"].(map[string]any)
		if sa, ok := props["serviceArguments"].(map[string]any); ok {
			add("serviceArguments", sa["properties"])
		}
		if eps, ok := props["endpoints"].(map[string]any); ok {
			if items, ok := eps["items"].(map[string]any); ok {
				add("endpoints", items["properties"])
			}
		}
	}
	return nil
}

func parseProps(v any) []Prop {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Prop, 0, len(names))
	for _, n := range names {
		pm, _ := m[n].(map[string]any)
		p := Prop{Name: n}
		p.Type, _ = pm["type"].(string)
		p.Description, _ = pm["description"].(string)
		if raw, ok := pm["enum"].([]any); ok {
			for _, e := range raw {
				p.Enum = append(p.Enum, fmt.Sprint(e))
			}
		}
		out = append(out, p)
	}
	return out
}

// Ops returns every operation sorted by path then method.
func (s *Spec) Ops() []Op {
	out := make([]Op, 0, len(s.ops))
	for _, o := range s.ops {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// Has reports whether "METHOD /path" is an operation of the spec.
func (s *Spec) Has(key string) bool { _, ok := s.ops[key]; return ok }

// Model returns the properties of a request model ("serviceArguments",
// "endpoints", or a definitions name) and whether it exists.
func (s *Spec) Model(name string) ([]Prop, bool) {
	p, ok := s.models[name]
	return p, ok
}

// PropNames returns the property names of a model.
func (s *Spec) PropNames(name string) []string {
	props, _ := s.Model(name)
	out := make([]string, 0, len(props))
	for _, p := range props {
		out = append(out, p.Name)
	}
	return out
}

// Match returns the operations whose method equals method and whose path
// matches the literal pattern: segments are compared one by one, a swagger
// {param} segment matches anything, and a pattern segment holding a format
// verb (%s, %d, %v) or an empty trailing segment matches anything as well.
// method "" matches every method.
func (s *Spec) Match(method, pattern string) []Op {
	want := splitPath(pattern)
	var out []Op
	for _, o := range s.Ops() {
		if method != "" && o.Method != method {
			continue
		}
		got := splitPath(o.Path)
		if len(got) != len(want) {
			continue
		}
		ok := true
		for i := range got {
			if got[i] == want[i] || isParam(got[i]) || isWild(want[i]) {
				continue
			}
			ok = false
			break
		}
		if ok {
			out = append(out, o)
		}
	}
	return out
}

// splitPath splits "/a/b/" into ["a","b",""]: the trailing empty segment is
// kept so that a literal "/config/ai/apikey/" (completed by a path parameter
// at run time) is one segment longer than "/config/ai/apikey".
func splitPath(p string) []string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func isParam(seg string) bool { return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") }

func isWild(seg string) bool { return seg == "" || strings.Contains(seg, "%") }
