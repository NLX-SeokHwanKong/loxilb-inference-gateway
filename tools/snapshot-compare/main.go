// snapshot-compare compares normalized desired state, including global
// singleton values that the replacement engine's legacy item digest omits.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	snap "github.com/loxilb-io/loxilb/pkg/snapshot"
	"os"
	"reflect"
	"strings"
)

func read(path string) *snap.Document {
	raw, e := os.ReadFile(path)
	if e != nil {
		panic(e)
	}
	d, e := snap.Decode(bytes.NewReader(raw))
	if e != nil {
		panic(e)
	}
	if e = snap.VerifyChecksum(d); e != nil {
		panic(e)
	}
	if e = snap.NormalizeDomains(&d.Domains); e != nil {
		panic(e)
	}
	return d
}
func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: snapshot-compare first.json second.json")
		os.Exit(2)
	}
	a, b := read(os.Args[1]), read(os.Args[2])
	av, bv := reflect.ValueOf(a.Domains), reflect.ValueOf(b.Domains)
	var rows []map[string]any
	pass := true
	for i := 0; i < av.NumField(); i++ {
		name := strings.Split(av.Type().Field(i).Tag.Get("json"), ",")[0]
		left, _ := json.Marshal(av.Field(i).Interface())
		right, _ := json.Marshal(bv.Field(i).Interface())
		equal := bytes.Equal(left, right)
		pass = pass && equal
		rows = append(rows, map[string]any{"domain": name, "equal_desired_payload": equal})
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"all_equal": pass, "domains": rows}); err != nil {
		panic(err)
	}
	if !pass {
		os.Exit(1)
	}
}
