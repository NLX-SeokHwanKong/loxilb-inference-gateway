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
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	cmn "github.com/loxilb-io/loxilb/common"
	"github.com/loxilb-io/loxilb/pkg/epp/epptest"
)

// A backend that is not on loopback. The sockproxy's connect() to a
// loopback address completes inside the call; to a remote one it returns
// EINPROGRESS and the leg's writable event arrives later, re-entering the
// request setup for the SAME request — the shape that looped on the
// cluster (3,480 EPP submits for one FailOpen request while the EPP was
// down) and that loopback never shows. The backend lives in a network
// namespace behind a veth pair whose host side delays packets, so the
// handshake cannot finish before the EPP stage has run.
const (
	eppRemoteNs     = "lxepptest"
	eppRemoteVethH  = "lxeppv0"
	eppRemoteVethNs = "lxeppv1"
	eppRemoteHostIP = "10.254.7.1"
	eppRemoteIP     = "10.254.7.2"
	eppRemotePort   = 18000
)

func eppRemoteRun(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	// loxilb's global SIGCHLD reaper (loxinet.go) can reap this child before
	// CombinedOutput's own Wait, which then returns ECHILD even though the
	// command ran fine -- the same hazard isWaitECHILD guards everywhere else.
	if err != nil && !isWaitECHILD(err) {
		t.Fatalf("%s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func eppRemoteTeardown() {
	exec.Command("ip", "link", "del", eppRemoteVethH).Run()
	exec.Command("ip", "netns", "del", eppRemoteNs).Run()
}

// eppTestRemoteBackend builds the namespace and starts the `name` backend
// inside it, listening on eppRemoteIP:eppRemotePort. Skips when the host
// cannot make namespaces (not root, no ip/tc, no netem).
func eppTestRemoteBackend(t *testing.T, name string) (cmn.LbEndPointArg, *eppTestSeen) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to make a network namespace")
	}
	for _, tool := range []string{"ip", "tc"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	eppRemoteTeardown() // a previous run that died
	t.Cleanup(eppRemoteTeardown)
	eppRemoteRun(t, "ip", "netns", "add", eppRemoteNs)
	eppRemoteRun(t, "ip", "link", "add", eppRemoteVethH, "type", "veth", "peer", "name", eppRemoteVethNs)
	eppRemoteRun(t, "ip", "link", "set", eppRemoteVethNs, "netns", eppRemoteNs)
	eppRemoteRun(t, "ip", "addr", "add", eppRemoteHostIP+"/30", "dev", eppRemoteVethH)
	eppRemoteRun(t, "ip", "link", "set", eppRemoteVethH, "up")
	eppRemoteRun(t, "ip", "-n", eppRemoteNs, "addr", "add", eppRemoteIP+"/30", "dev", eppRemoteVethNs)
	eppRemoteRun(t, "ip", "-n", eppRemoteNs, "link", "set", eppRemoteVethNs, "up")
	eppRemoteRun(t, "ip", "-n", eppRemoteNs, "link", "set", "lo", "up")
	if out, err := exec.Command("tc", "qdisc", "add", "dev", eppRemoteVethH, "root", "netem", "delay", "30ms").CombinedOutput(); err != nil && !isWaitECHILD(err) {
		t.Skipf("tc netem not available: %v: %s", err, out)
	}

	// Bind the listener from inside the namespace: the socket keeps its
	// namespace afterwards, the thread goes back to the host's.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(orig)
	nsfd, err := unix.Open("/run/netns/"+eppRemoteNs, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(nsfd)
	if err := unix.Setns(nsfd, unix.CLONE_NEWNET); err != nil {
		t.Fatalf("setns: %v", err)
	}
	l, lerr := net.Listen("tcp", fmt.Sprintf("%s:%d", eppRemoteIP, eppRemotePort))
	if err := unix.Setns(orig, unix.CLONE_NEWNET); err != nil {
		t.Fatalf("setns back: %v", err)
	}
	if lerr != nil {
		t.Fatalf("listen in %s: %v", eppRemoteNs, lerr)
	}
	_, ep, seen := eppTestBackendOn(t, name, l)
	return ep, seen
}

// eppKeepAliveRequest sends one request the way curl does (HTTP/1.1, the
// connection kept open) and returns the status and body.
func eppKeepAliveRequest(t *testing.T, port uint16, body string) (int, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(eppTestVIP, fmt.Sprint(port)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(conn, "POST /v1/completions HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestEppRemoteBackendConnectsOncePerRequest(t *testing.T) {
	if mh.zr == nil || mh.dpEbpf == nil {
		t.Skip("loxinet harness not initialized (run the whole package as root)")
	}
	ep, seen := eppTestRemoteBackend(t, "remote")
	body := `{"model":"alias","prompt":"hello"}`

	hits := func() int {
		seen.mu.Lock()
		defer seen.mu.Unlock()
		return seen.hits
	}

	t.Run("FailOpen with the EPP down submits once", func(t *testing.T) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		dead := l.Addr().String()
		l.Close()
		eppTestRule(t, 28170, dead, cmn.EppFailureModeFailOpen, 2000, ep)
		errors0 := eppMetric(t, "loxilb_ai_epp_requests_total", "outcome", "error")
		hits0 := hits()
		code, resp := eppKeepAliveRequest(t, 28170, body)
		if code != 200 || !strings.Contains(resp, `"served":"remote"`) {
			t.Fatalf("FailOpen to the remote backend: code=%d resp=%q", code, resp)
		}
		time.Sleep(500 * time.Millisecond) // a loop would still be running
		if got := eppMetric(t, "loxilb_ai_epp_requests_total", "outcome", "error"); got != errors0+1 {
			t.Fatalf("EPP submits for one request: %v (error outcomes %v -> %v), want exactly 1", got-errors0, errors0, got)
		}
		if got := hits() - hits0; got != 1 {
			t.Fatalf("the backend served %d requests, want 1", got)
		}
		eppTestSettled(t)
	})

	t.Run("the EPP's decision to a remote endpoint", func(t *testing.T) {
		f := epptest.New("echo")
		f.Dest = eppAddr(ep)
		addr, stop := epptest.Start(t, f, false)
		defer stop()
		eppTestRule(t, 28172, addr, cmn.EppFailureModeFailClose, 3000, ep)
		hits0 := hits()
		code, resp := eppKeepAliveRequest(t, 28172, body)
		if code != 200 || !strings.Contains(resp, `"served":"remote"`) {
			t.Fatalf("EPP decision to the remote backend: code=%d resp=%q", code, resp)
		}
		eppTestResponsePhase(t, f, "200", eppAddr(ep))
		f.Mu.Lock()
		streams := f.Streams
		f.Mu.Unlock()
		if streams != 1 {
			t.Fatalf("the EPP saw %d streams for one request, want 1", streams)
		}
		if got := hits() - hits0; got != 1 {
			t.Fatalf("the backend served %d requests, want 1", got)
		}
		eppTestSettled(t)
	})
}
