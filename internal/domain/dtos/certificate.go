package dtos

import (
	"github.com/certimate-go/certimate/internal/domain"
)

type CertificateDownloadReq struct {
	CertificateId string                       `json:"-"`
	FileFormat    domain.CertificateFormatType `json:"fileFormat"`
	PfxPassword   string                       `json:"pfxPassword,omitempty"`
	PfxEncoder    string                       `json:"pfxEncoder,omitempty"`
	JksAlias      string                       `json:"jksAlias,omitempty"`
	JksKeypass    string                       `json:"jksKeypass,omitempty"`
	JksStorepass  string                       `json:"jksStorepass,omitempty"`
}

type CertificateDownloadResp struct {
	ZipBytes []byte `json:"zipBytes"`
}

type CertificateRevokeReq struct {
	CertificateId string `json:"-"`
	// Confirm explicitly reconfirms that a new revocation attempt may be
	// started when the latest attempt ended in an unknown state.
	Confirm bool `json:"confirm,omitempty"`
}

type CertificateRevokeResp struct {
	AttemptId string `json:"attemptId,omitempty"`
	Status    string `json:"status,omitempty"`
}
