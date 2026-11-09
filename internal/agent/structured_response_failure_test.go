package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestStructuredResponseClaimsReportsEnvelopeTypes(t *testing.T) {
	for _, tc := range []struct {
		name, input, code, pointer, actual string
	}{
		{"string", `{"result":"secret-claim"}`, "type", "/result", "string"},
		{"array", `{"result":[]}`, "type", "/result", "array"},
		{"null", `{"result":null}`, "type", "/result", "null"},
		{"number", `{"result":42}`, "type", "/result", "number"},
		{"boolean", `{"result":true}`, "type", "/result", "boolean"},
		{"missing", `{}`, "missing_property", "/result", "missing"},
		{"malformed", `{"result":`, "invalid_json", "/result", "invalid_json"},
		{"nonobject envelope", `[]`, "type", "", "array"},
		{"duplicate envelope", `{"result":{},"result":{}}`, "additional_property", "", "object"},
		{"trailing", `{"result":{}} {}`, "trailing_data", "", "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := structuredResponseClaims(tc.input)
			failure, ok := errors.AsType[*StructuredResponseError](err)
			if !ok || !errors.Is(err, ErrStructuredResponseTool) {
				t.Fatalf("missing typed response rejection: %v", err)
			}
			want := StructuredResponseIssue{Code: tc.code, Pointer: tc.pointer, Expected: "object", Actual: tc.actual}
			if failure.Issue != want || failure.Arguments != tc.input || failure.Issue.Validate() != nil {
				t.Fatalf("rejection=%+v, want issue=%+v", failure, want)
			}
			if strings.Contains(err.Error(), "secret-claim") {
				t.Fatal("untrusted arguments leaked into the diagnostic")
			}
		})
	}
}
