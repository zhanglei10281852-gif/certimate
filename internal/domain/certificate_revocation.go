package domain

import "time"

const CollectionNameCertificateRevocation = "certificate_revocation"

// CertificateRevocationStatusType is the terminal/in-flight outcome of a
// persisted certificate revocation attempt.
type CertificateRevocationStatusType string

func (t CertificateRevocationStatusType) String() string {
	return string(t)
}

const (
	// CertificateRevocationStatusTypeProcessing the attempt has been persisted
	// and is owned by an instance, but no terminal result is known yet.
	CertificateRevocationStatusTypeProcessing = CertificateRevocationStatusType("processing")
	// CertificateRevocationStatusTypeSucceeded the CA explicitly acknowledged
	// the revocation (or reported the certificate as already revoked).
	CertificateRevocationStatusTypeSucceeded = CertificateRevocationStatusType("succeeded")
	// CertificateRevocationStatusTypeFailed the CA explicitly rejected the
	// revocation request. A new attempt can be started through the regular
	// revocation entry point.
	CertificateRevocationStatusTypeFailed = CertificateRevocationStatusType("failed")
	// CertificateRevocationStatusTypeUnknown the remote result could not be
	// determined (timeout, connection interruption, 5xx, owner lost while the
	// request was in flight). A new attempt requires explicit reconfirmation.
	CertificateRevocationStatusTypeUnknown = CertificateRevocationStatusType("unknown")
)

// CertificateRevocationStageType marks whether the owner has reached the point
// of actually calling the ACME revocation endpoint.
type CertificateRevocationStageType string

func (t CertificateRevocationStageType) String() string {
	return string(t)
}

const (
	// CertificateRevocationStageTypePrepared the attempt is persisted and
	// owned, but the ACME endpoint has never been called. A restarted
	// instance can safely take it over.
	CertificateRevocationStageTypePrepared = CertificateRevocationStageType("prepared")
	// CertificateRevocationStageTypeCalling the owner is/was about to call or
	// is calling the ACME revocation endpoint. If the owner disappears in
	// this stage, the remote result is unknown.
	CertificateRevocationStageTypeCalling = CertificateRevocationStageType("calling")
)

type CertificateRevocation struct {
	Meta
	CertificateRef  string    `db:"certificateRef"  json:"certificateRef"`
	Status          string    `db:"status"          json:"status"`
	Stage           string    `db:"stage"           json:"stage"`
	Owner           string    `db:"owner"           json:"owner"`
	Heartbeat       time.Time `db:"heartbeat"       json:"heartbeat"`
	Converged       bool      `db:"converged"       json:"converged"`
	ErrorType       string    `db:"errorType"       json:"errorType"`
	ErrorStatusCode int       `db:"errorStatusCode" json:"errorStatusCode"`
	ErrorMessage    string    `db:"errorMessage"    json:"errorMessage"`
	StartedAt       time.Time `db:"startedAt"       json:"startedAt"`
	EndedAt         time.Time `db:"endedAt"         json:"endedAt"`
}
