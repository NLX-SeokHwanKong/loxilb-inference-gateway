package handler

import (
	"encoding/json"
	"github.com/go-openapi/runtime"
	"github.com/go-openapi/runtime/middleware"
	"github.com/go-openapi/strfmt"
	"github.com/loxilb-io/loxilb/api/models"
	"github.com/loxilb-io/loxilb/api/restapi/operations"
	cmn "github.com/loxilb-io/loxilb/common"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type hostStateScopeHook struct {
	cmn.NetHookInterface
	calls int
	port  uint16
	proto string
}

func (h *hostStateScopeHook) NetEpHostStateSet(m *cmn.EndPointHostMod) (int, error) {
	h.calls++
	h.port = m.EPPort
	h.proto = m.EPProto
	return 0, nil
}
func TestHostStateRejectsInvalidPortBeforeHook(t *testing.T) {
	previous := ApiHooks
	defer func() { ApiHooks = previous }()
	hook := &hostStateScopeHook{}
	ApiHooks = hook
	for _, body := range []*models.EndPointHostState{
		nil, {EpPort: -1, EpProto: "tcp"}, {EpPort: 65536, EpProto: "tcp"},
		{EpPort: 4294967297, EpProto: "tcp"}, {EpPort: 8201, EpProto: "unknown"},
		{EpPort: 8201}, {EpProto: "tcp"},
	} {
		result := ConfigPostEndPointHostState(operations.PostConfigEndpointhoststateParams{
			HTTPRequest: httptest.NewRequest(http.MethodPost, "/config/endpointhoststate", nil), Attr: body,
		}, nil)
		if _, ok := result.(*ErrorResponse); !ok {
			t.Fatalf("invalid tuple %+v returned %T", body, result)
		}
		recorder := httptest.NewRecorder()
		result.WriteResponse(recorder, runtime.JSONProducer())
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid tuple %+v returned HTTP %d: %s", body, recorder.Code, recorder.Body.String())
		}
		var rendered models.Error
		if err := json.Unmarshal(recorder.Body.Bytes(), &rendered); err != nil {
			t.Fatal(err)
		}
		if rendered.Code != http.StatusBadRequest || rendered.Result == "" {
			t.Fatalf("missing caller-facing validation refusal: %+v", rendered)
		}
		if hook.calls != 0 {
			t.Fatalf("invalid tuple reached dataplane hook: %+v", body)
		}
	}
	for _, port := range []int64{0, 1, 65535} {
		proto := "tcp"
		if port == 0 {
			proto = ""
		}
		result := ConfigPostEndPointHostState(operations.PostConfigEndpointhoststateParams{
			HTTPRequest: httptest.NewRequest(http.MethodPost, "/config/endpointhoststate", nil),
			Attr:        &models.EndPointHostState{HostName: "127.0.0.1", EpPort: port, EpProto: proto, State: "green"},
		}, nil)
		if _, ok := result.(*ResultResponse); !ok {
			t.Fatalf("valid port %d returned %T", port, result)
		}
		if hook.port != uint16(port) || hook.proto != proto {
			t.Fatalf("tuple changed before hook: %d/%q", hook.port, hook.proto)
		}
	}
	if hook.calls != 3 {
		t.Fatalf("valid hook calls=%d", hook.calls)
	}
}

// Exercise the generated binder and actual configured error renderer. Handler
// tests alone miss validation failures that are answered before dispatch.
func TestHostStateBindingRendersInvalidInput400(t *testing.T) {
	previous := ApiHooks
	defer func() { ApiHooks = previous }()
	hook := &hostStateScopeHook{}
	ApiHooks = hook
	route := &middleware.MatchedRoute{Consumer: runtime.JSONConsumer()}
	route.Formats = strfmt.Default
	for _, body := range []string{
		`{"epPort":8201}`, `{"epProto":"tcp"}`, `{"epPort":0,"epProto":"tcp"}`,
		`{"epPort":-1,"epProto":"tcp"}`, `{"epPort":65536,"epProto":"tcp"}`,
		`{"epPort":4294967297,"epProto":"tcp"}`, `{"epPort":8201,"epProto":"unknown"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/config/endpointhoststate", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		params := operations.NewPostConfigEndpointhoststateParams()
		err := params.BindRequest(request, route)

		recorder := httptest.NewRecorder()
		tupleOnly := body == `{"epPort":8201}` || body == `{"epProto":"tcp"}` || body == `{"epPort":0,"epProto":"tcp"}`
		if err == nil {
			if !tupleOnly {
				t.Fatalf("binder accepted invalid range/enum: %s", body)
			}
			// Swagger 2 cannot express this cross-field tuple constraint.
			// The actual handler must refuse it before the state hook.
			ConfigPostEndPointHostState(params, nil).WriteResponse(recorder, runtime.JSONProducer())
		} else {
			if params.Attr != nil {
				t.Fatalf("invalid range/enum published attributes: %s", body)
			}
			ServeAPIError(recorder, request, err)
		}
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("binder error for %s rendered HTTP %d: %s", body, recorder.Code, recorder.Body.String())
		}
		var rendered models.Error
		if err := json.Unmarshal(recorder.Body.Bytes(), &rendered); err != nil {
			t.Fatal(err)
		}
		if rendered.Code != http.StatusBadRequest {
			t.Fatalf("binder error JSON status %+v", rendered)
		}
		if hook.calls != 0 {
			t.Fatal("invalid binding reached state hook")
		}
	}
	for _, body := range []string{
		`{"hostName":"127.0.0.1","state":"green"}`,
		`{"hostName":"127.0.0.1","state":"green","epPort":65535,"epProto":"tcp"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/config/endpointhoststate", strings.NewReader(body))
		params := operations.NewPostConfigEndpointhoststateParams()
		if err := params.BindRequest(request, route); err != nil {
			t.Fatalf("binder rejected valid input: %v", err)
		}
		result := ConfigPostEndPointHostState(params, nil)
		recorder := httptest.NewRecorder()
		result.WriteResponse(recorder, runtime.JSONProducer())
		if recorder.Code != http.StatusOK {
			t.Fatalf("valid bound input rendered HTTP %d", recorder.Code)
		}
	}
	if hook.calls != 2 || hook.port != 65535 {
		t.Fatalf("valid binding tuple lost: %+v", hook)
	}
}
