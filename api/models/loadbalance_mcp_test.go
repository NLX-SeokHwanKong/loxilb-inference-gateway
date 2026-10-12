/*
 * Copyright (c) 2026 NetLOX Inc
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy at http://www.apache.org/licenses/LICENSE-2.0
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 */

package models

import (
	"encoding/json"
	"github.com/go-openapi/strfmt"
	"testing"
)

func TestMCPProtocolVersionModelValidation(t *testing.T) {
	for _, c := range []struct {
		raw    string
		reject bool
	}{
		{`{}`, false}, {`{"mcp_protocol_version":""}`, false}, {`{"mcp_protocol_version":"2026-07-28"}`, false},
		{`{"mcp_protocol_version":"future"}`, true}, {`{"mcp_protocol_version":20260728}`, true},
		{`{"mcp_protocol_version":["2026-07-28"]}`, true},
	} {
		var m LoadbalanceEntryServiceArguments
		err := json.Unmarshal([]byte(c.raw), &m)
		if err == nil {
			err = m.Validate(strfmt.Default)
		}
		if (err != nil) != c.reject {
			t.Fatalf("%s rejected=%v want %v: %v", c.raw, err != nil, c.reject, err)
		}
	}
}
