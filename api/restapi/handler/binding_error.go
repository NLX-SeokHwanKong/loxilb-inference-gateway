package handler

import (
	"errors"
	apierrors "github.com/go-openapi/errors"
	"github.com/go-openapi/runtime"
	cmn "github.com/loxilb-io/loxilb/common"
	"net/http"
)

// ServeAPIError preserves the endpoint host-state POST's existing 400 contract
// after clean model generation. Other operations retain go-openapi's binding
// statuses (including 422 for invalid QoS scope enums).
func ServeAPIError(w http.ResponseWriter, r *http.Request, err error) {
	var typed apierrors.Error
	hostStatePost := r.Method == http.MethodPost &&
		(r.URL.Path == "/config/endpointhoststate" || r.URL.Path == "/netlox/v1/config/endpointhoststate")
	if hostStatePost && errors.As(err, &typed) && (typed.Code() == http.StatusUnprocessableEntity || typed.Code() >= 600) {
		(&ErrorResponse{Payload: ResultErrorResponseError(&cmn.ValidationError{Err: err})}).WriteResponse(w, runtime.JSONProducer())
		return
	}
	apierrors.ServeError(w, r, err)
}
