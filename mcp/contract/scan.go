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
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// PathRef is one REST path literal found in the bridge source, attributed to
// the tool whose handler contains it when that can be read statically.
type PathRef struct {
	File    string
	Line    int
	Tool    string // "" when the enclosing function names no tool
	Method  string // "" when the literal is not an argument of a client call and the function mixes methods
	Literal string
}

// client methods and the argument index that carries the path.
var clientMethods = map[string]struct {
	method string
	arg    int
}{
	"Get": {"GET", 1}, "GetQ": {"GET", 1}, "GetText": {"GET", 1},
	"Post": {"POST", 1}, "PostMultipartFile": {"POST", 1},
	"Put": {"PUT", 1}, "Patch": {"PATCH", 1},
	"Delete": {"DELETE", 1}, "DeleteQ": {"DELETE", 1},
}

// registration helpers whose first argument is the tool name and whose
// second argument is the path.
var toolPathHelpers = map[string]string{
	"passthrough": "GET", "enableFlagTool": "POST", "postSimple": "POST",
}

// calls whose first argument names the tool the enclosing function serves.
var toolNamingCalls = map[string]bool{"audit": true, "auditMut": true, "gateDestructive": true}

var pathLitRe = regexp.MustCompile(`^/[a-z][A-Za-z0-9_./%{}-]*$`)

// ScanPathRefs parses every non-test .go file under dir and returns the
// REST path literals it finds.
func ScanPathRefs(dir string) ([]PathRef, error) {
	files, err := goFiles(dir)
	if err != nil {
		return nil, err
	}
	var refs []PathRef
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range af.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					refs = append(refs, scanFunc(fset, f, d.Body)...)
				}
			case *ast.GenDecl:
				// package-level tables (the legacy metrics path map)
				ast.Inspect(d, func(n ast.Node) bool {
					if lit, ok := pathLiteral(n); ok {
						refs = append(refs, PathRef{File: f, Line: fset.Position(n.Pos()).Line, Literal: lit})
					}
					return true
				})
			}
		}
	}
	return refs, nil
}

func scanFunc(fset *token.FileSet, file string, body *ast.BlockStmt) []PathRef {
	tool := ""
	methods := map[string]bool{}
	attributed := map[ast.Node]bool{}
	var refs []PathRef

	// pass 1: tool name, method set, literals that are direct call arguments
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		if toolNamingCalls[name] && len(call.Args) > 0 {
			if s, ok := stringLit(call.Args[0]); ok && tool == "" {
				tool = s
			}
		}
		if m, ok := toolPathHelpers[name]; ok && len(call.Args) >= 2 {
			t, _ := stringLit(call.Args[0])
			if t != "" && tool == "" {
				tool = t
			}
			if lit, ok := pathLiteral(call.Args[1]); ok {
				refs = append(refs, PathRef{File: file, Line: fset.Position(call.Pos()).Line, Tool: t, Method: m, Literal: lit})
				attributed[call.Args[1]] = true
			} else {
				methods[m] = true
			}
		}
		if cm, ok := clientMethods[name]; ok && len(call.Args) > cm.arg {
			methods[cm.method] = true
			if lit, ok := pathLiteral(call.Args[cm.arg]); ok {
				refs = append(refs, PathRef{File: file, Line: fset.Position(call.Pos()).Line, Method: cm.method, Literal: lit})
				attributed[call.Args[cm.arg]] = true
			}
		}
		return true
	})

	// pass 2: every other path literal in the function (assigned to a
	// variable, in a map) takes the function's method when it is unambiguous
	method := ""
	if len(methods) == 1 {
		for m := range methods {
			method = m
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if attributed[n] {
			return false
		}
		switch n.(type) {
		case *ast.BasicLit, *ast.BinaryExpr, *ast.CallExpr:
			if lit, ok := pathLiteral(n); ok {
				refs = append(refs, PathRef{File: file, Line: fset.Position(n.Pos()).Line, Method: method, Literal: lit})
				return false
			}
		}
		return true
	})
	for i := range refs {
		if refs[i].Tool == "" {
			refs[i].Tool = tool
		}
	}
	return refs
}

// pathLiteral reduces an expression to a REST path literal: a string
// literal, a concatenation whose left-most operand is one, or an
// fmt.Sprintf whose format is one.
func pathLiteral(n ast.Node) (string, bool) {
	switch e := n.(type) {
	case *ast.BasicLit:
		s, ok := stringLit(e)
		if ok && pathLitRe.MatchString(s) {
			return s, true
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			if s, ok := pathLiteral(leftMost(e)); ok {
				return s, true
			}
		}
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sprintf" && len(e.Args) > 0 {
			return pathLiteral(e.Args[0])
		}
	}
	return "", false
}

func leftMost(e *ast.BinaryExpr) ast.Expr {
	for {
		l, ok := e.X.(*ast.BinaryExpr)
		if !ok {
			return e.X
		}
		e = l
	}
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// TagField is one json-tagged struct field of the bridge source.
type TagField struct {
	File   string
	Line   int
	Struct string
	Field  string
	JSON   string // json name (tag before the first comma)
	Input  bool   // struct name ends in "In" (a tool input)
}

// ScanJSONTags returns every json-tagged struct field of the non-test .go
// files under dir, including fields of anonymous structs (named after the
// enclosing declaration).
func ScanJSONTags(dir string) ([]TagField, error) {
	files, err := goFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []TagField
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			return nil, err
		}
		var structName string
		ast.Inspect(af, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeSpec:
				structName = x.Name.Name
			case *ast.StructType:
				for _, fld := range x.Fields.List {
					if fld.Tag == nil {
						continue
					}
					tag, err := strconv.Unquote(fld.Tag.Value)
					if err != nil {
						continue
					}
					js := reflect.StructTag(tag).Get("json")
					name, _, _ := strings.Cut(js, ",")
					if name == "" || name == "-" {
						continue
					}
					fname := ""
					if len(fld.Names) > 0 {
						fname = fld.Names[0].Name
					}
					out = append(out, TagField{
						File: f, Line: fset.Position(fld.Pos()).Line, Struct: structName,
						Field: fname, JSON: name, Input: strings.HasSuffix(structName, "In"),
					})
				}
			}
			return true
		})
	}
	return out, nil
}

// ScanStringLiterals returns the set of every string literal in the
// non-test .go files under dir.
func ScanStringLiterals(dir string) (map[string]bool, error) {
	files, err := goFiles(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok {
				if s, ok := stringLit(lit); ok {
					out[s] = true
				}
			}
			return true
		})
	}
	return out, nil
}

func goFiles(dir string) ([]string, error) {
	all, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range all {
		if !strings.HasSuffix(f, "_test.go") {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out, nil
}
