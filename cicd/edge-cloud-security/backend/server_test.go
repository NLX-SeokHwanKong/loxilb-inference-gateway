package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamHasUsageDoneAndNonce(t *testing.T) {
	r := httptest.NewRequest("POST", "http://backend/v1/completions", strings.NewReader(`{"stream":true}`))
	r.Header.Set("X-Edge-Nonce", "fixture-stream")
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("missing SSE response")
	}
	body := w.Body.String()
	for _, want := range []string{"fixture-stream", `"usage"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s", want)
		}
	}
}

func TestCredentialsAreNotReflected(t *testing.T) {
	r := httptest.NewRequest("POST", "http://backend/v1/completions", strings.NewReader(`{}`))
	r.Header.Set("X-Api-Key", "fixture-key-should-stay-private")
	r.Header.Set("Authorization", "Bearer fixture-token-should-stay-private")
	r.Header.Set("X-Edge-Nonce", "fixture-nonce")
	w := httptest.NewRecorder()
	handler(w, r)
	body := w.Body.String()
	if strings.Contains(body, "should-stay-private") {
		t.Fatal("credential reflected")
	}
	if !strings.Contains(body, "fixture-nonce") {
		t.Fatal("nonce missing")
	}
}
