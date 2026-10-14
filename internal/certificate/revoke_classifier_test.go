package certificate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/go-acme/lego/v5/acme"
)

type fakeNetError struct{}

func (fakeNetError) Error() string   { return "connection refused" }
func (fakeNetError) Timeout() bool   { return false }
func (fakeNetError) Temporary() bool { return true }

var _ net.Error = fakeNetError{}

func TestClassifyRevokeError(t *testing.T) {
	alreadyRevoked := &acme.ProblemDetails{
		Type:       acme.AlreadyRevokedErrorType,
		HTTPStatus: 400,
		Detail:     "already revoked",
	}
	fourHundred := &acme.ProblemDetails{
		Type:       acme.UnauthorizedErrorType,
		HTTPStatus: 403,
		Detail:     "unauthorized",
	}
	badReason := &acme.ProblemDetails{
		Type:       acme.BadRevocationReasonErrorType,
		HTTPStatus: 400,
		Detail:     "bad reason",
	}
	serverInternal := &acme.ProblemDetails{
		Type:       acme.ServerInternalErrorType,
		HTTPStatus: 500,
		Detail:     "boom",
	}
	zeroStatus := &acme.ProblemDetails{
		Type:   acme.ServerInternalErrorType,
		Detail: "no status",
	}

	cases := []struct {
		name    string
		err     error
		want    revokeOutcome
		wantTyp string
	}{
		{"nil", nil, revokeOutcomeSucceeded, ""},
		{"already revoked", alreadyRevoked, revokeOutcomeSucceeded, acme.AlreadyRevokedErrorType},
		{"already revoked wrapped", fmt.Errorf("wrap: %w", alreadyRevoked), revokeOutcomeSucceeded, acme.AlreadyRevokedErrorType},
		{"403 unauthorized", fourHundred, revokeOutcomeFailed, acme.UnauthorizedErrorType},
		{"400 bad revocation reason", badReason, revokeOutcomeFailed, acme.BadRevocationReasonErrorType},
		{"403 wrapped", fmt.Errorf("failed to revoke: %w", fourHundred), revokeOutcomeFailed, acme.UnauthorizedErrorType},
		{"500 server internal", serverInternal, revokeOutcomeUnknown, acme.ServerInternalErrorType},
		{"zero http status", zeroStatus, revokeOutcomeUnknown, acme.ServerInternalErrorType},
		{"deadline exceeded", context.DeadlineExceeded, revokeOutcomeUnknown, revokeErrorTypeTimeout},
		{"deadline wrapped", fmt.Errorf("do request: %w", context.DeadlineExceeded), revokeOutcomeUnknown, revokeErrorTypeTimeout},
		{"canceled", context.Canceled, revokeOutcomeUnknown, revokeErrorTypeCanceled},
		{"network error", fakeNetError{}, revokeOutcomeUnknown, revokeErrorTypeNetwork},
		{"network wrapped", fmt.Errorf("Post: %w", fakeNetError{}), revokeOutcomeUnknown, revokeErrorTypeNetwork},
		{"eof", io.ErrUnexpectedEOF, revokeOutcomeUnknown, revokeErrorTypeUnknown},
		{"plain error", errors.New("something strange"), revokeOutcomeUnknown, revokeErrorTypeUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRevokeError(tc.err)
			if got.outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (type=%q)", got.outcome, tc.want, got.errorType)
			}
			if tc.wantTyp != "" && got.errorType != tc.wantTyp {
				t.Fatalf("errorType = %q, want %q", got.errorType, tc.wantTyp)
			}
			if tc.err != nil && got.message == "" {
				t.Fatalf("expected non-empty diagnostic message")
			}
		})
	}
}

func TestClassifyLocalError(t *testing.T) {
	if got := classifyLocalError(nil); got.outcome != revokeOutcomeSucceeded {
		t.Fatalf("nil local error = %q, want succeeded", got.outcome)
	}

	got := classifyLocalError(errors.New("account missing"))
	if got.outcome != revokeOutcomeFailed || got.errorType != revokeErrorTypeLocal {
		t.Fatalf("local error = (%q, %q), want (failed, local)", got.outcome, got.errorType)
	}
}
