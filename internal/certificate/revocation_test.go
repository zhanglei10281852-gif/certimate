package certificate

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/dbx"

	"github.com/go-acme/lego/v5/acme"

	"github.com/certimate-go/certimate/internal/domain"
	"github.com/certimate-go/certimate/internal/domain/dtos"
)

// ---------------- fakes ----------------

type fakeCertRepo struct {
	mu           sync.Mutex
	certs        map[string]*domain.Certificate
	saveFailures map[string]int // remaining forced failures by id
	saveCount    map[string]int
}

func newFakeCertRepo() *fakeCertRepo {
	return &fakeCertRepo{
		certs:        map[string]*domain.Certificate{},
		saveFailures: map[string]int{},
		saveCount:    map[string]int{},
	}
}

func cloneCert(c *domain.Certificate) *domain.Certificate {
	v := *c
	return &v
}

func (r *fakeCertRepo) seed(c *domain.Certificate) *domain.Certificate {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.Id == "" {
		c.Id = "cert-1"
	}
	r.certs[c.Id] = cloneCert(c)
	return r.certs[c.Id]
}

func (r *fakeCertRepo) GetById(ctx context.Context, id string) (*domain.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.certs[id]
	if !ok {
		return nil, domain.ErrRecordNotFound
	}
	return cloneCert(c), nil
}

func (r *fakeCertRepo) Save(ctx context.Context, c *domain.Certificate) (*domain.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if c.Id == "" {
		c.Id = fmt.Sprintf("cert-%d", len(r.certs)+1)
	}
	if r.saveFailures[c.Id] > 0 {
		r.saveFailures[c.Id]--
		r.saveCount[c.Id]++
		return c, fmt.Errorf("simulated certificate save failure")
	}
	r.saveCount[c.Id]++
	stored := cloneCert(c)
	r.certs[c.Id] = stored
	return cloneCert(stored), nil
}

func (r *fakeCertRepo) DeleteWithExprs(ctx context.Context, exprs ...dbx.Expression) (int, error) {
	return 0, nil
}

func (r *fakeCertRepo) failNextSave(id string, times int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saveFailures[id] = times
}

func (r *fakeCertRepo) isRevoked(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.certs[id]
	return ok && c.IsRevoked
}

func (r *fakeCertRepo) delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.certs, id)
}

type fakeRevocationRepo struct {
	mu           sync.Mutex
	store        map[string]*domain.CertificateRevocation
	seq          int
	createErr    error // forced CreateProcessingIfAbsent failure
	saveFailures int   // number of consecutive forced Save failures
}

func newFakeRevocationRepo() *fakeRevocationRepo {
	return &fakeRevocationRepo{store: map[string]*domain.CertificateRevocation{}}
}

func cloneAttempt(a *domain.CertificateRevocation) *domain.CertificateRevocation {
	v := *a
	return &v
}

func (r *fakeRevocationRepo) GetLatestByCertificateId(ctx context.Context, certificateId string) (*domain.CertificateRevocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest *domain.CertificateRevocation
	for _, a := range r.store {
		if a.CertificateRef != certificateId {
			continue
		}
		if latest == nil || a.CreatedAt.After(latest.CreatedAt) {
			latest = a
		}
	}
	if latest == nil {
		return nil, domain.ErrRecordNotFound
	}
	return cloneAttempt(latest), nil
}

func (r *fakeRevocationRepo) ListProcessing(ctx context.Context) ([]*domain.CertificateRevocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ret []*domain.CertificateRevocation
	for _, a := range r.store {
		if a.Status == domain.CertificateRevocationStatusTypeProcessing.String() {
			ret = append(ret, cloneAttempt(a))
		}
	}
	return ret, nil
}

func (r *fakeRevocationRepo) ListUnconverged(ctx context.Context) ([]*domain.CertificateRevocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ret []*domain.CertificateRevocation
	for _, a := range r.store {
		if a.Status == domain.CertificateRevocationStatusTypeSucceeded.String() && !a.Converged {
			ret = append(ret, cloneAttempt(a))
		}
	}
	return ret, nil
}

func (r *fakeRevocationRepo) CreateProcessingIfAbsent(ctx context.Context, attempt *domain.CertificateRevocation) (*domain.CertificateRevocation, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return nil, false, r.createErr
	}
	for _, a := range r.store {
		if a.CertificateRef == attempt.CertificateRef && a.Status == domain.CertificateRevocationStatusTypeProcessing.String() {
			return cloneAttempt(a), false, nil
		}
	}
	r.seq++
	if attempt.Id == "" {
		attempt.Id = fmt.Sprintf("att-%d", r.seq)
	}
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = time.Unix(1700000000, 0).Add(time.Duration(r.seq) * time.Second)
		attempt.UpdatedAt = attempt.CreatedAt
	}
	r.store[attempt.Id] = cloneAttempt(attempt)
	return cloneAttempt(attempt), true, nil
}

func (r *fakeRevocationRepo) Save(ctx context.Context, attempt *domain.CertificateRevocation) (*domain.CertificateRevocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveFailures > 0 {
		r.saveFailures--
		return attempt, fmt.Errorf("simulated attempt save failure")
	}
	if attempt.Id == "" {
		r.seq++
		attempt.Id = fmt.Sprintf("att-%d", r.seq)
		if attempt.CreatedAt.IsZero() {
			attempt.CreatedAt = time.Unix(1700000000, 0).Add(time.Duration(r.seq) * time.Second)
		}
	}
	attempt.UpdatedAt = time.Now()
	r.store[attempt.Id] = cloneAttempt(attempt)
	return cloneAttempt(attempt), nil
}

func (r *fakeRevocationRepo) countByCert(certId string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, a := range r.store {
		if a.CertificateRef == certId {
			n++
		}
	}
	return n
}

func (r *fakeRevocationRepo) seed(attempt *domain.CertificateRevocation) *domain.CertificateRevocation {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	attempt.Id = fmt.Sprintf("att-seed-%d", r.seq)
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = time.Unix(1700000000, 0).Add(time.Duration(r.seq) * time.Second)
		attempt.UpdatedAt = attempt.CreatedAt
	}
	r.store[attempt.Id] = cloneAttempt(attempt)
	return cloneAttempt(attempt)
}

type fakeRevokeInvoker struct {
	calls atomic.Int32
	// prepareErr simulates a local pre-call error (account/client).
	prepareErr error
	// doErr is returned by the CA call.
	doErr error
	// onDo is invoked inside the actual CA call; tests use it to assert the
	// persisted attempt state at call time.
	onDo func(certId string)
	// gate blocks the CA call until release is closed (concurrency tests).
	entered chan struct{}
	release chan struct{}
}

func (i *fakeRevokeInvoker) Prepare(ctx context.Context, certificate *domain.Certificate) (revokeCall, error) {
	if i.prepareErr != nil {
		return nil, &revokeLocalError{err: i.prepareErr}
	}
	return func(callCtx context.Context) error {
		if i.entered != nil {
			select {
			case i.entered <- struct{}{}:
			default:
			}
		}
		if i.release != nil {
			select {
			case <-i.release:
			case <-callCtx.Done():
				return callCtx.Err()
			}
		}
		i.calls.Add(1)
		if i.onDo != nil {
			i.onDo(certificate.Id)
		}
		return i.doErr
	}, nil
}

// ---------------- helpers ----------------

func newTestService(certRepo *fakeCertRepo, attRepo *fakeRevocationRepo, invoker *fakeRevokeInvoker) *CertificateService {
	setOwnerIDForTest("owner-test")
	return &CertificateService{
		certificateRepo: certRepo,
		revocationRepo:  attRepo,
		revokeInvoker:   invoker,
	}
}

func issuedCertificate() *domain.Certificate {
	return &domain.Certificate{
		Meta:               domain.Meta{Id: "cert-1"},
		Source:             domain.CertificateSourceTypeRequest,
		Certificate:        "PEM",
		CA:                 "letsencrypt",
		ACMEAccountUrl:     "https://example.com/acct/1",
		ACMECertificateUrl: "https://example.com/cert/1",
	}
}

func revokeReq(confirm bool) *dtos.CertificateRevokeReq {
	return &dtos.CertificateRevokeReq{CertificateId: "cert-1", Confirm: confirm}
}

func waitUntil(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func assertDomainCode(t *testing.T, err error, code int) {
	t.Helper()
	var dErr *domain.Error
	if err == nil {
		t.Fatalf("expected error with code %d, got nil", code)
	}
	if !asDomainError(err, &dErr) || dErr.Code != code {
		t.Fatalf("expected error code %d, got %v", code, err)
	}
}

func asDomainError(err error, target **domain.Error) bool {
	for err != nil {
		if e, ok := err.(*domain.Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ---------------- tests ----------------

func TestRevokePersistsAttemptBeforeCACallAndConverges(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{}
	invoker.onDo = func(certId string) {
		latest, err := attRepo.GetLatestByCertificateId(context.Background(), certId)
		if err != nil {
			t.Errorf("attempt must be persisted before CA call: %v", err)
			return
		}
		if latest.Status != domain.CertificateRevocationStatusTypeProcessing.String() ||
			latest.Stage != domain.CertificateRevocationStageTypeCalling.String() ||
			latest.Owner == "" {
			t.Errorf("attempt state at CA call = %s/%s owner=%q, want processing/calling with owner",
				latest.Status, latest.Stage, latest.Owner)
		}
	}
	s := newTestService(certRepo, attRepo, invoker)

	resp, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err != nil {
		t.Fatalf("RevokeCertificate: %v", err)
	}
	if resp.Status != domain.CertificateRevocationStatusTypeSucceeded.String() || resp.AttemptId == "" {
		t.Fatalf("unexpected resp: %+v", resp)
	}
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d, want 1", invoker.calls.Load())
	}
	if !certRepo.isRevoked("cert-1") {
		t.Fatal("certificate should be converged to revoked")
	}
	latest, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if latest.Status != "succeeded" || !latest.Converged {
		t.Fatalf("attempt = %s converged=%v, want succeeded/converged", latest.Status, latest.Converged)
	}
}

func TestRevokeConcurrentRequestsShareSingleCACall(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	s := newTestService(certRepo, attRepo, invoker)

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.RevokeCertificate(context.Background(), revokeReq(false))
		}(i)
	}

	select {
	case <-invoker.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("CA call never started")
	}
	close(invoker.release)
	wg.Wait()

	for i, err := range errs {
		if err != nil && err.Error() != "could not revoke a certificate which is already revoked" {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d, want 1", invoker.calls.Load())
	}
	if attRepo.countByCert("cert-1") != 1 {
		t.Fatalf("attempt records = %d, want 1", attRepo.countByCert("cert-1"))
	}
}

func TestRevokeExplicitRejectionFailsAndAllowsNewAttempt(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{doErr: &acme.ProblemDetails{Type: acme.UnauthorizedErrorType, HTTPStatus: 403}}
	s := newTestService(certRepo, attRepo, invoker)

	_, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err == nil {
		t.Fatal("expected rejection error")
	}
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d, want 1", invoker.calls.Load())
	}
	first, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if first.Status != domain.CertificateRevocationStatusTypeFailed.String() || first.ErrorStatusCode != 403 {
		t.Fatalf("attempt = %s/%d, want failed/403", first.Status, first.ErrorStatusCode)
	}
	if certRepo.isRevoked("cert-1") {
		t.Fatal("certificate must stay not revoked after explicit rejection")
	}

	// regular entry point starts a new attempt (without confirm)
	invoker.doErr = nil
	_, err = s.RevokeCertificate(context.Background(), revokeReq(false))
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if invoker.calls.Load() != 2 {
		t.Fatalf("CA calls = %d, want 2", invoker.calls.Load())
	}
	if attRepo.countByCert("cert-1") != 2 {
		t.Fatalf("attempt records = %d, want 2", attRepo.countByCert("cert-1"))
	}
	if !certRepo.isRevoked("cert-1") {
		t.Fatal("certificate should be revoked after successful retry")
	}
}

func TestRevokeUnknownBlocksBlindResendUntilConfirmed(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{doErr: context.DeadlineExceeded}
	s := newTestService(certRepo, attRepo, invoker)

	_, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	assertDomainCode(t, err, 409)
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d, want 1", invoker.calls.Load())
	}
	first, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if first.Status != domain.CertificateRevocationStatusTypeUnknown.String() || first.ErrorType != revokeErrorTypeTimeout {
		t.Fatalf("attempt = %s/%s, want unknown/timeout", first.Status, first.ErrorType)
	}

	// blind resend without explicit confirmation must not contact the CA
	_, err = s.RevokeCertificate(context.Background(), revokeReq(false))
	assertDomainCode(t, err, 409)
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d after unconfirmed retry, want still 1", invoker.calls.Load())
	}
	if attRepo.countByCert("cert-1") != 1 {
		t.Fatalf("no new attempt may be created without confirmation, got %d", attRepo.countByCert("cert-1"))
	}

	// explicit confirmation starts a new attempt
	invoker.doErr = nil
	_, err = s.RevokeCertificate(context.Background(), revokeReq(true))
	if err != nil {
		t.Fatalf("confirmed retry: %v", err)
	}
	if invoker.calls.Load() != 2 {
		t.Fatalf("CA calls = %d after confirm, want 2", invoker.calls.Load())
	}
	if attRepo.countByCert("cert-1") != 2 {
		t.Fatalf("attempt records after confirm = %d, want 2", attRepo.countByCert("cert-1"))
	}
}

func TestRevokeSucceededButLocalSaveFailsConvergesWithoutSecondCall(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	certRepo.failNextSave("cert-1", 1)
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{}
	s := newTestService(certRepo, attRepo, invoker)

	_, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err == nil {
		t.Fatal("expected local-save error on first request")
	}
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d, want 1", invoker.calls.Load())
	}
	first, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if first.Status != domain.CertificateRevocationStatusTypeSucceeded.String() || first.Converged {
		t.Fatalf("attempt = %s converged=%v, want succeeded/not converged", first.Status, first.Converged)
	}

	// follow-up request converges locally only
	resp, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err != nil {
		t.Fatalf("converge request: %v", err)
	}
	if resp.Status != domain.CertificateRevocationStatusTypeSucceeded.String() {
		t.Fatalf("status = %q", resp.Status)
	}
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d after convergence, want still 1", invoker.calls.Load())
	}
	if !certRepo.isRevoked("cert-1") {
		t.Fatal("certificate should be revoked after local convergence")
	}
	latest, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if !latest.Converged {
		t.Fatal("attempt should be marked converged")
	}
}

func TestPreparedLeftoverIsSafelyTakenOver(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{}

	// leftover from a dead owner, never reached the CA
	attRepo.seed(&domain.CertificateRevocation{
		CertificateRef: "cert-1",
		Status:         domain.CertificateRevocationStatusTypeProcessing.String(),
		Stage:          domain.CertificateRevocationStageTypePrepared.String(),
		Owner:          "owner-dead",
		Heartbeat:      time.Now().Add(-time.Hour),
		StartedAt:      time.Now().Add(-time.Hour),
	})
	s := newTestService(certRepo, attRepo, invoker)

	resp, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if resp.Status != domain.CertificateRevocationStatusTypeSucceeded.String() {
		t.Fatalf("status = %q", resp.Status)
	}
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d, want exactly 1 after takeover", invoker.calls.Load())
	}
	if attRepo.countByCert("cert-1") != 1 {
		t.Fatalf("takeover must reuse the same attempt record, got %d", attRepo.countByCert("cert-1"))
	}
	latest, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if latest.Owner != "owner-test" || latest.Status != "succeeded" {
		t.Fatalf("attempt owner=%s status=%s", latest.Owner, latest.Status)
	}
}

func TestCallingLeftoverBecomesUnknownWithoutCallingCA(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{}
	attRepo.seed(&domain.CertificateRevocation{
		CertificateRef: "cert-1",
		Status:         domain.CertificateRevocationStatusTypeProcessing.String(),
		Stage:          domain.CertificateRevocationStageTypeCalling.String(),
		Owner:          "owner-dead",
		Heartbeat:      time.Now().Add(-time.Hour),
		StartedAt:      time.Now().Add(-time.Hour),
	})
	s := newTestService(certRepo, attRepo, invoker)

	_, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	assertDomainCode(t, err, 409)
	if invoker.calls.Load() != 0 {
		t.Fatalf("CA calls = %d, want 0", invoker.calls.Load())
	}
	latest, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if latest.Status != domain.CertificateRevocationStatusTypeUnknown.String() {
		t.Fatalf("status = %s, want unknown", latest.Status)
	}
	if latest.ErrorMessage == "" {
		t.Fatal("unknown attempt must carry a diagnostic message")
	}
}

func TestAlreadyRevokedResponseConvergesLocally(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{doErr: &acme.ProblemDetails{Type: acme.AlreadyRevokedErrorType, HTTPStatus: 400}}
	s := newTestService(certRepo, attRepo, invoker)

	resp, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err != nil {
		t.Fatalf("alreadyRevoked should converge: %v", err)
	}
	if resp.Status != domain.CertificateRevocationStatusTypeSucceeded.String() {
		t.Fatalf("status = %q", resp.Status)
	}
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d, want 1", invoker.calls.Load())
	}
	if !certRepo.isRevoked("cert-1") {
		t.Fatal("certificate should be converged to revoked")
	}

	// second request must not call the CA and keeps the existing rejection
	// semantics once locally revoked
	_, err = s.RevokeCertificate(context.Background(), revokeReq(false))
	if err == nil || err.Error() != "could not revoke a certificate which is already revoked" {
		t.Fatalf("legacy already-revoked error expected, got %v", err)
	}
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d, want still 1", invoker.calls.Load())
	}
}

func TestSucceededAttemptWithDeletedCertificateIsConverged(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	certRepo.delete("cert-1")
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{}
	seeded := attRepo.seed(&domain.CertificateRevocation{
		CertificateRef: "cert-1",
		Status:         domain.CertificateRevocationStatusTypeSucceeded.String(),
		Stage:          domain.CertificateRevocationStageTypeCalling.String(),
		Converged:      false,
		EndedAt:        time.Now(),
	})
	s := newTestService(certRepo, attRepo, invoker)

	_, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err != nil {
		t.Fatalf("deleted certificate should converge without error: %v", err)
	}
	if invoker.calls.Load() != 0 {
		t.Fatalf("CA calls = %d, want 0", invoker.calls.Load())
	}
	latest, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if !latest.Converged || latest.Id != seeded.Id {
		t.Fatal("existing succeeded attempt should be marked converged")
	}
}

func TestRevokeCompatibilityGuards(t *testing.T) {
	// not issued in Certimate (no ACME urls)
	certRepo := newFakeCertRepo()
	c := issuedCertificate()
	c.ACMEAccountUrl = ""
	c.ACMECertificateUrl = ""
	certRepo.seed(c)
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{}
	s := newTestService(certRepo, attRepo, invoker)

	_, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err == nil || err.Error() != "could not revoke a certificate which is not issued in Certimate" {
		t.Fatalf("not-issued error expected, got %v", err)
	}
	if invoker.calls.Load() != 0 || attRepo.countByCert("cert-1") != 0 {
		t.Fatal("no attempt/CA call expected before validation")
	}

	// legacy revoked certificate (isRevoked without attempt)
	certRepo2 := newFakeCertRepo()
	c2 := issuedCertificate()
	c2.IsRevoked = true
	certRepo2.seed(c2)
	attRepo2 := newFakeRevocationRepo()
	invoker2 := &fakeRevokeInvoker{}
	s2 := newTestService(certRepo2, attRepo2, invoker2)
	_, err = s2.RevokeCertificate(context.Background(), revokeReq(false))
	if err == nil || err.Error() != "could not revoke a certificate which is already revoked" {
		t.Fatalf("already-revoked error expected, got %v", err)
	}
	if invoker2.calls.Load() != 0 {
		t.Fatal("no CA call expected for already revoked certificate")
	}
}

func TestLocalPreparationErrorRecordsDefinitiveFailure(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{prepareErr: fmt.Errorf("account missing")}
	s := newTestService(certRepo, attRepo, invoker)

	_, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err == nil {
		t.Fatal("expected preparation error")
	}
	latest, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if latest.Status != domain.CertificateRevocationStatusTypeFailed.String() || latest.ErrorType != revokeErrorTypeLocal {
		t.Fatalf("attempt = %s/%s, want failed/local", latest.Status, latest.ErrorType)
	}
	if certRepo.isRevoked("cert-1") {
		t.Fatal("certificate must stay not revoked")
	}

	// retry through the regular entry point
	invoker.prepareErr = nil
	_, err = s.RevokeCertificate(context.Background(), revokeReq(false))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if invoker.calls.Load() != 1 {
		t.Fatalf("CA calls = %d, want 1", invoker.calls.Load())
	}
}

func TestRecoverInterruptedResolvesAllLeftovers(t *testing.T) {
	certRepo := newFakeCertRepo()
	c1 := issuedCertificate()
	certRepo.seed(c1)
	c2 := issuedCertificate()
	c2.Id = "cert-2"
	certRepo.seed(c2)
	c3 := issuedCertificate()
	c3.Id = "cert-3"
	certRepo.seed(c3)

	attRepo := newFakeRevocationRepo()
	invoker := &fakeRevokeInvoker{}

	// cert-1: prepared leftover -> take over and call CA once
	attRepo.seed(&domain.CertificateRevocation{
		CertificateRef: "cert-1",
		Status:         domain.CertificateRevocationStatusTypeProcessing.String(),
		Stage:          domain.CertificateRevocationStageTypePrepared.String(),
		Owner:          "owner-dead",
		Heartbeat:      time.Now().Add(-time.Hour),
		StartedAt:      time.Now().Add(-time.Hour),
	})
	// cert-2: calling leftover -> unknown, no CA call
	attRepo.seed(&domain.CertificateRevocation{
		CertificateRef: "cert-2",
		Status:         domain.CertificateRevocationStatusTypeProcessing.String(),
		Stage:          domain.CertificateRevocationStageTypeCalling.String(),
		Owner:          "owner-dead",
		Heartbeat:      time.Now().Add(-time.Hour),
		StartedAt:      time.Now().Add(-time.Hour),
	})
	// cert-3: succeeded but unconverged -> local convergence only
	attRepo.seed(&domain.CertificateRevocation{
		CertificateRef: "cert-3",
		Status:         domain.CertificateRevocationStatusTypeSucceeded.String(),
		Stage:          domain.CertificateRevocationStageTypeCalling.String(),
		Converged:      false,
		StartedAt:      time.Now().Add(-time.Hour),
		EndedAt:        time.Now().Add(-time.Hour),
	})
	s := newTestService(certRepo, attRepo, invoker)

	if err := s.RecoverInterrupted(context.Background()); err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}

	waitUntil(t, 2*time.Second, func() bool {
		return invoker.calls.Load() == 1 &&
			certRepo.isRevoked("cert-1") &&
			certRepo.isRevoked("cert-3")
	})

	a1, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if a1.Status != domain.CertificateRevocationStatusTypeSucceeded.String() || !a1.Converged {
		t.Fatalf("cert-1 attempt = %s/%v, want succeeded/converged", a1.Status, a1.Converged)
	}
	a2, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-2")
	if a2.Status != domain.CertificateRevocationStatusTypeUnknown.String() {
		t.Fatalf("cert-2 attempt = %s, want unknown", a2.Status)
	}
	a3, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-3")
	if !a3.Converged {
		t.Fatal("cert-3 attempt should be converged")
	}
	if invoker.calls.Load() != 1 {
		t.Fatalf("total CA calls = %d, want 1 (only the prepared leftover)", invoker.calls.Load())
	}
}

func TestRevokeAbortsWhenAttemptCannotBePersisted(t *testing.T) {
	// attempt row creation fails: the CA must never be contacted
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	attRepo := newFakeRevocationRepo()
	attRepo.createErr = errors.New("database is unavailable")
	invoker := &fakeRevokeInvoker{}
	s := newTestService(certRepo, attRepo, invoker)

	_, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err == nil {
		t.Fatal("expected persistence error")
	}
	if invoker.calls.Load() != 0 {
		t.Fatalf("CA calls = %d, want 0 when attempt persistence fails", invoker.calls.Load())
	}
	if attRepo.countByCert("cert-1") != 0 {
		t.Fatal("no attempt record may exist after a failed create")
	}
}

func TestRevokeAbortsWhenCallingStagePersistFails(t *testing.T) {
	certRepo := newFakeCertRepo()
	certRepo.seed(issuedCertificate())
	attRepo := newFakeRevocationRepo()
	attRepo.saveFailures = 1 // the first Save after create is the calling-stage write
	invoker := &fakeRevokeInvoker{}
	s := newTestService(certRepo, attRepo, invoker)

	_, err := s.RevokeCertificate(context.Background(), revokeReq(false))
	if err == nil {
		t.Fatal("expected calling-stage persistence error")
	}
	if invoker.calls.Load() != 0 {
		t.Fatalf("CA calls = %d, want 0 when calling stage cannot be persisted", invoker.calls.Load())
	}
	latest, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
	if latest.Status != domain.CertificateRevocationStatusTypeProcessing.String() ||
		latest.Stage != domain.CertificateRevocationStageTypePrepared.String() {
		t.Fatalf("attempt = %s/%s, want processing/prepared leftover", latest.Status, latest.Stage)
	}
}

func TestProcessingAttemptForMissingCertificateIsClosed(t *testing.T) {
	cases := []struct {
		name       string
		stage      string
		wantStatus string
		wantType   string
	}{
		{
			name:       "prepared leftover",
			stage:      domain.CertificateRevocationStageTypePrepared.String(),
			wantStatus: domain.CertificateRevocationStatusTypeFailed.String(),
			wantType:   revokeErrorTypeLocal,
		},
		{
			name:       "calling leftover",
			stage:      domain.CertificateRevocationStageTypeCalling.String(),
			wantStatus: domain.CertificateRevocationStatusTypeUnknown.String(),
			wantType:   revokeErrorTypeInterrupted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			certRepo := newFakeCertRepo()
			certRepo.seed(issuedCertificate())
			certRepo.delete("cert-1")
			attRepo := newFakeRevocationRepo()
			invoker := &fakeRevokeInvoker{}
			attRepo.seed(&domain.CertificateRevocation{
				CertificateRef: "cert-1",
				Status:         domain.CertificateRevocationStatusTypeProcessing.String(),
				Stage:          tc.stage,
				Owner:          "owner-dead",
				Heartbeat:      time.Now().Add(-time.Hour),
				StartedAt:      time.Now().Add(-time.Hour),
			})
			s := newTestService(certRepo, attRepo, invoker)

			_, err := s.RevokeCertificate(context.Background(), revokeReq(false))
			if !domain.IsRecordNotFoundError(err) {
				t.Fatalf("expected not-found error, got %v", err)
			}
			if invoker.calls.Load() != 0 {
				t.Fatalf("CA calls = %d, want 0", invoker.calls.Load())
			}
			latest, _ := attRepo.GetLatestByCertificateId(context.Background(), "cert-1")
			if latest.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s", latest.Status, tc.wantStatus)
			}
			if latest.ErrorType != tc.wantType || latest.ErrorMessage == "" || latest.EndedAt.IsZero() {
				t.Fatalf("terminal attempt is missing diagnostics: %+v", latest)
			}

			// recovery must no longer enqueue the closed attempt
			processing, err := attRepo.ListProcessing(context.Background())
			if err != nil || len(processing) != 0 {
				t.Fatalf("processing leftovers = %d (err=%v), want 0", len(processing), err)
			}
		})
	}
}
