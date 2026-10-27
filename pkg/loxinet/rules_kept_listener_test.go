/*
 * Copyright (c) 2026 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 */
package loxinet

import (
	"strings"
	"testing"
	"time"

	cmn "github.com/loxilb-io/loxilb/common"
)

// TestKeptListenerAnswersNoRoute: deleting the last rule of a VIP:port keeps
// the fullproxy listener open so a client gets an HTTP error rather than a
// refused connection. That error names the real state, a listener with no
// rule, not "model_unavailable" (the cluster report of 2026-10-13, item 4:
// operators read that as a pool problem and looked for the model's pool).
// A rule added back on the address takes the kept listener over and serves.
func TestKeptListenerAnswersNoRoute(t *testing.T) {
	if mh.zr == nil || mh.dpEbpf == nil {
		t.Skip("loxinet harness not initialized (run the whole package as root)")
	}
	_, ep, _ := eppTestBackendSeen(t, "a")
	serv := cmn.LbServiceArg{ServIP: eppTestVIP, ServPort: 28190, Proto: "tcp", Mode: cmn.LBModeFullProxy, Sel: cmn.LbSelRr}
	if _, err := mh.zr.Rules.AddLbRule(serv, nil, nil, nil, []cmn.LbEndPointArg{ep}); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	t.Cleanup(func() { mh.zr.Rules.DeleteLbRule(serv) })
	body := `{"model":"alias","prompt":"hello"}`
	if code, resp, err := eppTestRequest(28190, body, 5*time.Second); err != nil || code != 200 {
		t.Fatalf("with the rule: code=%d resp=%q err=%v", code, resp, err)
	}
	if _, err := mh.zr.Rules.DeleteLbRule(serv); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	code, resp, err := eppTestRequest(28190, body, 5*time.Second)
	if err != nil {
		t.Fatalf("the kept listener refused the connection (want an HTTP 503): %v", err)
	}
	if code != 503 || !strings.Contains(resp, `"error":"no_route"`) || strings.Contains(resp, "model_unavailable") {
		t.Fatalf("rule deleted: code=%d resp=%q, want 503 no_route naming the rule-less listener", code, resp)
	}
	if _, err := mh.zr.Rules.AddLbRule(serv, nil, nil, nil, []cmn.LbEndPointArg{ep}); err != nil {
		t.Fatalf("add rule again: %v", err)
	}
	if code, resp, err := eppTestRequest(28190, body, 5*time.Second); err != nil || code != 200 {
		t.Fatalf("rule back on the kept listener: code=%d resp=%q err=%v", code, resp, err)
	}
}
