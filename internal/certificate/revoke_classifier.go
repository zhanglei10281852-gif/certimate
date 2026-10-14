package certificate

import (
	"context"
	"errors"
	"net"

	"github.com/go-acme/lego/v5/acme"
)

// revokeOutcome is the conservative classification of an ACME revocation
// call. Anything that cannot be proven to be a definitive success or a
// definitive 4xx rejection falls into "unknown" so that the system never
// blindly resends the request.
type revokeOutcome string

const (
	revokeOutcomeSucceeded revokeOutcome = "succeeded"
	revokeOutcomeFailed    revokeOutcome = "failed"
	revokeOutcomeUnknown   revokeOutcome = "unknown"
)

const (
	revokeErrorTypeTimeout     = "timeout"
	revokeErrorTypeCanceled    = "canceled"
	revokeErrorTypeNetwork     = "network"
	revokeErrorTypeRejected    = "rejected"
	revokeErrorTypeRemote      = "remote"
	revokeErrorTypeInterrupted = "interrupted"
	revokeErrorTypeLocal       = "local"
	revokeErrorTypeUnknown     = "unknown"
)

type revokeErrorResult struct {
	outcome    revokeOutcome
	errorType  string
	statusCode int
	message    string
}

// classifyRevokeError classifies the error returned by the ACME revocation
// call:
//   - nil                                        -> succeeded
//   - acme alreadyRevoked problem                -> succeeded (already revoked remotely)
//   - acme ProblemDetails with a 4xx HTTP status -> failed (explicit rejection)
//   - everything else (5xx, timeout, network, ..)-> unknown (remote result not provable)
func classifyRevokeError(err error) revokeErrorResult {
	if err == nil {
		return revokeErrorResult{outcome: revokeOutcomeSucceeded}
	}

	var problem *acme.ProblemDetails
	if errors.As(err, &problem) && problem != nil {
		if problem.Type == acme.AlreadyRevokedErrorType {
			return revokeErrorResult{
				outcome:    revokeOutcomeSucceeded,
				errorType:  acme.AlreadyRevokedErrorType,
				statusCode: problem.HTTPStatus,
				message:    problem.Error(),
			}
		}

		if problem.HTTPStatus >= 400 && problem.HTTPStatus < 500 {
			return revokeErrorResult{
				outcome:    revokeOutcomeFailed,
				errorType:  nonEmpty(problem.Type, revokeErrorTypeRejected),
				statusCode: problem.HTTPStatus,
				message:    problem.Error(),
			}
		}

		return revokeErrorResult{
			outcome:    revokeOutcomeUnknown,
			errorType:  nonEmpty(problem.Type, revokeErrorTypeRemote),
			statusCode: problem.HTTPStatus,
			message:    problem.Error(),
		}
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return revokeErrorResult{outcome: revokeOutcomeUnknown, errorType: revokeErrorTypeTimeout, message: err.Error()}
	case errors.Is(err, context.Canceled):
		return revokeErrorResult{outcome: revokeOutcomeUnknown, errorType: revokeErrorTypeCanceled, message: err.Error()}
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return revokeErrorResult{outcome: revokeOutcomeUnknown, errorType: revokeErrorTypeNetwork, message: err.Error()}
	}

	return revokeErrorResult{outcome: revokeOutcomeUnknown, errorType: revokeErrorTypeUnknown, message: err.Error()}
}

// classifyLocalError classifies an error that occurred before the ACME
// revocation endpoint was reached (account lookup, client initialization).
// The CA was never contacted, so the attempt is a definitive failure that the
// admin can retry through the regular entry point.
func classifyLocalError(err error) revokeErrorResult {
	if err == nil {
		return revokeErrorResult{outcome: revokeOutcomeSucceeded}
	}
	return revokeErrorResult{
		outcome:   revokeOutcomeFailed,
		errorType: revokeErrorTypeLocal,
		message:   err.Error(),
	}
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
