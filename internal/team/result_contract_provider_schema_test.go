package team

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResultGuidancePreservesVisibleConditionalConstraints(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, field string
		visible     bool
	}{
		{"model_field", "review_required", true},
		{"omitted_runtime_field", "receipt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var schema map[string]any
			raw := `{"type":"object","properties":{"severity":{"type":"string"},"review_required":{"type":"boolean"}},"allOf":[{"if":{"properties":{"severity":{"const":"warning"}},"required":["severity"]},"then":{"properties":{"` + tc.field + `":{"const":true}},"required":["` + tc.field + `"]}}]}`
			if err := json.Unmarshal([]byte(raw), &schema); err != nil {
				t.Fatal(err)
			}
			guidance := projectResultGuidanceSchema(schema)
			_, retained := guidance["allOf"]
			if retained != tc.visible {
				t.Fatalf("conditional retained=%t, want %t: %#v", retained, tc.visible, guidance)
			}
			portable := projectProviderResultSchema(schema)
			if portable["allOf"] != nil || !dynamicSchemaEligible(portable) {
				t.Fatal("prompt constraints leaked into portable provider descriptor")
			}
			encoded, err := json.Marshal(guidance)
			if err != nil || (!tc.visible && strings.Contains(string(encoded), "receipt")) {
				t.Fatalf("omitted fields reappeared in guidance: %s, %v", encoded, err)
			}
		})
	}
}

func TestResultGuidanceOmitsCompositeConditionalConstraints(t *testing.T) {
	t.Parallel()
	for _, constraint := range []string{
		`{"then":{"properties":{"rows":{"items":{"properties":{"receipt":{"const":"runtime"}}}}}}}`,
		`{"then":{"additionalProperties":false}}`,
		`{"then":{"const":{"receipt":"runtime"}}}`,
		`{"then":{"enum":[{"receipt":"runtime"}]}}`,
	} {
		var schema map[string]any
		raw := `{"type":"object","properties":{"rows":{"type":"array","items":{"type":"string"}},"status":{"type":"string"}},"allOf":[` + constraint + `]}`
		if err := json.Unmarshal([]byte(raw), &schema); err != nil {
			t.Fatal(err)
		}
		if guidance := projectResultGuidanceSchema(schema); guidance["allOf"] != nil {
			t.Fatalf("composite canonical constraint leaked into model projection: %#v", guidance)
		}
	}
}

func TestResultGuidancePreservesPrimitiveArrayConditions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, item string
		retained   bool
	}{
		{"string", `{"type":"string"}`, true},
		{"object", `{"type":"object","properties":{"receipt":{"type":"string"}}}`, false},
		{"unknown", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"type":"object","properties":{"status":{"type":"string"},"rows":{"type":"array","items":` + tc.item + `}},"allOf":[{"if":{"properties":{"status":{"const":"preserved"}}},"then":{"properties":{"rows":{"maxItems":0}}}}]}`
			value, err := decodeResultContractJSON([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			schema := value.(map[string]any)
			guidance := projectResultGuidanceSchema(schema)
			if (guidance["allOf"] != nil) != tc.retained {
				t.Fatalf("conditional retained incorrectly: %#v", guidance)
			}
			if portable := projectProviderResultSchema(schema); portable["allOf"] != nil {
				t.Fatal("conditional leaked into portable descriptor")
			}
		})
	}
}

func TestProviderResultSchemaPreservesNestedRequiredFields(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const schema = `{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object","additionalProperties":false,"required":["question","findings","limitations"],
		"properties":{
			"question":{"type":"string","minLength":1},
			"limitations":{"type":"string"},
			"findings":{"type":"array","minItems":1,"items":{"$ref":"#/$defs/finding"}}
		},
		"$defs":{
			"finding":{"type":"object","additionalProperties":false,"required":["claim","assessment","sources","open_questions"],
				"properties":{
					"claim":{"type":"string"},"assessment":{"type":"string","enum":["supported","unverified"]},
					"open_questions":{"type":"string"},"sources":{"type":"array","items":{"$ref":"#/$defs/source"}}
				},
				"allOf":[{"if":{"properties":{"assessment":{"const":"supported"}},"required":["assessment"]},
					"then":{"properties":{"sources":{"minItems":1}}}}]
			},
			"source":{"type":"object","additionalProperties":false,"required":["title","url","publisher","quote"],
				"properties":{"title":{"type":"string"},"url":{"type":"string","pattern":"^https://"},
					"publisher":{"type":"string"},"quote":{"type":"string"}}}
		}
	}`
	writeResultContractSchema(t, dir, "nested.json", schema)
	compiled, err := compileResultContractSchema(dir, "nested.json")
	if err != nil {
		t.Fatal(err)
	}
	canonical := string(compiled.CanonicalSchema)
	ref := compiled.ref(true)
	projected := providerVisibleResultPayloadSchema(ref, compiled)
	if !dynamicSchemaEligible(projected) {
		t.Fatalf("projection is not provider-portable: %#v", projected)
	}
	const valid = `{"question":"Question","limitations":"None","findings":[{"claim":"Claim","assessment":"supported","open_questions":"None","sources":[{"title":"Title","url":"https://example.com","publisher":"Publisher","quote":"Exact words"}]}]}`
	for _, tc := range []struct {
		name          string
		mutate        func(map[string]any)
		providerValid bool
		runtimeValid  bool
	}{
		{"valid", func(map[string]any) {}, true, true},
		{"missing_root_field", func(v map[string]any) { delete(v, "limitations") }, false, false},
		{"missing_finding_field", func(v map[string]any) {
			delete(v["findings"].([]any)[0].(map[string]any), "open_questions")
		}, false, false},
		{"missing_source_field", func(v map[string]any) {
			finding := v["findings"].([]any)[0].(map[string]any)
			delete(finding["sources"].([]any)[0].(map[string]any), "publisher")
		}, false, false},
		{"invalid_enum", func(v map[string]any) {
			v["findings"].([]any)[0].(map[string]any)["assessment"] = "unsupported"
		}, false, false},
		{"runtime_conditional", func(v map[string]any) {
			v["findings"].([]any)[0].(map[string]any)["sources"] = []any{}
		}, true, false},
		{"runtime_length", func(v map[string]any) { v["question"] = "" }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := decodeResultContractJSON([]byte(valid))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(value.(map[string]any))
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			_, providerErr := validateDynamicArguments(projected, raw)
			_, runtimeErr := validateStructuredResultPayload(compiled, ref, raw)
			if (providerErr == nil) != tc.providerValid || (runtimeErr == nil) != tc.runtimeValid {
				t.Fatalf("provider/runtime errors = %v / %v, want valid=%t/%t", providerErr, runtimeErr, tc.providerValid, tc.runtimeValid)
			}
		})
	}
	if string(compiled.CanonicalSchema) != canonical {
		t.Fatal("provider projection mutated the pinned schema")
	}
}

func TestProviderResultSchemaFallsBackForUnsafeReferenceShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		schema string
	}{
		{"cyclic", `{"type":"object","properties":{"child":{"$ref":"#/$defs/node"}},"$defs":{"node":{"type":"object","properties":{"child":{"$ref":"#/$defs/node"}}}}}`},
		{"unresolved", `{"type":"object","properties":{"child":{"$ref":"#/$defs/absent"}}}`},
		{"external", `{"type":"object","properties":{"child":{"$ref":"other.json"}}}`},
		{"constrained_ref_sibling", `{"type":"object","properties":{"child":{"$ref":"#/$defs/node","type":"integer"}},"$defs":{"node":{"type":"string"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compiled := &CompiledResultContract{ID: "complex.json", CanonicalSchema: []byte(tc.schema)}
			projected := providerVisibleResultPayloadSchema(compiled.ref(true), compiled)
			if projected["type"] != "object" || projected["properties"] != nil {
				t.Fatalf("unsafe shape did not keep the open fallback: %#v", projected)
			}
		})
	}
}

func TestProviderResultSchemaProjectionBounds(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"depth", "nodes"} {
		t.Run(name, func(t *testing.T) {
			root := map[string]any{"type": "object"}
			if name == "depth" {
				node := root
				for range maxDynamicSchemaDepth + 1 {
					child := map[string]any{"type": "object"}
					node["properties"] = map[string]any{"child": child}
					node = child
				}
			} else {
				properties := make(map[string]any)
				for i := range maxDynamicSchemaNodes + 1 {
					properties[string(rune(0x1000+i))] = map[string]any{"type": "string"}
				}
				root["properties"] = properties
			}
			if projected := projectProviderResultSchema(root); projected != nil {
				t.Fatal("oversized shape bypassed provider schema bounds")
			}
		})
	}
}

func TestProviderResultSchemaDoesNotClosePatternProperties(t *testing.T) {
	t.Parallel()
	root := map[string]any{
		"type": "object", "additionalProperties": false,
		"patternProperties": map[string]any{"^item": map[string]any{"type": "string"}},
	}
	projected := projectProviderResultSchema(root)
	if _, err := validateDynamicArguments(projected, []byte(`{"item1":"valid"}`)); err != nil {
		t.Fatalf("projection narrowed a valid pattern-property input: %v", err)
	}
}
