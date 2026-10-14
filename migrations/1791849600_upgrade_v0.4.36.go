package migrations

import (
	"errors"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"

	"github.com/certimate-go/certimate/internal/domain"
)

func init() {
	m.Register(func(app core.App) error {
		// skip if the collection already exists (idempotent upgrade)
		if _, err := app.FindCollectionByNameOrId(domain.CollectionNameCertificateRevocation); err == nil {
			return nil
		}

		tracer := NewTracer("v0.4.36")
		tracer.Printf("go ...")

		// create collection `certificate_revocation`
		//   - persists certificate revocation attempts and their owner
		//     before the ACME revocation is actually called
		{
			collection := core.NewBaseCollection(domain.CollectionNameCertificateRevocation)

			collection.Fields.Add(
				&core.TextField{Name: "certificateRef", Required: true},
				&core.TextField{Name: "status", Required: true},
				&core.TextField{Name: "stage"},
				&core.TextField{Name: "owner"},
				&core.DateField{Name: "heartbeat"},
				&core.BoolField{Name: "converged"},
				&core.TextField{Name: "errorType"},
				&core.NumberField{Name: "errorStatusCode"},
				&core.TextField{Name: "errorMessage"},
				&core.DateField{Name: "startedAt"},
				&core.DateField{Name: "endedAt"},
			)

			collection.Indexes = append(collection.Indexes,
				"CREATE INDEX `idx_certRevCertRef` ON `certificate_revocation` (`certificateRef`)",
				"CREATE INDEX `idx_certRevStatus` ON `certificate_revocation` (`status`)",
			)

			if err := app.Save(collection); err != nil {
				return err
			}

			tracer.Printf("collection '%s' created", collection.Name)
		}

		tracer.Printf("done")
		return nil
	}, func(app core.App) error {
		return errors.ErrUnsupported
	})
}
