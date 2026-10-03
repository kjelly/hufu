package utils

import (
	"strings"
	"testing"
)

func TestContainsSecretKeyNameMatchesRegex(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "empty", content: ""},
		{name: "plain prose", content: "review the latest commit for regressions"},
		{name: "lowercase token", content: "max token budget"},
		{name: "uppercase env", content: "export GITHUB_TOKEN=abc"},
		{name: "mixed case", content: "PassWord: hunter2"},
		{name: "passwd", content: "/etc/passwd"},
		{name: "credential", content: "Credentials rotated"},
		{name: "api key underscore", content: "API_KEY=1"},
		{name: "api key hyphen", content: "x-api-key: 1"},
		{name: "api key joined", content: "apikey"},
		{name: "api key spaced", content: "api key"},
		{name: "access key", content: "Access-Key"},
		{name: "private key", content: "private_key"},
		{name: "private key spaced", content: "BEGIN PRIVATE KEY"},
		{name: "kelvin sign", content: "toKen=1"},
		{name: "long s", content: "ſecret: 1"},
		{name: "long s without keyword", content: "ſome text"},
		{name: "non-ascii prose", content: "檢查硬碟效能"},
		{name: "non-ascii with keyword", content: "秘密 secret 值"},
		{name: "dotless i", content: "credentıal"},
		{name: "dotted capital i", content: "credentİal"},
		{name: "near miss", content: "tok en sec ret pass word"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := secretKeyNameRe.MatchString(tc.content)
			if got := containsSecretKeyName(tc.content); got != want {
				t.Fatalf("containsSecretKeyName(%q) = %v, regex = %v", tc.content, got, want)
			}
		})
	}
}

func TestRedactSecretsPrefilterKeepsRedaction(t *testing.T) {
	cases := []struct {
		name    string
		content string
		secret  string
	}{
		{name: "key value", content: "api_token=abcdef123456", secret: "abcdef123456"},
		{name: "json key", content: `{"client_secret":"s3cr3t-value"}`, secret: "s3cr3t-value"},
		{name: "env uppercase", content: "export DB_PASSWORD=Sup3rS3cret", secret: "Sup3rS3cret"},
		{name: "authorization header", content: "Authorization: Bearer abc.def.ghi", secret: "abc.def.ghi"},
		{name: "private key block", content: "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----", secret: "MIIE"},
		{name: "kelvin sign key", content: "toKen=kelvin-secret-value", secret: "kelvin-secret-value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLearnedSecrets(t)
			got := RedactSecrets(tc.content)
			if strings.Contains(got, tc.secret) || !strings.Contains(got, redactedSecret) {
				t.Fatalf("RedactSecrets(%q) = %q, want %q redacted", tc.content, got, tc.secret)
			}
		})
	}
}

func TestRedactSecretsLeavesTextWithoutKeyNamesUnchanged(t *testing.T) {
	resetLearnedSecrets(t)
	const content = "internal/team/coordinator_session.go:759 saveCheckpoint rewrites session.json"
	if got := RedactSecrets(content); got != content {
		t.Fatalf("RedactSecrets changed text without credential key names: %q", got)
	}
}

// TestRedactSecretsKeepsSourceCodeLocationReferences is the regression for
// treating `secrets.go:123`, `token_store.go:45`, or
// `internal/teampkg/secrets.go:123:45` as a credential. The key word in the
// filename is a credential name and the value is just a line number, but the
// pair is a stack trace / diagnostic reference, not a key/value credential,
// and rewriting it loses the only evidence a failed run records.
func TestRedactSecretsKeepsSourceCodeLocationReferences(t *testing.T) {
	resetLearnedSecrets(t)
	cases := []string{
		"secrets.go:123",
		"token_store.go:45",
		"internal/teampkg/secrets.go:123:45",
		"see internal/team/secrets.go:99 for the leak",
		"trace ends at credential_store.go:7:42",
	}
	for _, content := range cases {
		got := RedactSecrets(content)
		if got != content {
			t.Errorf("RedactSecrets rewrote source-code location reference %q -> %q", content, got)
		}
		if twice := RedactSecrets(got); twice != got {
			t.Errorf("redaction of %q was not idempotent: got %q then %q", content, got, twice)
		}
	}
}

// TestRedactSecretsStillRedactsRealCredentials pins the other half of the
// regression: even though file references with credential words must survive,
// a real key/value pair under the same kind of key still has to be redacted.
func TestRedactSecretsStillRedactsRealCredentials(t *testing.T) {
	resetLearnedSecrets(t)
	cases := []struct {
		content string
		secret  string
	}{
		{content: "api_token=abcd1234efgh", secret: "abcd1234efgh"},
		{content: "password: hunter2", secret: "hunter2"},
		{content: "DB_PASSWORD=xyz", secret: "xyz"},
	}
	for _, tc := range cases {
		got := RedactSecrets(tc.content)
		if strings.Contains(got, tc.secret) || !strings.Contains(got, redactedSecret) {
			t.Fatalf("RedactSecrets(%q) = %q, want %q redacted", tc.content, got, tc.secret)
		}
		if twice := RedactSecrets(got); twice != got {
			t.Fatalf("redaction of %q was not idempotent: got %q then %q", tc.content, got, twice)
		}
	}
}
