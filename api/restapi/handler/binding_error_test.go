package handler

import (
	apierrors "github.com/go-openapi/errors"
	"github.com/go-openapi/runtime"
	"github.com/go-openapi/runtime/middleware"
	"github.com/go-openapi/strfmt"
	"github.com/loxilb-io/loxilb/api/restapi/operations/ai"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestBindingRendererPreservesNonValidationStatus(t *testing.T) {
	for _, status := range []int{401, 403, 404, 405, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ServeAPIError(recorder, httptest.NewRequest("GET", "/", nil), apierrors.New(int32(status), "test failure"))
			if recorder.Code != status {
				t.Fatalf("non-validation status rewritten: got %d want %d", recorder.Code, status)
			}
		})
	}
}

func TestBindingRendererPreservesQoSScopeValidation422(t *testing.T) {
	for _, method := range []string{"POST", "GET"} {
		t.Run(method, func(t *testing.T) {
			path := "/netlox/v1/config/ai/ratelimit/defaults"
			if method == "GET" {
				path += "/bogus"
			}
			request := httptest.NewRequest(method, path, strings.NewReader(`{"scope":"bogus","default_user_rps":1}`))
			request.Header.Set("Content-Type", "application/json")
			route := &middleware.MatchedRoute{Consumer: runtime.JSONConsumer(), Params: middleware.RouteParams{{Name: "scope", Value: "bogus"}}}
			route.Formats = strfmt.Default
			var err error
			if method == "POST" {
				p := ai.NewPostConfigAiRatelimitDefaultsParams()
				err = p.BindRequest(request, route)
			} else {
				p := ai.NewGetConfigAiRatelimitDefaultsScopeParams()
				err = p.BindRequest(request, route)
			}
			if err == nil {
				t.Fatal("invalid scope reached dispatch")
			}
			recorder := httptest.NewRecorder()
			ServeAPIError(recorder, request, err)
			if recorder.Code != 422 {
				t.Fatalf("QoS binding contract changed: HTTP %d, want 422", recorder.Code)
			}
		})
	}
}

func TestBindingRendererLimitsHostState400ToDeclaredPost(t *testing.T) {
	for _, path := range []string{"/config/endpointhoststate", "/netlox/v1/config/endpointhoststate"} {
		for _, method := range []string{"POST", "GET"} {
			t.Run(method+path, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ServeAPIError(recorder, httptest.NewRequest(method, path, nil), apierrors.New(422, "invalid port"))
				want := 422
				if method == "POST" {
					want = 400
				}
				if recorder.Code != want {
					t.Fatalf("status=%d want=%d", recorder.Code, want)
				}
			})
		}
	}
}
