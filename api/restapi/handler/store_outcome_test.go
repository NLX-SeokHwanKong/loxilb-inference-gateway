package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loxilb-io/loxilb/api/restapi/operations"
	cmn "github.com/loxilb-io/loxilb/common"
)

// The write-outcome contract on the wire: a store that was never reached
// is 503 (safe to retry), a store that was handed the statement and did
// not answer is 504 with its own code (read back before retrying), and the
// second is tested before the first because the error chain carries both
// the outcome sentinel and the cause.
func TestKeyStoreFailureDistinguishesNotSentFromOutcomeUnknown(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{"unconfigured", cmn.ErrKeyStoreUnconfigured, http.StatusServiceUnavailable, "ai_key_store_unconfigured"},
		{"unavailable", cmn.ErrDBUnavailable, http.StatusServiceUnavailable, "ai_key_store_unavailable"},
		{"unavailable, wrapped", fmt.Errorf("aikey: %w: dial", cmn.ErrDBUnavailable), http.StatusServiceUnavailable, "ai_key_store_unavailable"},
		{"outcome unknown", cmn.ErrStoreOutcomeUnknown, http.StatusGatewayTimeout, keyStoreOutcomeUnknownCode},
		{"outcome unknown, wrapped with a timeout cause", fmt.Errorf("%w: %w", cmn.ErrStoreOutcomeUnknown, context.DeadlineExceeded), http.StatusGatewayTimeout, keyStoreOutcomeUnknownCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := keyStoreFailure(tc.err)
			if resp == nil {
				t.Fatalf("keyStoreFailure(%v) = nil", tc.err)
			}
			if int(resp.Payload.Code) != tc.wantCode || resp.Payload.Message != tc.wantMsg {
				t.Fatalf("keyStoreFailure(%v) = %d %q, want %d %q", tc.err, resp.Payload.Code, resp.Payload.Message, tc.wantCode, tc.wantMsg)
			}
			rec := httptest.NewRecorder()
			if !writeKeyStoreFailure(rec, tc.err) || rec.Code != tc.wantCode {
				t.Fatalf("raw arm answered %d, want %d", rec.Code, tc.wantCode)
			}
		})
	}
	if keyStoreFailure(errors.New("some other failure")) != nil {
		t.Fatal("an unrelated error was mapped as a store failure")
	}
}

// The management plane's handlers classify on the message: the
// outcome-unknown wording must come out as 504 even though the message also
// names the store, which the 503 rule matches.
func TestErrorMessageMapperReportsOutcomeUnknownAs504(t *testing.T) {
	unknown := fmt.Errorf("%w: %w", cmn.ErrStoreOutcomeUnknown, context.DeadlineExceeded)
	if got := ResultErrorResponseErrorMessage(unknown.Error()); got.Code != 504 || got.Result != "store_outcome_unknown" {
		t.Fatalf("outcome unknown mapped to %d %q", got.Code, got.Result)
	}
	unavailable := fmt.Errorf("%w: %w", cmn.ErrDBUnavailable, context.DeadlineExceeded)
	if got := ResultErrorResponseErrorMessage(unavailable.Error()); got.Code != 503 {
		t.Fatalf("unavailable mapped to %d", got.Code)
	}
}

// blockingDepHook is a readiness dependency that answers only when the
// probe's context ends -- a store that keeps the connection open and says
// nothing. The probe must carry the request's context, so the handler
// answers within the client's window instead of hanging with it.
type blockingDepHook struct {
	cmn.NetHookInterface
	sawDeadline bool
}

func (h *blockingDepHook) NetRecoveryDepsGet() ([]cmn.RecoveryDependency, error) {
	return []cmn.RecoveryDependency{{Type: cmn.RecoveryDepAPIKeyDB, ID: "keys", Required: true}}, nil
}

func (h *blockingDepHook) NetRecoveryDepReady(ctx context.Context, _ string) error {
	_, h.sawDeadline = ctx.Deadline()
	<-ctx.Done()
	return fmt.Errorf("data-plane API-key store not answering: %w", ctx.Err())
}

func (h *blockingDepHook) NetEbpfAttachmentGet() ([]cmn.EbpfAttachmentDump, error) { return nil, nil }
func (h *blockingDepHook) NetAiInFlightStreamsGet() int64                          { return 0 }

func TestDependencyProbesRunUnderTheRequestContext(t *testing.T) {
	prev := ApiHooks
	hook := &blockingDepHook{}
	ApiHooks = hook
	t.Cleanup(func() { ApiHooks = prev })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/netlox/v1/diagnostics", nil).WithContext(ctx)
	start := time.Now()
	resp := ConfigGetDiagnostics(operations.GetDiagnosticsParams{HTTPRequest: req}, nil)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("diagnostics hung for %v on a stalled dependency", took)
	}
	if !hook.sawDeadline {
		t.Fatal("the dependency probe ran without the request's deadline")
	}
	ok, isOK := resp.(*operations.GetDiagnosticsOK)
	if !isOK {
		t.Fatalf("diagnostics returned %T", resp)
	}
	if ok.Payload == nil || ok.Payload.Ready == nil || *ok.Payload.Ready || len(ok.Payload.ExternalDependencies) != 1 {
		t.Fatalf("a stalled required dependency left the gateway ready: %+v", ok.Payload)
	}
}
