package certificate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/certimate-go/certimate/internal/certacme"
	"github.com/certimate-go/certimate/internal/domain"
	"github.com/certimate-go/certimate/internal/domain/dtos"
	"github.com/pocketbase/pocketbase/tools/security"
)

const (
	// revokeCallTimeout bounds the detached ACME revocation call. A timeout is
	// persisted as an unknown outcome rather than retried blindly.
	revokeCallTimeout = 60 * time.Second
	// revokeOwnerHeartbeatStaleness decides whether a processing attempt owned
	// by the current instance is still alive.
	revokeOwnerHeartbeatStaleness = 2 * time.Minute
)

// revokeCall is the prepared, single-use revocation request. It performs the
// actual CA call; implementations must not do account/client preparation here.
type revokeCall func(ctx context.Context) error

// revokeInvoker separates the pre-call local preparation (which is safe to
// retry) from the actual CA call (which must never be sent twice blindly).
type revokeInvoker interface {
	// Prepare resolves the ACME account and client without contacting the
	// revocation endpoint. The returned revokeCall performs the endpoint call.
	Prepare(ctx context.Context, certificate *domain.Certificate) (revokeCall, error)
}

// revokeLocalError marks an error that occurred before the CA was contacted.
type revokeLocalError struct {
	err error
}

func (e *revokeLocalError) Error() string {
	return e.err.Error()
}

func (e *revokeLocalError) Unwrap() error {
	return e.err
}

// acmeRevokeInvoker is the production revokeInvoker.
type acmeRevokeInvoker struct {
	acmeAccountRepo acmeAccountRepository
}

func newACMERevokeInvoker(acmeAccountRepo acmeAccountRepository) *acmeRevokeInvoker {
	return &acmeRevokeInvoker{acmeAccountRepo: acmeAccountRepo}
}

func (i *acmeRevokeInvoker) Prepare(ctx context.Context, certificate *domain.Certificate) (revokeCall, error) {
	acmeAccount, err := i.acmeAccountRepo.GetByCAAndAcctUrl(ctx, certificate.CA, certificate.ACMEAccountUrl)
	if err != nil {
		return nil, &revokeLocalError{err: fmt.Errorf("could not find acme account: %w", err)}
	}

	acmeClient, err := certacme.NewACMEClientWithAccount(acmeAccount)
	if err != nil {
		return nil, &revokeLocalError{err: fmt.Errorf("could not initialize acme config: %w", err)}
	}

	return func(callCtx context.Context) error {
		_, err := acmeClient.RevokeCertificate(callCtx, &certacme.RevokeCertificateRequest{
			Certificate: certificate.Certificate,
		})
		return err
	}, nil
}

var (
	ownerMu      sync.Mutex
	ownerIDValue string
)

// currentOwnerID returns the process-unique owner identifier persisted on
// revocation attempts.
func currentOwnerID() string {
	ownerMu.Lock()
	defer ownerMu.Unlock()

	if ownerIDValue == "" {
		hostname, _ := os.Hostname()
		ownerIDValue = fmt.Sprintf("%s#%d#%s", hostname, os.Getpid(), security.PseudorandomString(8))
	}
	return ownerIDValue
}

// setOwnerIDForTest overrides the owner identifier; tests only.
func setOwnerIDForTest(id string) {
	ownerMu.Lock()
	defer ownerMu.Unlock()
	ownerIDValue = id
}

// RevokeCertificate runs the persisted, de-duplicated revocation workflow.
// Concurrent requests for the same certificate share one attempt and observe
// the same terminal result. The ACME call runs on a context detached from the
// HTTP request so that client disconnects cannot abort an in-flight attempt
// without persisting its outcome.
func (s *CertificateService) RevokeCertificate(ctx context.Context, req *dtos.CertificateRevokeReq) (*dtos.CertificateRevokeResp, error) {
	if req == nil || req.CertificateId == "" {
		return nil, domain.ErrInvalidParams
	}
	// The CA call is detached from the HTTP request lifetime: a client page
	// closing or timing out must not abort an in-flight attempt before its
	// outcome can be persisted. The detached call itself is bounded by
	// revokeCallTimeout.
	bgCtx := context.WithoutCancel(ctx)

	v, err, _ := s.revokeFlights.Do(req.CertificateId, func() (ret any, retErr error) {
		// convert a panic into a regular error: waiters share one ordinary
		// failure instead of re-panicking, and the singleflight key completes
		// its normal lifecycle
		defer func() {
			if r := recover(); r != nil {
				slog.Default().Error("panic while revoking certificate",
					slog.String("certificateId", req.CertificateId), slog.Any("recover", r))
				retErr = fmt.Errorf("certificate revocation panicked: %v", r)
			}
		}()
		return s.runRevocation(bgCtx, req.CertificateId, req.Confirm)
	})
	if err != nil {
		return nil, err
	}

	resp, _ := v.(*dtos.CertificateRevokeResp)
	return resp, nil
}

func (s *CertificateService) runRevocation(ctx context.Context, certificateId string, confirm bool) (*dtos.CertificateRevokeResp, error) {
	attempt, err := s.revocationRepo.GetLatestByCertificateId(ctx, certificateId)
	if err != nil && !domain.IsRecordNotFoundError(err) {
		return nil, err
	}

	certificate, err := s.certificateRepo.GetById(ctx, certificateId)
	if err != nil {
		// a certificate deleted after the CA succeeded is treated as converged
		if domain.IsRecordNotFoundError(err) && attempt != nil &&
			attempt.Status == domain.CertificateRevocationStatusTypeSucceeded.String() && !attempt.Converged {
			if convErr := s.markAttemptConverged(ctx, attempt); convErr != nil {
				return nil, convErr
			}
			return buildRevokeResp(attempt), nil
		}
		// a processing attempt whose certificate no longer exists must be
		// closed, otherwise startup recovery would enqueue it forever
		if domain.IsRecordNotFoundError(err) && attempt != nil &&
			attempt.Status == domain.CertificateRevocationStatusTypeProcessing.String() {
			s.closeProcessingAttemptForMissingCertificate(ctx, attempt)
		}
		return nil, err
	}

	if certificate.ACMEAccountUrl == "" || certificate.ACMECertificateUrl == "" {
		return nil, fmt.Errorf("could not revoke a certificate which is not issued in Certimate")
	}

	// A succeeded attempt is authoritative: if local convergence has not yet
	// been completed, finish it locally without ever contacting the CA again.
	if attempt != nil && attempt.Status == domain.CertificateRevocationStatusTypeSucceeded.String() {
		if !attempt.Converged {
			if err := s.convergeRevokedCertificate(ctx, certificate, attempt); err != nil {
				return nil, err
			}
			return buildRevokeResp(attempt), nil
		}
		// fully converged: fall through to the existing "already revoked"
		// rejection semantics below
	}

	if certificate.IsRevoked {
		return nil, fmt.Errorf("could not revoke a certificate which is already revoked")
	}

	if attempt != nil {
		switch attempt.Status {
		case domain.CertificateRevocationStatusTypeProcessing.String():
			return s.handleProcessingAttempt(ctx, certificate, attempt)

		case domain.CertificateRevocationStatusTypeFailed.String():
			// definitive rejection: the regular entry point starts a new attempt
			return s.startNewAttempt(ctx, certificate)

		case domain.CertificateRevocationStatusTypeUnknown.String():
			if !confirm {
				return nil, buildUnknownOutcomeError(attempt)
			}
			return s.startNewAttempt(ctx, certificate)
		}
	}

	return s.startNewAttempt(ctx, certificate)
}

// handleProcessingAttempt decides what to do with an in-flight attempt
// discovered at the start of the workflow.
func (s *CertificateService) handleProcessingAttempt(ctx context.Context, certificate *domain.Certificate, attempt *domain.CertificateRevocation) (*dtos.CertificateRevokeResp, error) {
	heartbeatFresh := attempt.Owner == currentOwnerID() && time.Since(attempt.Heartbeat) < revokeOwnerHeartbeatStaleness
	if heartbeatFresh {
		// Same live owner inside the singleflight flight is unreachable in
		// normal operation; stay conservative and never send a second request.
		return nil, fmt.Errorf("a revocation request for this certificate is already in progress")
	}

	switch attempt.Stage {
	case domain.CertificateRevocationStageTypePrepared.String():
		// owner exited before the CA was ever contacted: safe to take over
		slog.Default().Info("take over an interrupted revocation attempt before the CA was called",
			slog.String("certificateId", certificate.Id), slog.String("attemptId", attempt.Id), slog.String("previousOwner", attempt.Owner))
		attempt.Owner = currentOwnerID()
		attempt.Heartbeat = time.Now()
		if _, err := s.revocationRepo.Save(ctx, attempt); err != nil {
			return nil, err
		}
		return s.executeAttempt(ctx, certificate, attempt)

	default: // stage=calling or anything unexpected: remote result is unknown
		markAttemptUnknown(attempt, revokeErrorTypeInterrupted, 0,
			"the previous owner was interrupted while the revocation request was in flight; the remote result is unknown")
		if _, err := s.revocationRepo.Save(ctx, attempt); err != nil {
			return nil, err
		}
		return nil, buildUnknownOutcomeError(attempt)
	}
}

func (s *CertificateService) startNewAttempt(ctx context.Context, certificate *domain.Certificate) (*dtos.CertificateRevokeResp, error) {
	now := time.Now()
	attempt := &domain.CertificateRevocation{
		CertificateRef: certificate.Id,
		Status:         domain.CertificateRevocationStatusTypeProcessing.String(),
		Stage:          domain.CertificateRevocationStageTypePrepared.String(),
		Owner:          currentOwnerID(),
		Heartbeat:      now,
		StartedAt:      now,
	}

	persisted, created, err := s.revocationRepo.CreateProcessingIfAbsent(ctx, attempt)
	if err != nil {
		return nil, err
	}
	if !created {
		// a processing attempt appeared concurrently; observe/take it over
		return s.handleProcessingAttempt(ctx, certificate, persisted)
	}

	return s.executeAttempt(ctx, certificate, persisted)
}

// executeAttempt performs the actual revocation for an already-persisted
// processing attempt. The attempt record exists and is in stage=prepared when
// this is entered; nothing calls the CA before this persistence.
func (s *CertificateService) executeAttempt(ctx context.Context, certificate *domain.Certificate, attempt *domain.CertificateRevocation) (*dtos.CertificateRevokeResp, error) {
	call, err := s.revokeInvoker.Prepare(ctx, certificate)
	if err != nil {
		var localErr *revokeLocalError
		if !errors.As(err, &localErr) {
			err = &revokeLocalError{err: err}
		}
		return s.finishAttempt(ctx, certificate, attempt, classifyLocalError(err))
	}

	// mark that we are about to contact the CA; if this cannot be persisted we
	// must not call the CA (a restart could no longer prove the call was missed)
	attempt.Stage = domain.CertificateRevocationStageTypeCalling.String()
	attempt.Heartbeat = time.Now()
	if _, err := s.revocationRepo.Save(ctx, attempt); err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, revokeCallTimeout)
	defer cancel()

	callErr := call(callCtx)
	return s.finishAttempt(ctx, certificate, attempt, classifyRevokeError(callErr))
}

func (s *CertificateService) finishAttempt(ctx context.Context, certificate *domain.Certificate, attempt *domain.CertificateRevocation, result revokeErrorResult) (*dtos.CertificateRevokeResp, error) {
	now := time.Now()
	attempt.EndedAt = now
	attempt.Heartbeat = now
	attempt.ErrorType = result.errorType
	attempt.ErrorStatusCode = result.statusCode
	attempt.ErrorMessage = result.message

	switch result.outcome {
	case revokeOutcomeSucceeded:
		attempt.Status = domain.CertificateRevocationStatusTypeSucceeded.String()
		attempt.Converged = false
		if _, err := s.revocationRepo.Save(ctx, attempt); err != nil {
			// the CA was already revoked successfully; surface the error so a
			// follow-up request/startup recovery converges locally without a
			// second CA call
			return nil, fmt.Errorf("failed to persist the succeeded revocation attempt: %w", err)
		}

		if err := s.convergeRevokedCertificate(ctx, certificate, attempt); err != nil {
			return nil, fmt.Errorf("the CA revoked the certificate but the local state could not be saved: %w", err)
		}

		return buildRevokeResp(attempt), nil

	case revokeOutcomeFailed:
		attempt.Status = domain.CertificateRevocationStatusTypeFailed.String()
		if _, err := s.revocationRepo.Save(ctx, attempt); err != nil {
			return nil, err
		}
		slog.Default().Warn("certificate revocation was rejected by the CA",
			slog.String("certificateId", certificate.Id), slog.String("errorType", result.errorType), slog.Int("statusCode", result.statusCode))
		return nil, fmt.Errorf("failed to revoke certificate: %s", result.message)

	default: // unknown
		attempt.Status = domain.CertificateRevocationStatusTypeUnknown.String()
		if _, err := s.revocationRepo.Save(ctx, attempt); err != nil {
			return nil, err
		}
		slog.Default().Warn("certificate revocation result is unknown",
			slog.String("certificateId", certificate.Id), slog.String("errorType", result.errorType), slog.String("message", result.message))
		return nil, buildUnknownOutcomeError(attempt)
	}
}

// convergeRevokedCertificate flips certificate.isRevoked to true based solely
// on a succeeded revocation attempt. It never calls the CA. A certificate that
// has been deleted meanwhile is treated as already converged.
func (s *CertificateService) convergeRevokedCertificate(ctx context.Context, certificate *domain.Certificate, attempt *domain.CertificateRevocation) error {
	if attempt.Converged {
		return nil
	}

	latest := certificate
	if latest == nil || latest.Id != attempt.CertificateRef {
		c, err := s.certificateRepo.GetById(ctx, attempt.CertificateRef)
		if err != nil {
			if domain.IsRecordNotFoundError(err) {
				return s.markAttemptConverged(ctx, attempt)
			}
			return err
		}
		latest = c
	}

	if !latest.IsRevoked {
		latest.IsRevoked = true
		if _, err := s.certificateRepo.Save(ctx, latest); err != nil {
			return err
		}
	}

	return s.markAttemptConverged(ctx, attempt)
}

func (s *CertificateService) markAttemptConverged(ctx context.Context, attempt *domain.CertificateRevocation) error {
	if attempt.Converged {
		return nil
	}
	attempt.Converged = true
	_, err := s.revocationRepo.Save(ctx, attempt)
	return err
}

// RecoverInterrupted resolves revocation attempts left behind by a previous
// process. It runs once on startup:
//   - processing/calling -> unknown (the remote result cannot be proven);
//   - processing/prepared -> taken over through the same singleflight entry
//     point (the CA was never contacted);
//   - succeeded but unconverged -> local convergence only.
func (s *CertificateService) RecoverInterrupted(ctx context.Context) error {
	processing, err := s.revocationRepo.ListProcessing(ctx)
	if err != nil {
		return fmt.Errorf("failed to list processing revocation attempts: %w", err)
	}

	certIdsToResume := make(map[string]struct{})
	for _, attempt := range processing {
		switch attempt.Stage {
		case domain.CertificateRevocationStageTypePrepared.String():
			certIdsToResume[attempt.CertificateRef] = struct{}{}

		default:
			// stage=calling or any unexpected stage: never resend blindly
			markAttemptUnknown(attempt, revokeErrorTypeInterrupted, 0,
				"the owner process exited while the revocation request was in flight; the remote result is unknown")
			if _, err := s.revocationRepo.Save(ctx, attempt); err != nil {
				slog.Default().Error("failed to mark an interrupted revocation attempt as unknown",
					slog.String("attemptId", attempt.Id), slog.Any("error", err))
			} else {
				slog.Default().Warn("marked an interrupted revocation attempt as unknown",
					slog.String("certificateId", attempt.CertificateRef), slog.String("attemptId", attempt.Id))
			}
		}
	}

	unconverged, err := s.revocationRepo.ListUnconverged(ctx)
	if err != nil {
		slog.Default().Error("failed to list unconverged revocation attempts", slog.Any("error", err))
	} else {
		for _, attempt := range unconverged {
			certIdsToResume[attempt.CertificateRef] = struct{}{}
		}
	}

	for certId := range certIdsToResume {
		certId := certId
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Default().Error("panic while recovering a revocation attempt",
						slog.String("certificateId", certId), slog.Any("recover", r))
				}
			}()

			_, _, _ = s.revokeFlights.Do(certId, func() (any, error) {
				defer func() {
					if r := recover(); r != nil {
						slog.Default().Error("panic while recovering a revocation attempt",
							slog.String("certificateId", certId), slog.Any("recover", r))
					}
				}()
				return s.runRevocation(context.WithoutCancel(ctx), certId, false)
			})
		}()
	}

	if len(processing) > 0 || len(unconverged) > 0 {
		slog.Default().Info("revocation attempt recovery scheduled",
			slog.Int("processing", len(processing)), slog.Int("unconverged", len(unconverged)),
			slog.Int("resumed", len(certIdsToResume)))
	}

	return nil
}

// closeProcessingAttemptForMissingCertificate puts a processing attempt whose
// certificate record has been deleted into a terminal state, without ever
// contacting the CA. A prepared attempt is closed as a definitive failure; a
// calling attempt (which might have reached the CA) is closed as unknown.
func (s *CertificateService) closeProcessingAttemptForMissingCertificate(ctx context.Context, attempt *domain.CertificateRevocation) {
	now := time.Now()
	message := "the certificate record no longer exists; the revocation attempt is closed without contacting the CA"
	if attempt.Stage == domain.CertificateRevocationStageTypeCalling.String() {
		attempt.Status = domain.CertificateRevocationStatusTypeUnknown.String()
		attempt.ErrorType = revokeErrorTypeInterrupted
	} else {
		attempt.Status = domain.CertificateRevocationStatusTypeFailed.String()
		attempt.ErrorType = revokeErrorTypeLocal
	}
	attempt.ErrorStatusCode = 0
	attempt.ErrorMessage = message
	attempt.Heartbeat = now
	if attempt.StartedAt.IsZero() {
		attempt.StartedAt = attempt.CreatedAt
	}
	attempt.EndedAt = now
	if _, err := s.revocationRepo.Save(ctx, attempt); err != nil {
		slog.Default().Error("failed to close a revocation attempt whose certificate is missing",
			slog.String("attemptId", attempt.Id), slog.Any("error", err))
	}
}

func markAttemptUnknown(attempt *domain.CertificateRevocation, errorType string, statusCode int, message string) {
	now := time.Now()
	attempt.Status = domain.CertificateRevocationStatusTypeUnknown.String()
	attempt.ErrorType = errorType
	attempt.ErrorStatusCode = statusCode
	attempt.ErrorMessage = message
	attempt.Heartbeat = now
	if attempt.StartedAt.IsZero() {
		attempt.StartedAt = attempt.CreatedAt
	}
	attempt.EndedAt = now
}

func buildRevokeResp(attempt *domain.CertificateRevocation) *dtos.CertificateRevokeResp {
	return &dtos.CertificateRevokeResp{
		AttemptId: attempt.Id,
		Status:    attempt.Status,
	}
}

func buildUnknownOutcomeError(attempt *domain.CertificateRevocation) error {
	message := "the certificate revocation result is unknown due to a timeout or an interrupted request; the CA will not be contacted again until you explicitly confirm a new attempt"
	if attempt.ErrorMessage != "" {
		message = fmt.Sprintf("%s: %s", message, attempt.ErrorMessage)
	}
	return domain.NewError(409, message)
}
