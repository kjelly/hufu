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
