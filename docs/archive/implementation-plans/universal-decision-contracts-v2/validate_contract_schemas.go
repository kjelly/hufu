//go:build ignore

// Command validate_contract_schemas validates the bundle's JSON Schemas and
// positive/negative fixtures without network access or a Python environment.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func main() {
	root, err := os.Getwd()
	must(err)

	valid := map[string]string{
		"evidence.valid.json":                   "primary-evidence.schema.json",
		"role-plan.valid.json":                  "role-binding-plan.schema.json",
		"role-constraints.valid.json":           "role-constraints.schema.json",
		"event.opened.valid.json":               "decision-events.schema.json",
		"logical-run.valid.json":                "logical-run.schema.json",
		"result.completed.valid.json":           "decision-output.schema.json",
		"result.blocked.valid.json":             "decision-output.schema.json",
		"result.preview.valid.json":             "decision-output.schema.json",
		"runtime.requirement.valid.json":        "runtime-contracts.schema.json",
		"runtime.authority.valid.json":          "runtime-contracts.schema.json",
		"runtime.occurrence.valid.json":         "runtime-contracts.schema.json",
		"runtime.terminal-proof.valid.json":     "runtime-contracts.schema.json",
		"runtime.manifest-proof.valid.json":     "runtime-contracts.schema.json",
		"runtime.terminal-extension.valid.json": "runtime-contracts.schema.json",
	}
	invalid := map[string]string{
		"evidence.unknown-field.invalid.json":     "primary-evidence.schema.json",
		"event.unknown-version.invalid.json":      "decision-events.schema.json",
		"bundle.mutated.invalid.json":             "profile-bundles.schema.json",
		"runtime.terminal-extension.invalid.json": "runtime-contracts.schema.json",
	}

	compiled := make(map[string]*jsonschema.Schema)
	for _, schemaName := range append(values(valid), values(invalid)...) {
		if compiled[schemaName] != nil {
			continue
		}
		path := filepath.Join(root, schemaName)
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		must(compiler.AddResource(path, decode(path)))
		compiled[schemaName], err = compiler.Compile(path)
		must(err)
	}

	for fixture, schemaName := range valid {
		must(compiled[schemaName].Validate(decode(filepath.Join(root, "fixtures", fixture))))
	}
	for fixture, schemaName := range invalid {
		if err := compiled[schemaName].Validate(decode(filepath.Join(root, "fixtures", fixture))); err == nil {
			panic("invalid fixture accepted: " + fixture)
		}
	}

	report := map[string]any{
		"schemas_valid":             len(compiled),
		"valid_examples_accepted":   len(valid),
		"invalid_examples_rejected": len(invalid),
	}
	out, err := json.MarshalIndent(report, "", "  ")
	must(err)
	fmt.Println(string(out))
}

func decode(path string) any {
	f, err := os.Open(path)
	must(err)
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.UseNumber()
	var value any
	must(decoder.Decode(&value))
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		panic("trailing JSON value: " + path)
	}
	return value
}

func values(m map[string]string) []string {
	values := make([]string, 0, len(m))
	for _, value := range m {
		values = append(values, value)
	}
	return values
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
