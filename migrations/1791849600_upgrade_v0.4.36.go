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
		//   - add field `draftVersion` (草稿代际，仅保存草稿/发布/回滚时推进)
		//   - add field `contentVersion` (已发布代际，仅发布成功时推进)
		{
			collection, err := app.FindCollectionByNameOrId("tovyif5ax6j62ur")
			if err != nil {
				return err
			}

			if collection.Fields.GetByName("draftVersion") == nil {
				if err := collection.Fields.AddMarshaledJSONAt(len(collection.Fields), []byte(`{
					"hidden": false,
					"id": "drftver1",
					"max": null,
					"min": 0,
					"name": "draftVersion",
					"onlyInt": true,
					"presentable": false,
					"required": false,
					"system": false,
					"type": "number"
				}`)); err != nil {
					return err
				}
			}

			if collection.Fields.GetByName("contentVersion") == nil {
				if err := collection.Fields.AddMarshaledJSONAt(len(collection.Fields), []byte(`{
					"hidden": false,
					"id": "contver1",
					"max": null,
					"min": 0,
					"name": "contentVersion",
					"onlyInt": true,
					"presentable": false,
					"required": false,
					"system": false,
					"type": "number"
				}`)); err != nil {
					return err
				}
			}

			if err := app.Save(collection); err != nil {
				return err
			}
		}

		tracer.Printf("done")
		return nil
	}, func(app core.App) error {
		return errors.ErrUnsupported
	})
}
