/*
 * Copyright (c) 2022 NetLOX Inc
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
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-openapi/runtime"
	"github.com/go-openapi/runtime/middleware"
	"github.com/loxilb-io/loxilb/api/models"
	"github.com/loxilb-io/loxilb/api/restapi/operations"
	cmn "github.com/loxilb-io/loxilb/common"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

type bfdContractHook struct {
	cmn.NetHookInterface
	rows []cmn.BFDMod
	err  error
}

func (h *bfdContractHook) NetBFDGet() ([]cmn.BFDMod, error)   { return h.rows, h.err }
func (h *bfdContractHook) NetBFDAdd(*cmn.BFDMod) (int, error) { return 0, h.err }
func (h *bfdContractHook) NetBFDDel(*cmn.BFDMod) (int, error) { return 0, h.err }

func TestBFDGetWireContract(t *testing.T) {
	old := ApiHooks
	t.Cleanup(func() { ApiHooks = old })
	for _, tc := range []struct {
		name string
		rows []cmn.BFDMod
		err  error
		code int
	}{
		{name: "idle empty list", code: 200},
		{name: "configured down session", rows: []cmn.BFDMod{{Instance: "default", RemoteIP: net.ParseIP("127.0.0.2"), SourceIP: net.ParseIP("127.0.0.1"), State: "BFDDown"}}, code: 200},
		{name: "uninitialized", err: fmt.Errorf("wrapped: %w", cmn.ErrBFDUnavailable), code: 503},
		{name: "unsupported", err: cmn.ErrBFDUnsupported, code: 403},
		{name: "actual failure stays failure", err: errors.New("unexpected hook fault"), code: 500},
		{name: "lookalike is not a sentinel", err: errors.New(cmn.ErrBFDUnavailable.Error()), code: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ApiHooks = &bfdContractHook{rows: tc.rows, err: tc.err}
			req := httptest.NewRequest(http.MethodGet, "/netlox/v1/config/bfd/all", nil)
			rec := httptest.NewRecorder()
			ConfigGetBFDSession(operations.GetConfigBfdAllParams{HTTPRequest: req}, nil).WriteResponse(rec, runtime.JSONProducer())
			if rec.Code != tc.code {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if tc.code == 200 {
				var body struct{ Attr []json.RawMessage }
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Attr == nil || len(body.Attr) != len(tc.rows) {
					t.Fatalf("wire array=%s", rec.Body.String())
				}
			}
		})
	}
}

func TestBFDMutationUnavailableStatus(t *testing.T) {
	old := ApiHooks
	t.Cleanup(func() { ApiHooks = old })
	for _, tc := range []struct {
		err  error
		code int
	}{{cmn.ErrBFDUnavailable, 503}, {cmn.ErrBFDUnsupported, 403}, {errors.New("unknown failure"), 500}} {
		ApiHooks = &bfdContractHook{err: tc.err}
		req := httptest.NewRequest(http.MethodPost, "/netlox/v1/config/bfd", nil)
		calls := []middleware.Responder{
			ConfigPostBFDSession(operations.PostConfigBfdParams{HTTPRequest: req, Attr: &models.BfdEntry{}}, nil),
			ConfigDeleteBFDSession(operations.DeleteConfigBfdRemoteIPRemoteIPParams{HTTPRequest: req, RemoteIP: "127.0.0.2"}, nil),
		}
		for _, resp := range calls {
			if code := renderStatus(t, resp); code != tc.code {
				t.Fatalf("got%d want%d", code, tc.code)
			}
		}
	}
}
