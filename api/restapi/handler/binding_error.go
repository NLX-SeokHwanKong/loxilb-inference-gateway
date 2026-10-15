package handler

import (
	"errors"
	apierrors "github.com/go-openapi/errors"
	"github.com/go-openapi/runtime"
	cmn "github.com/loxilb-io/loxilb/common"
	"net/http"
)

// ServeAPIError gives generated binding validation the same 400 envelope as
// validation performed by handlers. The default go-openapi renderer uses 422
// and an internal 600-series code for these identical caller-input errors.
func ServeAPIError(w http.ResponseWriter, r *http.Request, err error) {
	var typed apierrors.Error
	if errors.As(err, &typed) && (typed.Code() == http.StatusUnprocessableEntity || typed.Code() >= 600) {
		(&ErrorResponse{Payload: ResultErrorResponseError(&cmn.ValidationError{Err: err})}).WriteResponse(w, runtime.JSONProducer())
		return
	}
	apierrors.ServeError(w, r, err)
}
