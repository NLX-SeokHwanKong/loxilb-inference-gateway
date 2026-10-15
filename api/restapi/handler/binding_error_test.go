package handler

import (
	apierrors "github.com/go-openapi/errors"
	"net/http/httptest"
	"strconv"
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
