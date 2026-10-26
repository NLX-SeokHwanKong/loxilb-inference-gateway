//go:build linux

/*
 * Copyright (c) 2026 NetLOX Inc
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
 */
package status

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func processFixtureStat(pid, command string, cpu, start, rss uint64) string {
	f := make([]string, 22)
	for i := range f {
		f[i] = "0"
	}
	f[0] = "S"
	f[11] = strconv.FormatUint(cpu, 10)
	f[15] = "20"
	f[16] = "0"
	f[19] = strconv.FormatUint(start, 10)
	f[20] = "1048576"
	f[21] = strconv.FormatUint(rss, 10)
	return pid + " (" + command + ") " + strings.Join(f, " ")
}

func writeProcessFixture(t *testing.T, root, pid, command string, cpu, start uint64) {
	t.Helper()
	path := filepath.Join(root, pid)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"stat": processFixtureStat(pid, command, cpu, start, 10), "status": "Name:\tworker\nUid:\t1000\t1001\t1001\t1001\n", "statm": "256 10 3 0 0 0 0\n"} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProcessStatCommandParsing(t *testing.T) {
	for _, command := range []string{"normal", "space worker", "a)b (c)", "line\nbreak"} {
		t.Run(command, func(t *testing.T) {
			p, err := parseProcessStat(processFixtureStat("23", command, 500, 1000, 10))
			if err != nil || p.command != command || p.cpu != 500 || p.start != 1000 || p.resident != 10 {
				t.Fatalf("stat=%+v err=%v", p, err)
			}
		})
	}
	for _, bad := range []string{"", "1 worker", "1 (worker) S", strings.Replace(processFixtureStat("1", "worker", 2, 3, 4), "1048576", "garbage", 1)} {
		if _, err := parseProcessStat(bad); err == nil {
			t.Fatalf("accepted malformed stat %q", bad)
		}
	}
}

func TestProcessProcfsContract(t *testing.T) {
	root := t.TempDir()
	passwd := filepath.Join(root, "passwd")
	for name, data := range map[string]string{"uptime": "100.00 0.00\n", "meminfo": "MemTotal: 1000 kB\n", "passwd": "worker:x:1001:1001::/:/bin/false\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProcessFixture(t, root, "21", "worker ) with space", 500, 1000)
	writeProcessFixture(t, root, "20", "idle", 0, 0)
	// A directory left behind by a process that exited is harmless.
	if err := os.Mkdir(filepath.Join(root, "99"), 0700); err != nil {
		t.Fatal(err)
	}
	rows, err := processInfoFromProc(root, passwd, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d", len(rows))
	}
	p := rows[0]
	page := uint64(os.Getpagesize())
	mem := fmt.Sprintf("%.1f", 100*float64(10*page)/1024/1000)
	if p.Pid != "21" || p.Command != "worker ) with space" || p.User != "worker" || p.CPUUsage != "5.6" || p.MemoryUsage != mem || p.Time != "0:05.00" || p.VirtMemory != "1024" || p.ResidentSize != strconv.FormatUint(10*page/1024, 10) || p.SharedMemory != strconv.FormatUint(3*page/1024, 10) || p.Priority != "20" || p.Nice != "0" || p.Status != "S" {
		t.Fatalf("unexpected process fields: %+v", p)
	}
	rows, err = processInfoFromProc(root, filepath.Join(root, "missing-passwd"), 100)
	if err != nil || rows[0].User != "1001" {
		t.Fatalf("numeric UID fallback: err=%v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "21", "stat"), []byte("bad stat"), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err = processInfoFromProc(root, passwd, 100)
	if err != nil || len(rows) != 1 || rows[0].Pid != "20" {
		t.Fatalf("one malformed PID must not fail healthy rows: %+v %v", rows, err)
	}
}

func TestProcessProcfsUnavailable(t *testing.T) {
	root := t.TempDir()
	if _, err := processInfoFromProc(root, "", 100); err == nil {
		t.Fatal("missing uptime reported success")
	}
	os.WriteFile(filepath.Join(root, "uptime"), []byte("1 0"), 0600)
	if _, err := processInfoFromProc(root, "", 100); err == nil {
		t.Fatal("missing MemTotal reported success")
	}
	os.WriteFile(filepath.Join(root, "meminfo"), []byte("MemTotal: 1000 kB"), 0600)
	if _, err := processInfoFromProc(root, "", 100); err == nil {
		t.Fatal("empty process list reported success")
	}
	if _, err := processInfoFromProc(root, "", 0); err == nil {
		t.Fatal("invalid tick rate reported success")
	}
}

func TestProcessNativeConcurrentWithoutExecutables(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	pid := strconv.Itoa(os.Getpid())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				rows, err := ProcessInfoGet()
				if err != nil {
					t.Error(err)
					return
				}
				found := false
				for _, p := range rows {
					if p.Pid == pid {
						found = true
						if p.Command == "" || p.CPUUsage == "" || p.MemoryUsage == "" {
							t.Error("incomplete current process")
						}
					}
				}
				if !found {
					t.Error("current process missing")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func processReviewFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, data := range map[string]string{"uptime": "100 0", "meminfo": "MemTotal: 1000 kB", "passwd": "first:x:1001:1001::/:/bin/false\nsecond:x:1001:1001::/:/bin/false\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProcessFixture(t, root, "20", "stable", 1, 1)
	writeProcessFixture(t, root, "21", "exiting", 1, 1)
	return root
}

func TestProcessExitDuringEachRead(t *testing.T) {
	for _, target := range []string{"stat", "status", "statm", "stat-again"} {
		t.Run(target, func(t *testing.T) {
			root := processReviewFixture(t)
			statReads := 0
			injected := false
			reader := func(path string) ([]byte, error) {
				if filepath.Dir(path) == filepath.Join(root, "21") {
					name := filepath.Base(path)
					if name == "stat" {
						statReads++
					}
					if name == target || target == "stat-again" && name == "stat" && statReads == 2 {
						injected = true
						return nil, &os.PathError{Op: "read", Path: path, Err: syscall.ESRCH}
					}
				}
				return os.ReadFile(path)
			}
			rows, err := processInfoFromProcWithReader(root, filepath.Join(root, "passwd"), 100, reader)
			if !injected || err != nil || len(rows) != 1 || rows[0].Pid != "20" {
				t.Fatalf("exit %s: injected=%v rows=%+v err=%v", target, injected, rows, err)
			}
		})
	}
	for _, err := range []error{syscall.ESRCH, &os.PathError{Op: "read", Path: "/proc/example/stat", Err: syscall.ESRCH}, fmt.Errorf("wrapped: %w", syscall.ESRCH), syscall.ENOENT, syscall.EPERM, syscall.EACCES} {
		if !transientProcessError(err) {
			t.Errorf("must skip %v", err)
		}
	}
	if transientProcessError(syscall.EIO) || transientProcessError(nil) {
		t.Fatal("unexpected I/O must not be silently skipped")
	}
}

func TestProcessMalformedRowsAndPriority(t *testing.T) {
	for _, file := range []string{"stat", "status", "statm"} {
		t.Run(file, func(t *testing.T) {
			root := processReviewFixture(t)
			if err := os.WriteFile(filepath.Join(root, "21", file), []byte("malformed"), 0600); err != nil {
				t.Fatal(err)
			}
			rows, err := processInfoFromProc(root, filepath.Join(root, "passwd"), 100)
			if err != nil || len(rows) != 1 || rows[0].Pid != "20" {
				t.Fatalf("malformed %s: rows=%+v err=%v", file, rows, err)
			}
		})
	}
	for _, priority := range []string{"-100", "-51", "20"} {
		t.Run(priority, func(t *testing.T) {
			root := processReviewFixture(t)
			data := strings.Replace(processFixtureStat("20", "stable", 1, 1, 1), " 20 0 ", " "+priority+" 0 ", 1)
			if err := os.WriteFile(filepath.Join(root, "20", "stat"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			rows, err := processInfoFromProc(root, filepath.Join(root, "passwd"), 100)
			if err != nil {
				t.Fatal(err)
			}
			want := priority
			if priority == "-100" {
				want = "rt"
			}
			for _, p := range rows {
				if p.Pid == "20" && (p.Priority != want || p.User != "first") {
					t.Fatalf("priority/first UID: %+v", p)
				}
			}
		})
	}
}

func TestProcessServerErrorsAreTyped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader func(string) ([]byte, error)
		ticks  float64
	}{
		{"missing procfs", os.ReadFile, 100},
		{"invalid uptime", func(string) ([]byte, error) { return []byte("invalid"), nil }, 100},
		{"invalid ticks", os.ReadFile, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := processInfoFromProcWithReader(t.TempDir(), "", tc.ticks, tc.reader)
			var typed *ProcessReadError
			if !errors.As(err, &typed) {
				t.Fatalf("missing typed server error: %v", err)
			}
		})
	}
}

func TestProcessCollectionDuringChildChurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var children atomic.Int64
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				err := exec.CommandContext(ctx, "/bin/true").Run()
				if err != nil && ctx.Err() == nil {
					t.Errorf("spawn churn child: %v", err)
					return
				}
				children.Add(1)
			}
		}()
	}
	defer wg.Wait()
	reads := 0
	pid := strconv.Itoa(os.Getpid())
	for ctx.Err() == nil {
		rows, err := ProcessInfoGet()
		if err != nil {
			cancel()
			t.Fatalf("collection during process exits: %v", err)
		}
		found := false
		for _, p := range rows {
			if p.Pid == pid {
				found = true
			}
		}
		if !found {
			cancel()
			t.Fatal("stable current process missing during churn")
		}
		reads++
	}
	if reads == 0 || children.Load() == 0 {
		t.Fatal("churn regression did not exercise both spawning and collection")
	}
	t.Logf("process churn: %d collections, %d short-lived children, zero collection failures", reads, children.Load())
}
