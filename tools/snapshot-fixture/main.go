// snapshot-fixture produces an empty desired-state document for an isolated
// replacement qualification. Never apply its output to an existing service.
package main

import (
	"bytes"
	"flag"
	snap "github.com/loxilb-io/loxilb/pkg/snapshot"
	"os"
)

func main() {
	input := flag.String("input", "", "snapshot input")
	output := flag.String("output", "", "new fixture output")
	changeGlobal := flag.Bool("change-global", false, "change BGP/IPsec global fields instead of emptying")
	flag.Parse()
	raw, err := os.ReadFile(*input)
	if err != nil {
		panic(err)
	}
	doc, err := snap.Decode(bytes.NewReader(raw))
	if err != nil {
		panic(err)
	}
	if err = snap.VerifyChecksum(doc); err != nil {
		panic(err)
	}
	if *changeGlobal {
		if c := doc.Domains.BGP.GlobalConfig; c != nil {
			c.LocalAs = 65128
			c.RouterID = "192.0.2.28"
			c.ListenPort = 1189
			c.SetNHSelf = false
		}
		if c := doc.Domains.IPsec.Config; c != nil {
			c.MTU = 1381
			c.AntiReplayEnabled = !c.AntiReplayEnabled
			c.SALifetimeWarnSeconds = 83
			c.SeqOverflowAction = "continue"
		}
	} else {
		doc.Domains = snap.Domains{}
	}
	encoded, err := snap.Encode(doc)
	if err != nil {
		panic(err)
	}
	f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		panic(err)
	}
	if _, err = f.Write(encoded); err != nil {
		panic(err)
	}
	if err = f.Close(); err != nil {
		panic(err)
	}
}
