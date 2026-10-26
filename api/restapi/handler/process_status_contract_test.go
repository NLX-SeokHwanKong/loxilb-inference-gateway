/*
 * Copyright (c) 2026 NetLOX Inc
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
 */
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/go-openapi/runtime"
	"github.com/loxilb-io/loxilb/api/apiutils/status"
)

func TestProcessCollectionFailuresAreServerErrors(t *testing.T) {
	for _, cause := range []error{&os.PathError{Op: "read", Path: "/proc/123/stat", Err: syscall.ESRCH}, errors.New("invalid proc uptime"), errors.New("invalid clock tick rate"), os.ErrNotExist, syscall.EIO} {
		for _, err := range []error{&status.ProcessReadError{Err: cause}, fmt.Errorf("wrapped: %w", &status.ProcessReadError{Err: cause}), cause} {
			rec := httptest.NewRecorder()
			processStatusResponse(nil, err).WriteResponse(rec, runtime.JSONProducer())
			if rec.Code != 500 {
				t.Errorf("server cause %v returned %d", err, rec.Code)
			}
			var body struct{ Code int }
			if e := json.Unmarshal(rec.Body.Bytes(), &body); e != nil || body.Code != 500 {
				t.Errorf("invalid server error body: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "/proc/") {
				t.Error("internal path leaked into API error")
			}
		}
	}
}
