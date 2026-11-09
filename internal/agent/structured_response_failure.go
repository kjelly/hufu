package agent

import (
	"encoding/json/jsontext"
	"fmt"
)

// StructuredResponseIssue describes the SDK function-argument boundary, not
// raw HTTP data or validation of the claims inside the result object.
// All values are runtime-derived vocabulary; rejected arguments stay separate.
type StructuredResponseIssue struct {
	Code     string `json:"code"`
	Pointer  string `json:"pointer"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

func (issue StructuredResponseIssue) Validate() error {
	switch issue.Code {
	case "invalid_json", "type", "missing_property", "additional_property", "trailing_data":
	default:
		return fmt.Errorf("invalid structured response issue code")
	}
	if issue.Pointer != "" && issue.Pointer != "/result" {
		return fmt.Errorf("invalid structured response issue pointer")
	}
	if issue.Expected != "object" {
		return fmt.Errorf("invalid structured response expected type")
	}
	switch issue.Actual {
	case "object", "array", "string", "number", "boolean", "null", "missing", "invalid_json":
		return nil
	default:
		return fmt.Errorf("invalid structured response actual type")
	}
}

func (issue StructuredResponseIssue) Diagnostic() string {
	return fmt.Sprintf("return_result response envelope at %q: %s; expected %s, received %s. Supply exactly {\"result\": <complete claims object>}; result must be an object, not a JSON-encoded string, array or null. Do not change the claims to satisfy transport formatting.", issue.Pointer, issue.Code, issue.Expected, issue.Actual)
}

// StructuredResponseError preserves complete, bounded SDK function arguments.
// Callers must redact Arguments before persistence and treat them as untrusted
// evidence. Error intentionally never includes those model-controlled bytes.
type StructuredResponseError struct {
	Issue     StructuredResponseIssue `json:"issue"`
	Arguments string                  `json:"-"`
}

func (failure *StructuredResponseError) Error() string {
	return fmt.Sprintf("%v: %s", ErrStructuredResponseTool, failure.Issue.Diagnostic())
}

func (*StructuredResponseError) Unwrap() error { return ErrStructuredResponseTool }

func responseValueType(kind jsontext.Kind) string {
	switch kind {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case '0':
		return "number"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "invalid_json"
	}
}
