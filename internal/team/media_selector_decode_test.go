package team

import (
	"encoding/json/v2"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/media"
)

func TestModelTaskImageInputsPreserveExplicitOrderAndRejectOverrides(t *testing.T) {
	for _, test := range []struct {
		name  string
		field string
		value any
		valid bool
	}{
		{"ordered and repeated", "image_inputs", []media.ImageInputSelector{{TurnID: "turn", PartIndex: 2}, {TurnID: "turn", PartIndex: 1}, {TurnID: "turn", PartIndex: 2}}, true},
		{"null", "image_inputs", nil, false},
		{"empty", "image_inputs", []any{}, false},
		{"missing index", "image_inputs", []any{map[string]any{"turn_id": "turn"}}, false},
		{"path", "image_inputs", []any{map[string]any{"turn_id": "turn", "part_index": 1, "path": "image.png"}}, false},
		{"CAS ID", "image_inputs", []any{map[string]any{"turn_id": "turn", "part_index": 1, "media_id": "image-id"}}, false},
		{"binding", "media_binding", map[string]any{}, false},
		{"binding alias", "MediaBinding", map[string]any{}, false},
		{"binding dash", "media-binding", map[string]any{}, false},
		{"digest", "media_input_digest", "digest", false},
		{"digest alias", "MediaInputDigest", "digest", false},
		{"digest dash", "media-input-digest", "digest", false},
		{"selector alias", "image-inputs", []any{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{"tasks": []any{map[string]any{"agent": "worker", "goal": "inspect selected images", test.field: test.value}}})
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := decodeModelTaskDefs(data)
			if !test.valid {
				if err == nil {
					t.Fatal("invalid model image request silently became text work")
				}
				return
			}
			if err != nil || len(tasks) != 1 || !reflect.DeepEqual(tasks[0].ImageInputs, test.value) || tasks[0].MediaBinding != nil || tasks[0].MediaInputDigest != "" {
				t.Fatal("typed selection lost order or manufactured runtime authority", err)
			}
		})
	}
}
