// snapshot-inventory expands both authoritative Swagger files into request
// fields and compares their names with the snapshot wire document. Candidate
// matches are structural evidence only; they are not a runtime PASS.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	snap "github.com/loxilb-io/loxilb/pkg/snapshot"
	yaml "gopkg.in/yaml.v2"
)

type schema struct {
	Ref         string            `yaml:"$ref"`
	Type        string            `yaml:"type"`
	Properties  map[string]schema `yaml:"properties"`
	Items       *schema           `yaml:"items"`
	AllOf       []schema          `yaml:"allOf"`
	ReadOnly    bool              `yaml:"readOnly"`
	Description string            `yaml:"description"`
}
type parameter struct {
	Ref         string  `yaml:"$ref"`
	Name        string  `yaml:"name"`
	In          string  `yaml:"in"`
	Type        string  `yaml:"type"`
	Schema      *schema `yaml:"schema"`
	Items       *schema `yaml:"items"`
	Description string  `yaml:"description"`
}
type operation struct {
	Summary    string      `yaml:"summary"`
	Parameters []parameter `yaml:"parameters"`
}
type spec struct {
	Paths       map[string]map[string]operation `yaml:"paths"`
	Definitions map[string]schema               `yaml:"definitions"`
	Parameters  map[string]parameter            `yaml:"parameters"`
}
type field struct {
	Path        string   `json:"path"`
	Type        string   `json:"type"`
	ReadOnly    bool     `json:"read_only,omitempty"`
	Description string   `json:"description,omitempty"`
	Candidates  []string `json:"snapshot_candidates,omitempty"`
}
type row struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Spec    string              `json:"spec"`
	Summary string              `json:"summary"`
	Class   snap.LifecycleClass `json:"class"`
	Area    string              `json:"area"`
	Desired bool                `json:"desired_state"`
	Fields  []field             `json:"request_fields"`
}

func prefix(a, b string) string {
	if a == "" {
		return b
	}
	return a + "." + b
}
func expand(s schema, path string, defs map[string]schema, seen map[string]bool) ([]field, error) {
	if s.Ref != "" {
		if seen[s.Ref] {
			return nil, fmt.Errorf("recursive ref %s", s.Ref)
		}
		def, ok := defs[strings.TrimPrefix(s.Ref, "#/definitions/")]
		if !ok {
			return nil, fmt.Errorf("unknown ref %s", s.Ref)
		}
		next := map[string]bool{}
		for k, v := range seen {
			next[k] = v
		}
		next[s.Ref] = true
		return expand(def, path, defs, next)
	}
	var out []field
	for _, part := range s.AllOf {
		f, e := expand(part, path, defs, seen)
		if e != nil {
			return nil, e
		}
		out = append(out, f...)
	}
	if len(s.Properties) > 0 {
		keys := []string{}
		for k := range s.Properties {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f, e := expand(s.Properties[k], prefix(path, k), defs, seen)
			if e != nil {
				return nil, e
			}
			out = append(out, f...)
		}
		return out, nil
	}
	if s.Type == "array" && s.Items != nil {
		return expand(*s.Items, path+"[]", defs, seen)
	}
	if len(out) == 0 {
		out = append(out, field{Path: path, Type: s.Type, ReadOnly: s.ReadOnly, Description: s.Description})
	}
	return out, nil
}
func wire(t reflect.Type, path string, out map[string][]string, domain string, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		if t.Elem().Kind() == reflect.Uint8 {
			out[domain] = append(out[domain], path)
			return
		}
		wire(t.Elem(), path+"[]", out, domain, seen)
		return
	}
	if t.Kind() != reflect.Struct || t.PkgPath() == "time" {
		out[domain] = append(out[domain], path)
		return
	}
	if seen[t] {
		return
	}
	next := map[reflect.Type]bool{}
	for k, v := range seen {
		next[k] = v
	}
	next[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "-" {
			continue
		}
		if f.Anonymous && tag == "" {
			wire(f.Type, path, out, domain, next)
			continue
		}
		if tag == "" {
			tag = f.Name
		}
		wire(f.Type, prefix(path, tag), out, domain, next)
	}
}
func norm(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "", "[]", "").Replace(s))
}
func main() {
	root := flag.String("root", ".", "source checkout")
	output := flag.String("output", "", "JSON report destination")
	flag.Parse()
	domains := map[string][]string{}
	typ := reflect.TypeOf(snap.Domains{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		wire(f.Type, "", domains, name, map[reflect.Type]bool{})
	}
	for k := range domains {
		sort.Strings(domains[k])
	}
	lifecycle := snap.RouteLifecycleIndex()
	var rows []row
	var failures []string
	for _, file := range []string{"api/swagger.yml", "api/swagger-extras.yml"} {
		raw, e := os.ReadFile(filepath.Join(*root, file))
		if e != nil {
			panic(e)
		}
		var spec spec
		if e = yaml.Unmarshal(raw, &spec); e != nil {
			panic(e)
		}
		for path, ops := range spec.Paths {
			for method, op := range ops {
				if method != "post" && method != "put" && method != "patch" && method != "delete" {
					continue
				}
				key := snap.RouteKey(method, path)
				lc, ok := lifecycle[key]
				if !ok {
					failures = append(failures, key)
				}
				r := row{Method: method, Path: path, Spec: file, Summary: op.Summary, Class: lc.Class, Area: lc.Area, Desired: lc.DesiredState}
				for _, p := range op.Parameters {
					if p.Ref != "" {
						var ok bool
						p, ok = spec.Parameters[strings.TrimPrefix(p.Ref, "#/parameters/")]
						if !ok {
							panic("unknown parameter ref")
						}
					}
					var fields []field
					if p.In == "body" && p.Schema != nil {
						fields, e = expand(*p.Schema, "", spec.Definitions, map[string]bool{})
						if e != nil {
							panic(e)
						}
					} else {
						fields = []field{{Path: p.In + ":" + p.Name, Type: p.Type, Description: p.Description}}
					}
					for _, f := range fields {
						if lc.Class == snap.ClassSnapshot && p.In == "body" {
							for _, target := range domains[lc.Area] {
								if norm(f.Path) == norm(strings.TrimPrefix(target, "[].")) {
									f.Candidates = append(f.Candidates, target)
								}
							}
							if len(f.Candidates) == 0 {
								leaf := strings.Split(norm(f.Path), ".")
								for _, target := range domains[lc.Area] {
									parts := strings.Split(norm(target), ".")
									if parts[len(parts)-1] == leaf[len(leaf)-1] {
										f.Candidates = append(f.Candidates, target)
									}
								}
							}
						}
						r.Fields = append(r.Fields, f)
					}
				}
				rows = append(rows, r)
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		return a.Method+" "+a.Path+" "+a.Spec < b.Method+" "+b.Path+" "+b.Spec
	})
	result := map[string]any{"routes": rows, "snapshot_wire_fields": domains, "unclassified_routes": failures}
	raw, e := json.MarshalIndent(result, "", "  ")
	if e != nil {
		panic(e)
	}
	if *output != "" {
		if e = os.WriteFile(*output, append(raw, '\n'), 0600); e != nil {
			panic(e)
		}
	} else {
		fmt.Println(string(raw))
	}
}
