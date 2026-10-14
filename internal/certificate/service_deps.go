package certificate

import (
	"context"

	"github.com/pocketbase/dbx"

	"github.com/certimate-go/certimate/internal/domain"
)

type acmeAccountRepository interface {
	GetByCAAndAcctUrl(ctx context.Context, ca string, acctUrl string) (*domain.ACMEAccount, error)
}

type certificateRepository interface {
	GetById(ctx context.Context, id string) (*domain.Certificate, error)
	Save(ctx context.Context, certificate *domain.Certificate) (*domain.Certificate, error)
	DeleteWithExprs(ctx context.Context, exprs ...dbx.Expression) (int, error)
}

type certificateRevocationRepository interface {
	GetLatestByCertificateId(ctx context.Context, certificateId string) (*domain.CertificateRevocation, error)
	ListProcessing(ctx context.Context) ([]*domain.CertificateRevocation, error)
	ListUnconverged(ctx context.Context) ([]*domain.CertificateRevocation, error)
	CreateProcessingIfAbsent(ctx context.Context, attempt *domain.CertificateRevocation) (*domain.CertificateRevocation, bool, error)
	Save(ctx context.Context, attempt *domain.CertificateRevocation) (*domain.CertificateRevocation, error)
}
