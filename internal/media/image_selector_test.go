package media

import (
	"encoding/json/v2"
	"errors"
	"testing"
)

func TestImageInputSelectorRequiresOnlyExplicitOccurrenceFields(t *testing.T) {
	for _, test := range []struct {
		name  string
		data  string
		valid bool
	}{
		{"first part", `{"turn_id":"turn","part_index":0}`, true},
		{"later part", `{"turn_id":"turn","part_index":2}`, true},
		{"missing index", `{"turn_id":"turn"}`, false},
		{"missing turn", `{"part_index":1}`, false},
		{"null index", `{"turn_id":"turn","part_index":null}`, false},
		{"empty turn", `{"turn_id":"","part_index":1}`, false},
		{"negative index", `{"turn_id":"turn","part_index":-1}`, false},
		{"fractional index", `{"turn_id":"turn","part_index":1.5}`, false},
		{"path override", `{"turn_id":"turn","part_index":1,"path":"image.png"}`, false},
		{"CAS override", `{"turn_id":"turn","part_index":1,"media_id":"image-id"}`, false},
		{"snapshot override", `{"turn_id":"turn","part_index":1,"snapshot_id":"snapshot"}`, false},
		{"duplicate index", `{"turn_id":"turn","part_index":1,"part_index":2}`, false},
		{"null selector", `null`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var selection ImageInputSelector
			err := json.Unmarshal([]byte(test.data), &selection)
			if test.valid {
				if err != nil || selection.TurnID != "turn" || selection.Validate() != nil {
					t.Fatal("valid selector rejected", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid selector was accepted")
			}
			if rejection, ok := errors.AsType[*Error](err); ok && rejection.Code != BindingConflict {
				t.Fatal("invalid selector returned the wrong rejection class", err)
			}
		})
	}
}
