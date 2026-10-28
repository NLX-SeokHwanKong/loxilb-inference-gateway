/*
 * Copyright (c) 2026 LoxiLB Authors
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

package handler

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/loxilb-io/loxilb/pkg/snapshot"
)

// kickProbe installs a debouncer whose quiet period is short enough to
// observe: each kick fires once on the returned channel.
func kickProbe(t *testing.T) <-chan struct{} {
	t.Helper()
	fired := make(chan struct{}, 16)
	saved := autoPersist
	autoPersist = snapshot.NewAutoPersister(time.Millisecond, func() { fired <- struct{}{} })
	t.Cleanup(func() { autoPersist.Stop(); autoPersist = saved })
	return fired
}

func kicked(fired <-chan struct{}, want bool) bool {
	wait := 50 * time.Millisecond
	if want {
		wait = 2 * time.Second
	}
	select {
	case <-fired:
		return true
	case <-time.After(wait):
		return false
	}
}

func configDirtyGauge(t *testing.T) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "loxilb_config_dirty" {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatal("loxilb_config_dirty is not registered")
	return 0
}

func serveWithStatus(method, path string, status int) {
	h := AutoPersistMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, nil))
}

// Only a route the lifecycle registry classes as snapshot marks the config
// dirty and kicks the write-through; every other successful mutating
// config call, key-store and runtime-only alike, still advances the config
// generation. The lifecycle operations do neither, nor do logins and
// audit operations, which are not configuration, nor a refused mutation.
func TestAutoPersistEligibilityFollowsLifecycleRegistry(t *testing.T) {
	fired := kickProbe(t)
	for _, tc := range []struct {
		method, path  string
		status        int
		counted, kick bool
	}{
		{"POST", "/netlox/v1/config/loadbalancer", http.StatusOK, true, true},
		{"POST", "/netlox/v1/config/ai/tenant/ratelimit", http.StatusOK, true, false},
		{"POST", "/netlox/v1/config/params", http.StatusOK, true, false},
		{"POST", "/netlox/v1/config/pii/enable", http.StatusOK, true, false},
		{"DELETE", "/netlox/v1/config/ai/netidentity/x", http.StatusOK, true, true},
		{"POST", "/netlox/v1/audit/sink", http.StatusOK, true, true},
		{"DELETE", "/netlox/v1/config/loadbalancer/externalipaddress/10.0.0.1/port/80/protocol/tcp", http.StatusOK, true, true},
		{"DELETE", "/netlox/v1/config/loadbalancer/all", http.StatusOK, true, true},
		{"POST", "/netlox/v1/config/persist", http.StatusOK, false, false},
		{"POST", "/netlox/v1/config/restore", http.StatusOK, false, false},
		{"POST", "/netlox/v1/auth/login", http.StatusOK, false, false},
		{"POST", "/netlox/v1/audit/rotate", http.StatusOK, false, false},
		{"POST", "/netlox/v1/config/loadbalancer", http.StatusBadRequest, false, false},
		{"DELETE", "/netlox/v1/config/ai/netidentity/x", http.StatusNotFound, false, false},
	} {
		before := snapshot.ConfigGeneration()
		serveWithStatus(tc.method, tc.path, tc.status)
		if got := snapshot.ConfigGeneration() != before; got != tc.counted {
			t.Errorf("%s %s -> %d: generation advanced %v, want %v", tc.method, tc.path, tc.status, got, tc.counted)
		}
		if got := kicked(fired, tc.kick); got != tc.kick {
			t.Errorf("%s %s -> %d: kicked %v, want %v", tc.method, tc.path, tc.status, got, tc.kick)
		}
	}
}

// A key-store write is a config mutation an orphaned intent must be
// placeable against, but not a change the snapshot carries: it advances
// the generation and leaves config_dirty, and the write-through, alone.
func TestKeyStoreWriteCountsWithoutDirtyingSnapshot(t *testing.T) {
	fired := kickProbe(t)
	gen, dirty := snapshot.ConfigGeneration(), configDirtyGauge(t)
	serveWithStatus(http.MethodPost, "/netlox/v1/config/ai/tenant/ratelimit", http.StatusOK)
	if got := snapshot.ConfigGeneration(); got != gen+1 {
		t.Fatalf("generation %d after a tenant rate-limit write, want %d", got, gen+1)
	}
	if got := configDirtyGauge(t); got != dirty {
		t.Fatalf("config_dirty moved from %v to %v on a key-store write", dirty, got)
	}
	if kicked(fired, false) {
		t.Fatal("a key-store write kicked the write-through")
	}
	// The boundary on the other side: a login is not configuration.
	gen = snapshot.ConfigGeneration()
	serveWithStatus(http.MethodPost, "/netlox/v1/auth/login", http.StatusOK)
	if got := snapshot.ConfigGeneration(); got != gen {
		t.Fatalf("generation moved from %d to %d on a login", gen, got)
	}
}

var templateParam = regexp.MustCompile(`\{[^}]+\}`)

// Every snapshot-class route of the registry is eligible through its own
// template, so no snapshot write depends on the prefix fallback.
func TestAutoPersistEligibleForEverySnapshotRoute(t *testing.T) {
	for _, rl := range snapshot.RouteLifecycles {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Method = map[string]string{"post": "POST", "put": "PUT", "patch": "PATCH", "delete": "DELETE"}[rl.Method]
		r.URL.Path = apiBasePath + templateParam.ReplaceAllString(rl.Path, "v")
		class, ok := lifecycleClassOf(r)
		if !ok || class != rl.Class {
			t.Errorf("%s %s: matched %q (%v), want %q", rl.Method, rl.Path, class, ok, rl.Class)
		}
		if got := autoPersistEligible(r); got != (rl.Class == snapshot.ClassSnapshot) {
			t.Errorf("%s %s: eligible %v, class %s", rl.Method, rl.Path, got, rl.Class)
		}
		if counted, kick := configMutationScope(r); kick && !counted {
			t.Errorf("%s %s: kicks the write-through without advancing the generation", rl.Method, rl.Path)
		}
	}
}
