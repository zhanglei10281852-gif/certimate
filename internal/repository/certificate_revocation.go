package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/certimate-go/certimate/internal/app"
	"github.com/certimate-go/certimate/internal/domain"
)

type CertificateRevocationRepository struct{}

func NewCertificateRevocationRepository() *CertificateRevocationRepository {
	return &CertificateRevocationRepository{}
}

func (r *CertificateRevocationRepository) GetLatestByCertificateId(ctx context.Context, certificateId string) (*domain.CertificateRevocation, error) {
	records, err := app.GetApp().FindRecordsByFilter(
		domain.CollectionNameCertificateRevocation,
		"certificateRef={:certificateId}",
		"-created",
		1, 0,
		dbx.Params{"certificateId": certificateId},
	)
	if err != nil {
		return nil, err
	}

	if len(records) == 0 {
		return nil, domain.ErrRecordNotFound
	}

	return r.castRecordToModel(records[0])
}

func (r *CertificateRevocationRepository) ListProcessing(ctx context.Context) ([]*domain.CertificateRevocation, error) {
	records, err := app.GetApp().FindAllRecords(
		domain.CollectionNameCertificateRevocation,
		dbx.NewExp("status={:status}", dbx.Params{"status": domain.CertificateRevocationStatusTypeProcessing.String()}),
	)
	if err != nil {
		return nil, err
	}

	ret := make([]*domain.CertificateRevocation, 0, len(records))
	for _, record := range records {
		m, err := r.castRecordToModel(record)
		if err != nil {
			return nil, err
		}
		ret = append(ret, m)
	}
	return ret, nil
}

// ListUnconverged returns succeeded attempts whose local certificate state has
// not been converged yet.
func (r *CertificateRevocationRepository) ListUnconverged(ctx context.Context) ([]*domain.CertificateRevocation, error) {
	records, err := app.GetApp().FindAllRecords(
		domain.CollectionNameCertificateRevocation,
		dbx.NewExp("status={:status} AND converged=0", dbx.Params{
			"status": domain.CertificateRevocationStatusTypeSucceeded.String(),
		}),
	)
	if err != nil {
		return nil, err
	}

	ret := make([]*domain.CertificateRevocation, 0, len(records))
	for _, record := range records {
		m, err := r.castRecordToModel(record)
		if err != nil {
			return nil, err
		}
		ret = append(ret, m)
	}
	return ret, nil
}

// CreateProcessingIfAbsent persists a new processing/prepared attempt for the
// certificate unless a processing attempt already exists. It returns the
// attempt and true when a new record was created, or the existing processing
// attempt and false when one already exists.
func (r *CertificateRevocationRepository) CreateProcessingIfAbsent(ctx context.Context, attempt *domain.CertificateRevocation) (*domain.CertificateRevocation, bool, error) {
	if attempt == nil {
		return nil, false, fmt.Errorf("the attempt is nil")
	}

	created := false
	txErr := app.GetApp().RunInTransaction(func(txApp core.App) error {
		collection, err := txApp.FindCollectionByNameOrId(domain.CollectionNameCertificateRevocation)
		if err != nil {
			return err
		}

		existingRecord, err := txApp.FindFirstRecordByFilter(
			domain.CollectionNameCertificateRevocation,
			"certificateRef={:certificateId} && status={:status}",
			dbx.Params{
				"certificateId": attempt.CertificateRef,
				"status":        domain.CertificateRevocationStatusTypeProcessing.String(),
			},
		)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		if existingRecord != nil {
			existing, err := r.castRecordToModel(existingRecord)
			if err != nil {
				return err
			}
			attempt = existing
			return nil
		}

		record := core.NewRecord(collection)
		record.Set("certificateRef", attempt.CertificateRef)
		record.Set("status", attempt.Status)
		record.Set("stage", attempt.Stage)
		record.Set("owner", attempt.Owner)
		record.Set("heartbeat", attempt.Heartbeat)
		record.Set("converged", attempt.Converged)
		record.Set("errorType", attempt.ErrorType)
		record.Set("errorStatusCode", attempt.ErrorStatusCode)
		record.Set("errorMessage", attempt.ErrorMessage)
		record.Set("startedAt", attempt.StartedAt)
		record.Set("endedAt", attempt.EndedAt)
		if err := txApp.Save(record); err != nil {
			return err
		}

		persisted, err := r.castRecordToModel(record)
		if err != nil {
			return err
		}
		attempt = persisted
		created = true
		return nil
	})
	if txErr != nil {
		return nil, false, txErr
	}

	return attempt, created, nil
}

func (r *CertificateRevocationRepository) Save(ctx context.Context, attempt *domain.CertificateRevocation) (*domain.CertificateRevocation, error) {
	if attempt == nil {
		return nil, fmt.Errorf("the attempt is nil")
	}

	collection, err := app.GetApp().FindCollectionByNameOrId(domain.CollectionNameCertificateRevocation)
	if err != nil {
		return attempt, err
	}

	var record *core.Record
	if attempt.Id == "" {
		record = core.NewRecord(collection)
	} else {
		record, err = app.GetApp().FindRecordById(collection, attempt.Id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return attempt, domain.ErrRecordNotFound
			}
			return attempt, err
		}
	}

	record.Set("certificateRef", attempt.CertificateRef)
	record.Set("status", attempt.Status)
	record.Set("stage", attempt.Stage)
	record.Set("owner", attempt.Owner)
	record.Set("heartbeat", attempt.Heartbeat)
	record.Set("converged", attempt.Converged)
	record.Set("errorType", attempt.ErrorType)
	record.Set("errorStatusCode", attempt.ErrorStatusCode)
	record.Set("errorMessage", attempt.ErrorMessage)
	record.Set("startedAt", attempt.StartedAt)
	record.Set("endedAt", attempt.EndedAt)
	if err := app.GetApp().Save(record); err != nil {
		return attempt, err
	}

	attempt.Id = record.Id
	attempt.CreatedAt = record.GetDateTime("created").Time()
	attempt.UpdatedAt = record.GetDateTime("updated").Time()
	return attempt, nil
}

func (r *CertificateRevocationRepository) castRecordToModel(record *core.Record) (*domain.CertificateRevocation, error) {
	if record == nil {
		return nil, fmt.Errorf("the record is nil")
	}

	attempt := &domain.CertificateRevocation{
		Meta: domain.Meta{
			Id:        record.Id,
			CreatedAt: record.GetDateTime("created").Time(),
			UpdatedAt: record.GetDateTime("updated").Time(),
		},
		CertificateRef:  record.GetString("certificateRef"),
		Status:          record.GetString("status"),
		Stage:           record.GetString("stage"),
		Owner:           record.GetString("owner"),
		Heartbeat:       record.GetDateTime("heartbeat").Time(),
		Converged:       record.GetBool("converged"),
		ErrorType:       record.GetString("errorType"),
		ErrorStatusCode: record.GetInt("errorStatusCode"),
		ErrorMessage:    record.GetString("errorMessage"),
		StartedAt:       record.GetDateTime("startedAt").Time(),
		EndedAt:         record.GetDateTime("endedAt").Time(),
	}
	return attempt, nil
}
