package belief

import (
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestDiagnosticSchemasMatchTypedContracts(t *testing.T) {
	_, _, policy := diagnosticTestInputs(t)
	for _, test := range []struct {
		file  string
		value any
	}{
		{"diagnostic-policy.schema.json", policy},
		{"diagnostic-evidence.schema.json", diagnosticTestEvidence()},
		{"diagnostic-adapter.schema.json", map[string]any{"schema_version": 1, "version": DiagnosticAdapterVersion, "source": "verification", "experiments": []string{"comparison"}, "outcomes": []string{"pass", "fail"}}},
	} {
		t.Run(test.file, func(t *testing.T) {
			data, err := os.ReadFile(test.file)
			if err != nil {
				t.Fatal(err)
			}
			var document any
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			compiler := jsonschema.NewCompiler()
			compiler.DefaultDraft(jsonschema.Draft2020)
			const resource = "https://hufu.invalid/diagnostic-schema.json"
			if err := compiler.AddResource(resource, document); err != nil {
				t.Fatal(err)
			}
			schema, err := compiler.Compile(resource)
			if err != nil {
				t.Fatal(err)
			}
			data, err = json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(value); err != nil {
				t.Fatal(err)
			}
			value["unexpected"] = true
			if schema.Validate(value) == nil {
				t.Fatal("schema accepted unknown fields")
			}
			delete(value, "unexpected")
			value["schema_version"] = nil
			if schema.Validate(value) == nil {
				t.Fatal("schema accepted null version")
			}
		})
	}
}
