// CPU-only UC-7 backend: reflect nonce, record key presence and stream usage/DONE.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
		return
	}
	nonce := r.Header.Get("X-Edge-Nonce")
	// No key value is logged. A present key still fails the receipt assertion.
	keyPresent := r.Header.Get("X-Api-Key") != "" || r.Header.Get("Authorization") != ""
	_ = json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"event": "uc7_receipt", "nonce": nonce, "key_present": keyPresent, "proto": r.Proto, "path": r.URL.Path})
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body read failed", http.StatusBadRequest)
		return
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	if request.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		first, _ := json.Marshal(map[string]interface{}{"id": "uc7-fixture", "object": "text_completion", "choices": []interface{}{map[string]interface{}{"text": nonce, "index": 0, "finish_reason": nil}}})
		fmt.Fprintf(w, "data: %s\n\n", first)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(20 * time.Millisecond)
		last, _ := json.Marshal(map[string]interface{}{"id": "uc7-fixture", "choices": []interface{}{map[string]interface{}{"text": "", "index": 0, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", last)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "X-Echo-Backend: uc7-backend")
	keys := make([]string, 0, len(r.Header))
	for key := range r.Header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		// Presence is recorded above; credentials are never reflected into responses.
		if strings.EqualFold(key, "X-Api-Key") || strings.EqualFold(key, "Authorization") {
			continue
		}
		for _, value := range r.Header.Values(key) {
			fmt.Fprintf(w, "%s: %s\n", key, value)
		}
	}
	fmt.Fprintf(w, "Host: %s\n", r.Host)
}

func main() {
	server := &http.Server{Addr: ":80", Handler: h2c.NewHandler(http.HandlerFunc(handler), &http2.Server{}), ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
