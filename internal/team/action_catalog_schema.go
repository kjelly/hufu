package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/kjelly/hufu/internal/utils"
)

// normalizeActionCatalogSchemas normalizes an entry's input and output
// schemas with the RunInputSchema dialect and applies the catalog's stricter
// rules.
func normalizeActionCatalogSchemas(id string, raw actionCatalogEntryYAML) (RunInputSchema, *RunInputSchema, []ContractFinding) {
	var findings []ContractFinding
	var input RunInputSchema
	if raw.InputSchema == nil {
		findings = append(findings, errorFinding(actionCatalogField(id, "input-schema"), FindingActionCatalogInputSchemaInvalid, fmt.Sprintf("action %q requires an input-schema", id)))
	} else {
		schema, err := normalizeRunInputSchema(*raw.InputSchema, 1)
		if err == nil {
			err = validateCatalogInputSchema(schema)
		}
		if err != nil {
			findings = append(findings, errorFinding(actionCatalogField(id, "input-schema"), FindingActionCatalogInputSchemaInvalid, fmt.Sprintf("action %q input-schema: %v", id, err)))
		}
		input = schema
	}
	var output *RunInputSchema
	if raw.OutputSchema != nil {
		schema, err := normalizeRunInputSchema(*raw.OutputSchema, 1)
		if err == nil {
			err = validateCatalogOutputSchema(schema)
		}
		if err != nil {
			findings = append(findings, errorFinding(actionCatalogField(id, "output-schema"), FindingActionCatalogOutputSchemaInvalid, fmt.Sprintf("action %q output-schema: %v", id, err)))
		}
		output = &schema
	}
	return input, output, findings
}

// validateCatalogInputSchema requires a closed object whose numbers are all
// integers: RunInputSchema keeps a number's source text, so 1 and 1.0 would
// hash differently and a proposal would never match its dispatch.
func validateCatalogInputSchema(schema RunInputSchema) error {
	if schema.Type != "object" {
		return fmt.Errorf("top-level type must be object, got %q", schema.Type)
	}
	if err := walkCatalogSchema(schema, "", func(path string, node RunInputSchema) error {
		switch node.Type {
		case "number":
			return fmt.Errorf("%s: type number is not allowed; use integer", schemaPathLabel(path))
		case "object":
			if node.AdditionalProperties == nil || *node.AdditionalProperties {
				return fmt.Errorf("%s: object must set additional-properties: false", schemaPathLabel(path))
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return validateCatalogSchemaSize(schema)
}

// validateCatalogOutputSchema checks the output schema's shape; output values
// are redacted before validation, so a redacted property name could never
// validate stably.
func validateCatalogOutputSchema(schema RunInputSchema) error {
	if schema.Type != "object" {
		return fmt.Errorf("top-level type must be object, got %q", schema.Type)
	}
	if err := walkCatalogSchema(schema, "", func(string, RunInputSchema) error { return nil }); err != nil {
		return err
	}
	return validateCatalogSchemaSize(schema)
}

// walkCatalogSchema visits every schema node depth-first and rejects any
// property name that durable events would redact.
func walkCatalogSchema(schema RunInputSchema, path string, visit func(string, RunInputSchema) error) error {
	if err := visit(path, schema); err != nil {
		return err
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		childPath := path + "." + name
		if utils.IsRedactedJSONKey(name) {
			return fmt.Errorf("%s: property name would be redacted in durable events; rename it", schemaPathLabel(childPath))
		}
		if err := walkCatalogSchema(schema.Properties[name], childPath, visit); err != nil {
			return err
		}
	}
	if schema.Items != nil {
		if err := walkCatalogSchema(*schema.Items, path+"[]", visit); err != nil {
			return err
		}
	}
	return nil
}

func validateCatalogSchemaSize(schema RunInputSchema) error {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("encode schema: %w", err)
	}
	if len(encoded) > maxActionSchemaBytes {
		return errors.New("normalized schema exceeds 64 KiB")
	}
	return nil
}

func schemaPathLabel(path string) string {
	if path == "" {
		return "schema"
	}
	return "schema" + path
}
