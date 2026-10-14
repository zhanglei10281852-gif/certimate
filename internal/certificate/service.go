package certificate

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pocketbase/dbx"
	"golang.org/x/sync/singleflight"

	"github.com/certimate-go/certimate/internal/app"
	"github.com/certimate-go/certimate/internal/domain"
	"github.com/certimate-go/certimate/internal/domain/dtos"
	"github.com/certimate-go/certimate/internal/repository"
	"github.com/certimate-go/certimate/internal/settings"
	xcert "github.com/certimate-go/certimate/pkg/utils/cert"
	xcertpfx "github.com/certimate-go/certimate/pkg/utils/cert/pfx"
)

type CertificateService struct {
	acmeAccountRepo acmeAccountRepository
	certificateRepo certificateRepository
	revocationRepo  certificateRevocationRepository
	revokeInvoker   revokeInvoker

	// revokeFlights de-duplicates concurrent revocations of the same
	// certificate within the current instance.
	revokeFlights singleflight.Group
}

func NewCertificateService(acmeAccountRepo acmeAccountRepository, certificateRepo certificateRepository) *CertificateService {
	return &CertificateService{
		acmeAccountRepo: acmeAccountRepo,
		certificateRepo: certificateRepo,
		revocationRepo:  repository.NewCertificateRevocationRepository(),
		revokeInvoker:   newACMERevokeInvoker(acmeAccountRepo),
	}
}

func (s *CertificateService) InitSchedule(ctx context.Context) error {
	app.GetScheduler().MustAdd("cleanupCertificateExpired", "0 0 * * *", func() {
		s.cleanupExpiredCertificates(context.Background())
	})

	// resolve revocation attempts left behind by a previous process once
	// on startup; errors are logged and must not block serving
	if err := s.RecoverInterrupted(ctx); err != nil {
		app.GetLogger().Error("failed to recover interrupted revocation attempts", slog.Any("error", err))
	}

	return nil
}

func (s *CertificateService) DownloadCertificate(ctx context.Context, req *dtos.CertificateDownloadReq) (*dtos.CertificateDownloadResp, error) {
	certificate, err := s.certificateRepo.GetById(ctx, req.CertificateId)
	if err != nil {
		return nil, err
	}

	canonicalName := strings.Split(certificate.SubjectAltNames, ";")[0]
	canonicalName = strings.ReplaceAll(canonicalName, "*", "_")

	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)
	defer zipWriter.Close()

	var zipBytes []byte
	switch req.FileFormat {
	case "", domain.CertificateFormatTypePEM:
		{
			serverCertPEM, issuerCertPEM, err := xcert.ExtractCertificatesFromPEM(certificate.Certificate)
			if err != nil {
				return nil, fmt.Errorf("failed to extract certs: %w", err)
			}

			keyWriter, err := zipWriter.Create(fmt.Sprintf("%s.key", canonicalName))
			if err != nil {
				return nil, err
			} else {
				_, err = keyWriter.Write([]byte(certificate.PrivateKey))
				if err != nil {
					return nil, err
				}
			}

			certWriter, err := zipWriter.Create(fmt.Sprintf("%s.crt", canonicalName))
			if err != nil {
				return nil, err
			} else {
				_, err = certWriter.Write([]byte(certificate.Certificate))
				if err != nil {
					return nil, err
				}
			}

			serverCertWriter, err := zipWriter.Create(fmt.Sprintf("%s (server).pem", canonicalName))
			if err != nil {
				return nil, err
			} else {
				_, err = serverCertWriter.Write([]byte(serverCertPEM))
				if err != nil {
					return nil, err
				}
			}

			intermediaCertWriter, err := zipWriter.Create(fmt.Sprintf("%s (intermedia).pem", canonicalName))
			if err != nil {
				return nil, err
			} else {
				_, err = intermediaCertWriter.Write([]byte(issuerCertPEM))
				if err != nil {
					return nil, err
				}
			}

			err = zipWriter.Close()
			if err != nil {
				return nil, err
			}

			zipBytes = buf.Bytes()
		}

	case domain.CertificateFormatTypePFX:
		{
			pfxPassword := "certimate"
			if req.PfxPassword != "" {
				pfxPassword = req.PfxPassword
			}

			pfxEncoder, err := xcertpfx.ResolvePfxEncoder(req.PfxEncoder)
			if err != nil {
				return nil, err
			}

			certPFX, err := xcert.TransformCertificateFromPEMToPFX(certificate.Certificate, certificate.PrivateKey, pfxPassword, pfxEncoder)
			if err != nil {
				return nil, err
			}

			certWriter, err := zipWriter.Create(fmt.Sprintf("%s.pfx", canonicalName))
			if err != nil {
				return nil, err
			} else {
				_, err = certWriter.Write(certPFX)
				if err != nil {
					return nil, err
				}
			}

			readmeWriter, err := zipWriter.Create("README.txt")
			if err != nil {
				return nil, err
			} else {
				readme := fmt.Sprintf("[PFX Password]\n%s\n", pfxPassword)
				_, err = readmeWriter.Write([]byte(readme))
				if err != nil {
					return nil, err
				}
			}

			err = zipWriter.Close()
			if err != nil {
				return nil, err
			}

			zipBytes = buf.Bytes()
		}

	case domain.CertificateFormatTypeJKS:
		{
			jksAlias := "certimate"
			if req.JksAlias != "" {
				jksAlias = req.JksAlias
			}

			jksKeypass := "certimate"
			if req.JksKeypass != "" {
				jksKeypass = req.JksKeypass
			}

			jksStorepass := "certimate"
			if req.JksStorepass != "" {
				jksStorepass = req.JksStorepass
			}

			certJKS, err := xcert.TransformCertificateFromPEMToJKS(certificate.Certificate, certificate.PrivateKey, jksAlias, jksKeypass, jksStorepass)
			if err != nil {
				return nil, err
			}

			certWriter, err := zipWriter.Create(fmt.Sprintf("%s.jks", canonicalName))
			if err != nil {
				return nil, err
			} else {
				_, err = certWriter.Write(certJKS)
				if err != nil {
					return nil, err
				}
			}

			readmeWriter, err := zipWriter.Create("README.txt")
			if err != nil {
				return nil, err
			} else {
				readme := fmt.Sprintf("[JKS Alias]\n%s\n\n[JKS Key Password]\n%s\n\n[JKS Store Password]\n%s\n", jksAlias, jksKeypass, jksStorepass)
				_, err = readmeWriter.Write([]byte(readme))
				if err != nil {
					return nil, err
				}
			}

			err = zipWriter.Close()
			if err != nil {
				return nil, err
			}

			zipBytes = buf.Bytes()
		}

	default:
		return nil, domain.ErrInvalidParams
	}

	resp := &dtos.CertificateDownloadResp{
		ZipBytes: zipBytes,
	}
	return resp, nil
}

func (s *CertificateService) cleanupExpiredCertificates(ctx context.Context) error {
	globalSettingsForPersistence := settings.GetGlobalSettingsForPersistence()
	if globalSettingsForPersistence.CertificatesRetentionMaxDays != 0 {
		ret, err := s.certificateRepo.DeleteWithExprs(ctx,
			dbx.NewExp(fmt.Sprintf("validityNotAfter<DATETIME('now', '-%d days')", globalSettingsForPersistence.CertificatesRetentionMaxDays)),
		)
		if err != nil {
			app.GetLogger().Error("failed to delete expired certificates", slog.Any("error", err))
			return err
		}

		if ret > 0 {
			app.GetLogger().Info(fmt.Sprintf("cleanup %d expired certificates", ret))
		}
	}

	return nil
}
