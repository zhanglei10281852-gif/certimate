package migrations

import (
	"errors"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(func(app core.App) error {
		tracer := NewTracer("v0.4.36")
		tracer.Printf("go ...")

		// update collection `workflow`
		//   - add field `lifecycle`
		//
		// `lifecycle` is a persisted ownership fence. The empty value means an
		// ordinary, active workflow; "closing" means the workflow is being
		// deleted and must reject new manual/scheduled runs, while in-flight
		// runs are drained before the record itself is committed to deletion.
		{
			collection, err := app.FindCollectionByNameOrId("tovyif5ax6j62ur")
			if err != nil {
				return err
			}

			if collection.Fields.GetByName("lifecycle") == nil {
				if err := collection.Fields.AddMarshaledJSONAt(len(collection.Fields), []byte(`{
					"autogeneratePattern": "",
					"hidden": false,
					"id": "wflc2026aa",
					"max": 20,
					"min": 0,
					"name": "lifecycle",
					"pattern": "",
					"presentable": false,
					"primaryKey": false,
					"required": false,
					"system": false,
					"type": "text"
				}`)); err != nil {
					return err
				}

				if err := app.Save(collection); err != nil {
					return err
				}

				tracer.Printf("collection '%s' updated", collection.Name)
			}
		}

		// update collection `certificate`
		//   - add field `sourceSnapshot`
		//
		// Certificates are intentionally kept when their owning workflow/run is
		// deleted (both relations use cascadeDelete=false and are therefore
		// nulled on deletion). The snapshot preserves enough provenance so that
		// such a certificate does not become an unexplainable orphan.
		{
			collection, err := app.FindCollectionByNameOrId("4szxr9x43tpj6np")
			if err != nil {
				return err
			}

			if collection.Fields.GetByName("sourceSnapshot") == nil {
				if err := collection.Fields.AddMarshaledJSONAt(len(collection.Fields), []byte(`{
					"hidden": false,
					"id": "csnap2026aa",
					"maxSize": 200000,
					"name": "sourceSnapshot",
					"presentable": false,
					"required": false,
					"system": false,
					"type": "json"
				}`)); err != nil {
					return err
				}

				if err := app.Save(collection); err != nil {
					return err
				}

				tracer.Printf("collection '%s' updated", collection.Name)
			}
		}

		tracer.Printf("done")
		return nil
	}, func(app core.App) error {
		return errors.ErrUnsupported
	})
}
