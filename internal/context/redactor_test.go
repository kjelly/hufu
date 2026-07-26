package context

import (
	"strings"
	"testing"
)

func TestRedactSecretsCoversRequiredClasses(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string
	}{
		{"authorization_bearer", "Authorization: Bearer sk-abc123XYZ", "sk-abc123XYZ"},
		{"authorization_basic", "Authorization: Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"},
		{"api_key_assignment", `api_key = "sk-live-1234567890abcdef"`, "sk-live-1234567890abcdef"},
		{"x_api_key_header", "X-API-Key: sk-99887766", "sk-99887766"},
		{"api_key_header", "Api-Key: sk-1122334455", "sk-1122334455"},
		{"password_assignment", "password=SuperSecret!123", "SuperSecret!123"},
		{"password_header", "password: hunter2hunter2", "hunter2hunter2"},
		{"cookie_header", "Cookie: session=zzz999yyy888", "zzz999yyy888"},
		{"set_cookie_header", "Set-Cookie: session=zzz999yyy888; Path=/", "zzz999yyy888"},
		{"session_id_assignment", "Cookie: session_id=abcdef1234567890", "abcdef1234567890"},
		{"token_assignment", "token=eyJhbGciOiJIUzI1NiJ9", "eyJhbGciOiJIUzI1NiJ9"},
		{"secret_assignment", "secret=topsecretvalue", "topsecretvalue"},
		{"env_style_secret", "DATABASE_PASSWORD=hunter2hunter2", "hunter2hunter2"},
		{"private_key_block", "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----", "MIIEpAIBAAKCAQEA"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := RedactSecrets(c.in)
			if strings.Contains(out, c.secret) {
				t.Fatalf("secret leaked through RedactSecrets: in=%q out=%q", c.in, out)
			}
		})
	}
}
