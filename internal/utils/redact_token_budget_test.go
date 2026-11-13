package utils

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactJSONPreservesScopedTokenBudgets(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		public     bool
	}{
		{"max_review_input_tokens", "12000", true},
		{"max_analysis_input_tokens", "0", true},
		{"max_report_output_tokens", "2147483647", true},
		{"max_report_output_tokens", "2147483648", false},
		{"max_report_output_tokens", "-1", false},
		{"max_report_output_tokens", "1.5", false},
		{"max_report_output_tokens", "1e3", false},
		{"max_report_output_tokens", `"12000"`, false},
		{"max_report_output_tokens", `true`, false},
		{"max_report_output_tokens", `{"value":12000}`, false},
		{"max_api_input_tokens", "12000", false},
		{"max_password_input_tokens", "12000", false},
		{"max_auth_output_tokens", "12000", false},
		{"max_bearer_input_tokens", "12000", false},
		{"max_credential_input_tokens", "12000", false},
		{"max_token_input_tokens", "12000", false},
		{"max_private_key_input_tokens", "12000", false},
		{"max_report_input_tokens", `null`, false},
		{"max_report_input_tokens", `[]`, false},
		{"max_access_output_tokens", "12000", false},
		{"max_refresh_output_tokens", "12000", false},
		{"max_output_tokens", "12000", false},
		{"api_token", "12000", false},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			input := []byte(`{"` + tc.key + `":` + tc.value + `}`)
			got, err := RedactJSON(input)
			if err != nil {
				t.Fatal(err)
			}
			var data map[string]json.RawMessage
			if err = json.Unmarshal(got, &data); err != nil {
				t.Fatal(err)
			}
			expected := `"[REDACTED]"`
			if tc.public {
				expected = tc.value
			}
			if string(data[tc.key]) != expected {
				t.Fatalf("redaction=%s want %s", got, expected)
			}
			if tc.public && IsRedactedJSONKey(tc.key) {
				t.Fatal("public numeric budget classified as always sensitive")
			}
			// Action payloads are themselves JSON strings inside durable envelopes.
			wrapped, err := json.Marshal(map[string]string{"payload": string(input)})
			if err != nil {
				t.Fatal(err)
			}
			wrapped, err = RedactJSON(wrapped)
			if err != nil {
				t.Fatal(err)
			}
			var envelope map[string]string
			if err = json.Unmarshal(wrapped, &envelope); err != nil {
				t.Fatal(err)
			}
			if tc.public && envelope["payload"] != string(input) {
				t.Fatalf("embedded payload changed: %s", wrapped)
			}
			if !tc.public && !strings.Contains(envelope["payload"], "[REDACTED]") {
				t.Fatalf("embedded credential escaped redaction: %s", wrapped)
			}
		})
	}
}
