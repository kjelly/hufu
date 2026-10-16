package belief

import (
	"encoding/json/v2"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestModelSchemaMatchesFixture(t *testing.T) {
	schemaBytes, err := os.ReadFile("model.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schemaDocument any
	if err := json.Unmarshal(schemaBytes, &schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("model.json", schemaDocument); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("model.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/synthetic-model.json")
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatal(err)
	}
	object := value.(map[string]any)
	object["synthetic"] = nil
	if err := schema.Validate(object); err == nil {
		t.Fatal("schema accepted missing model classification")
	}
}

func TestPolicySchemaMatchesFixture(t *testing.T) {
	schemaBytes, err := os.ReadFile("policy.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(schemaBytes, &document); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("policy.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("policy.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/synthetic-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePolicy(data); err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatal(err)
	}
	value.(map[string]any)["mode"] = "active"
	if err := schema.Validate(value); err == nil {
		t.Fatal("policy schema allowed active control")
	}
}
