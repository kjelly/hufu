package utils

import (
	"encoding/json"
	"strings"
	"testing"
)

type testSecretRedactor struct{ value string }

func (r testSecretRedactor) RedactText(value string) string {
	return strings.ReplaceAll(value, r.value, "[REGISTRY_REDACTED]")
}

func (r testSecretRedactor) RedactJSON(data []byte) ([]byte, error) { return data, nil }

func TestRedactSecretsUsesProcessRedactor(t *testing.T) {
	const exact = "registry-only-secret-7f8e"
	RegisterSecretRedactor(testSecretRedactor{value: exact})
	got := RedactSecrets("diagnostic=" + exact)
	if strings.Contains(got, exact) || !strings.Contains(got, "[REGISTRY_REDACTED]") {
		t.Fatalf("process redactor did not remove exact secret: %q", got)
	}
}

func TestRedactSecrets(t *testing.T) {
	input := "ipa_admin_password: \"keep-me-secret\"\n" +
		"export API_TOKEN=token-value\n" +
		`{"restic_aws_secret_access_key":"json-secret"}` + "\n" +
		"Authorization: Bearer bearer-secret\n" +
		"-----BEGIN RSA PRIVATE KEY-----\nprivate-key-material\n-----END RSA PRIVATE KEY-----\n" +
		"plain text remains"

	got := RedactSecrets(input)
	for _, secret := range []string{"keep-me-secret", "token-value", "json-secret", "bearer-secret", "private-key-material"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted output still contains %q: %s", secret, got)
		}
	}
	for _, want := range []string{"ipa_admin_password: \"[REDACTED]\"", "API_TOKEN=[REDACTED]", `"restic_aws_secret_access_key":"[REDACTED]"`, "plain text remains"} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted output missing %q: %s", want, got)
		}
	}
}

func TestRedactJSONPreservesEscapedContent(t *testing.T) {
	input := []byte(`{"entries":[{"content":"ipa_admin_password: \\\"ExampleSecret\\\" and api_token: hidden"}],"password":"top-secret"}`)
	got, err := RedactJSON(input)
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	if !json.Valid(got) {
		t.Fatalf("redacted output is invalid JSON: %s", got)
	}
	if strings.Contains(string(got), "ExampleSecret") || strings.Contains(string(got), "hidden") || strings.Contains(string(got), "top-secret") {
		t.Fatalf("secret leaked: %s", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal redacted output: %v", err)
	}
}

func TestRedactJSONPreservesNumericTelemetryWithSecretLikeKey(t *testing.T) {
	input := []byte(`{"tokens_used":1234,"max_tokens_without_progress":2000000,"tokens_since_criterion_progress":42,"tokens_since_progress":"credential-like","nested":{"max_tokens_without_progress":true},"api_token":"secret"}`)
	got, err := RedactJSON(input)
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal redacted output: %v", err)
	}
	if _, ok := decoded["tokens_used"].(float64); !ok {
		t.Fatalf("tokens_used changed type: %#v", decoded["tokens_used"])
	}
	if decoded["tokens_used"] != float64(1234) {
		t.Fatalf("tokens_used changed value: %#v", decoded["tokens_used"])
	}
	if _, ok := decoded["max_tokens_without_progress"].(float64); !ok {
		t.Fatalf("numeric telemetry changed type: %#v", decoded["max_tokens_without_progress"])
	}
	if decoded["max_tokens_without_progress"] != float64(2000000) || decoded["tokens_since_criterion_progress"] != float64(42) {
		t.Fatalf("numeric telemetry changed value: %#v", decoded)
	}
	if decoded["tokens_since_progress"] != redactedSecret {
		t.Fatalf("string under telemetry key was not redacted: %#v", decoded["tokens_since_progress"])
	}
	nested, ok := decoded["nested"].(map[string]any)
	if !ok || nested["max_tokens_without_progress"] != redactedSecret {
		t.Fatalf("non-numeric telemetry value was not redacted: %#v", decoded["nested"])
	}
	if decoded["api_token"] != redactedSecret {
		t.Fatalf("credential was not redacted: %#v", decoded["api_token"])
	}
}

func TestRedactJSONRedactsNumericAndBooleanCredentials(t *testing.T) {
	input := []byte(`{"nested":{"password":123456,"token":9876,"api_key":true,"api_secret":false}}`)
	got, err := RedactJSON(input)
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal redacted output: %v", err)
	}
	nested, ok := decoded["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested value = %#v, want object", decoded["nested"])
	}
	for _, key := range []string{"password", "token", "api_key", "api_secret"} {
		if nested[key] != redactedSecret {
			t.Errorf("%s = %#v, want %q", key, nested[key], redactedSecret)
		}
	}
	for _, leaked := range []string{"123456", "9876", "true", "false"} {
		if strings.Contains(string(got), leaked) {
			t.Errorf("credential value %q leaked in %s", leaked, got)
		}
	}
}

func TestRedactionPreservesSafeSecretHandlingMetadata(t *testing.T) {
	resetLearnedSecrets(t)
	input := `{"secret_handling":"value_env_required","api_secret":"real-secret-value"}`

	text := RedactSecrets(input)
	if !strings.Contains(text, `"secret_handling":"value_env_required"`) {
		t.Fatalf("text redaction removed safe policy metadata: %s", text)
	}
	if strings.Contains(text, "real-secret-value") {
		t.Fatalf("text redaction leaked credential: %s", text)
	}

	data, err := RedactJSON([]byte(input))
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal redacted JSON: %v", err)
	}
	if decoded["secret_handling"] != "value_env_required" {
		t.Fatalf("secret_handling = %#v, want safe policy metadata", decoded["secret_handling"])
	}
	if decoded["api_secret"] != redactedSecret {
		t.Fatalf("api_secret = %#v, want redacted", decoded["api_secret"])
	}
}

func TestRedactionDoesNotTrustUnknownSecretHandlingValue(t *testing.T) {
	resetLearnedSecrets(t)
	const credential = "actual-credential-value"
	for _, got := range []string{
		RedactSecrets(`{"secret_handling":"` + credential + `"}`),
		string(mustRedactJSON(t, []byte(`{"secret_handling":"`+credential+`"}`))),
	} {
		if strings.Contains(got, credential) || !strings.Contains(got, redactedSecret) {
			t.Fatalf("unknown policy value was not redacted: %s", got)
		}
	}
}

func mustRedactJSON(t *testing.T, data []byte) []byte {
	t.Helper()
	got, err := RedactJSON(data)
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	return got
}
