/*
 * Copyright (c) 2026 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 */
package loxinet

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	cmn "github.com/loxilb-io/loxilb/common"
	"github.com/loxilb-io/loxilb/pkg/epp/epptest"
)

// An EPP that names a different endpoint for every request is followed
// every time: with two backends and an EPP that alternates, the requests
// land a, b, a, b... and each backend serves exactly half (the T2 skew
// check the cluster could not run with one decode pod, and the aggregated
// round-robin of the second verification). Both over one connection per
// request and over a single kept connection, where the previous request's
// backend leg must not carry the next one. Needs the loxinet harness (root).
func TestEppDecisionsSpreadAcrossBackends(t *testing.T) {
	if mh.zr == nil || mh.dpEbpf == nil {
		t.Skip("loxinet harness not initialized (run the whole package as root)")
	}
	body := `{"model":"alias","prompt":"hello"}`

	servedBy := func(resp string) string {
		for _, name := range []string{"a", "b"} {
			if strings.Contains(resp, fmt.Sprintf(`"served":"%s"`, name)) {
				return name
			}
		}
		return "?"
	}
	wantAlternating := func(t *testing.T, order []string, seenA, seenB *eppTestSeen) {
		t.Helper()
		for i, s := range order {
			if want := []string{"a", "b"}[i%2]; s != want {
				t.Fatalf("request %d went to %q, want %q (order %v)", i+1, s, want, order)
			}
		}
		seenA.mu.Lock()
		hitsA := seenA.hits
		seenA.mu.Unlock()
		seenB.mu.Lock()
		hitsB := seenB.hits
		seenB.mu.Unlock()
		if hitsA != len(order)/2 || hitsB != len(order)/2 {
			t.Fatalf("backend hits a=%d b=%d, want %d each", hitsA, hitsB, len(order)/2)
		}
	}

	t.Run("one connection per request", func(t *testing.T) {
		_, epA, seenA := eppTestBackendSeen(t, "a")
		_, epB, seenB := eppTestBackendSeen(t, "b")
		f := epptest.New("echo")
		f.Dests = []string{eppAddr(epA), eppAddr(epB)}
		addr, stop := epptest.Start(t, f, false)
		defer stop()
		eppTestRule(t, 28160, addr, cmn.EppFailureModeFailClose, 3000, epA, epB)

		var order []string
		for i := 1; i <= 6; i++ {
			code, resp, err := eppTestRequest(28160, body, 5*time.Second)
			if err != nil || code != 200 {
				t.Fatalf("request %d: code=%d resp=%q err=%v", i, code, resp, err)
			}
			order = append(order, servedBy(resp))
			select {
			case <-f.StreamClosed:
			case <-time.After(3 * time.Second):
				t.Fatalf("request %d: the EPP stream did not end", i)
			}
		}
		wantAlternating(t, order, seenA, seenB)
		eppTestSettled(t)
	})

	t.Run("one kept connection", func(t *testing.T) {
		_, epA, seenA := eppTestBackendSeen(t, "a")
		_, epB, seenB := eppTestBackendSeen(t, "b")
		f := epptest.New("echo")
		f.Dests = []string{eppAddr(epA), eppAddr(epB)}
		addr, stop := epptest.Start(t, f, false)
		defer stop()
		eppTestRule(t, 28162, addr, cmn.EppFailureModeFailClose, 3000, epA, epB)

		conn, err := net.Dial("tcp", net.JoinHostPort(eppTestVIP, "28162"))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(15 * time.Second))
		rd := bufio.NewReader(conn)
		var order []string
		for i := 1; i <= 6; i++ {
			fmt.Fprintf(conn, "POST /v1/completions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
			resp, err := http.ReadResponse(rd, nil)
			if err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("request %d: code=%d body=%q", i, resp.StatusCode, b)
			}
			order = append(order, servedBy(string(b)))
			select {
			case <-f.StreamClosed:
			case <-time.After(3 * time.Second):
				t.Fatalf("request %d: the EPP stream did not end", i)
			}
		}
		wantAlternating(t, order, seenA, seenB)
		eppTestSettled(t)
	})
}
