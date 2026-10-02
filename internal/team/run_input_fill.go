package team

import (
	"bytes"
	"encoding/json"
)

// fillSingleValueRequiredProperties adds each required object property that a
// semantic candidate omits when the schema allows exactly one value for it.
// That value follows from the schema alone, so supplying it never guesses; a
// model told to omit whatever the request does not state would otherwise leave
// the candidate invalid. Present values, even wrong ones, are left for
// validation, and anything that is not a JSON object is returned unchanged.
func fillSingleValueRequiredProperties(schema RunInputSchema, raw json.RawMessage) json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || decoder.More() {
		return raw
	}
	if !fillSingleValueRequired(schema, value) {
		return raw
	}
	filled, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return filled
}

func fillSingleValueRequired(schema RunInputSchema, value any) bool {
	object, ok := value.(map[string]any)
	if !ok || schema.Type != "object" {
		return false
	}
	changed := false
	for _, name := range schema.RequiredProperties {
		property, declared := schema.Properties[name]
		if _, present := object[name]; present || !declared || len(property.Enum) != 1 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(property.Enum[0]))
		decoder.UseNumber()
		var only any
		if decoder.Decode(&only) != nil {
			continue
		}
		object[name] = only
		changed = true
	}
	for name, property := range schema.Properties {
		if nested, present := object[name]; present && fillSingleValueRequired(property, nested) {
			changed = true
		}
	}
	return changed
}
