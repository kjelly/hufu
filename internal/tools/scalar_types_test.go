package tools

import (
	"testing"

	"charm.land/fantasy"
)

func TestCanonicalToolArgumentsScalarTypes(t *testing.T) {
	info := fantasy.ToolInfo{Parameters: map[string]any{
		"enabled": map[string]any{"type": "boolean"},
		"done":    map[string]any{"const": true},
		"count":   map[string]any{"type": "integer"},
		"score":   map[string]any{"type": "number"},
		"id":      map[string]any{"type": "string"},
		"status":  map[string]any{"enum": []any{"blocked", "success"}},
		"union":   map[string]any{"type": []any{"string", "boolean"}},
		"choice":  map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}},
		"nested": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
			"flag": map[string]any{"type": "boolean"},
		}}},
	}}
	for _, tc := range []struct{ input, want string }{
		{`{"enabled":"false","done":"true","count":"9007199254740993","score":" 1.25e-3 ","status":"SUCCESS"}`, `{"count":9007199254740993,"done":true,"enabled":false,"score":1.25e-3,"status":"success"}`},
		{`{"nested":[{"flag":"true"}]}`, `{"nested":[{"flag":true}]}`},
		{`{"id":"00123","union":"false","choice":"42"}`, ""},
		{`{"enabled":"yes","done":"1","count":"01","score":"NaN"}`, ""},
		{`{"enabled":"TRUE","count":"1.2","score":"Infinity"}`, ""},
		{`{"count":"1e2"}`, ""},
		{`{"score":"+1"}`, ""},
		{`{"score":"1e1000"}`, `{"score":1e1000}`},
		{`{"enabled":"true","enabled":"false"}`, ""},
		{`{"nested":[{"flag":"true","flag":"false"}]}`, ""},
		{`{"enabled":"true"} false`, ""},
		{`{"enabled":true,"count":3}`, ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, changed := CanonicalToolArguments(tc.input, info)
			want := tc.want
			if want == "" {
				want = tc.input
			}
			if got != want || changed != (tc.want != "") {
				t.Fatalf("got %s changed=%v, want %s", got, changed, want)
			}
		})
	}
}

func TestCanonicalSchemaScalarTypesReferencesAndAmbiguity(t *testing.T) {
	root := map[string]any{"type": "object", "$defs": map[string]any{
		"counter": map[string]any{"type": "integer"},
		"loop":    map[string]any{"$ref": "#/$defs/loop"},
	}, "properties": map[string]any{
		"count":       map[string]any{"$ref": "#/$defs/counter"},
		"loop":        map[string]any{"$ref": "#/$defs/loop"},
		"ambiguous":   map[string]any{"$ref": "#/$defs/counter", "type": "string"},
		"conditional": map[string]any{"allOf": []any{map[string]any{"type": "integer"}}},
		"flags":       map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "boolean"}},
	}}
	value := map[string]any{"count": "9007199254740993", "loop": "3", "ambiguous": "3", "conditional": "3", "flags": map[string]any{"a": "true"}}
	got, changed := CanonicalSchemaScalarTypes(value, root)
	if !changed || got.(map[string]any)["count"] == "9007199254740993" {
		t.Fatal("local reference was not normalized")
	}
	for _, key := range []string{"loop", "ambiguous", "conditional"} {
		if value[key] != "3" {
			t.Fatalf("guessed type at %s", key)
		}
	}
	if value["flags"].(map[string]any)["a"] != true {
		t.Fatal("typed map value was not normalized")
	}
}
