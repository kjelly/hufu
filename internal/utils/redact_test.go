package utils

import (
	"strings"
	"testing"
)

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
