package utils

import (
	"strings"
	"testing"
)

// TestRedactSecretsURLUserinfoPositive covers the new URL userinfo pass:
// every input embeds a password inside an absolute URL with a scheme, and
// the password must be replaced with [REDACTED] while the username,
// scheme, host, and path are preserved.
func TestRedactSecretsURLUserinfoPositive(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "https user pass host path",
			content: "https://user:FAKEpass123@example.invalid/repo.git",
			want:    "https://user:[REDACTED]@example.invalid/repo.git",
		},
		{
			name:    "git clone prefix",
			content: "git clone https://user:FAKEpass123@example.invalid/repo.git",
			want:    "git clone https://user:[REDACTED]@example.invalid/repo.git",
		},
		{
			name:    "api_user trailing slash",
			content: "https://api_user:abcd1234efgh@example.invalid/",
			want:    "https://api_user:[REDACTED]@example.invalid/",
		},
		{
			name:    "deep path preserved",
			content: "https://svc:p4ssw0rd!@host.example.invalid/very/long/path?q=1&r=2#frag",
			want:    "https://svc:[REDACTED]@host.example.invalid/very/long/path?q=1&r=2#frag",
		},
		{
			name:    "basic auth",
			content: "https://alice:s3cret-p4ss@EXAMPLE.invalid/",
			want:    "https://alice:[REDACTED]@EXAMPLE.invalid/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLearnedSecrets(t)
			got := RedactSecrets(tc.content)
			if got != tc.want {
				t.Fatalf("RedactSecrets(%q) = %q, want %q", tc.content, got, tc.want)
			}
			if again := RedactSecrets(got); again != got {
				t.Fatalf("RedactSecrets is not idempotent for %q: once=%q twice=%q", tc.content, got, again)
			}
		})
	}
}

// TestRedactSecretsURLUserinfoNegative pins the false-positive floor for
// the URL pass. None of these inputs carries a password, so the URL is
// returned unchanged. The negative cases include inputs that DO contain
// `://` and `@` but not as scheme/authority delimiters (path-only or
// query-only `@`) — those must still pass through untouched.
func TestRedactSecretsURLUserinfoNegative(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "user only https", content: "https://user@example.invalid"},
		{name: "user only ssh", content: "ssh://git@example.invalid/x.git"},
		{name: "no userinfo", content: "https://example.com/repo.git"},
		{name: "at in path", content: "https://example.com/@user"},
		{name: "at in query", content: "https://example.com/?email=user@example.com"},
		{name: "authorization basic still redacts", content: "Authorization: Basic dXNlcjpwYXNz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLearnedSecrets(t)
			got := RedactSecrets(tc.content)
			// The Authorization line is the one case where the URL pass must
			// not interfere — the Authorization pass still rewrites it.
			want := tc.content
			if tc.name == "authorization basic still redacts" {
				want = "Authorization: Basic [REDACTED]"
			}
			if got != want {
				t.Fatalf("RedactSecrets(%q) = %q, want %q", tc.content, got, want)
			}
		})
	}
}

// TestRedactSecretsURLUserinfoIdempotent pins idempotency for every
// positive case individually: RedactSecrets(RedactSecrets(x)) must equal
// RedactSecrets(x) so redaction can be safely re-applied on the way to
// every persistence destination.
func TestRedactSecretsURLUserinfoIdempotent(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "user pass repo", in: "https://user:FAKEpass123@example.invalid/repo.git"},
		{name: "git clone", in: "git clone https://user:FAKEpass123@example.invalid/repo.git"},
		{name: "api_user", in: "https://api_user:abcd1234efgh@example.invalid/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLearnedSecrets(t)
			once := RedactSecrets(tc.in)
			twice := RedactSecrets(once)
			if once != twice {
				t.Fatalf("idempotency violated: in=%q once=%q twice=%q", tc.in, once, twice)
			}
			if strings.Contains(once, "FAKEpass123") || strings.Contains(once, "abcd1234efgh") {
				t.Fatalf("password still leaked: in=%q once=%q", tc.in, once)
			}
		})
	}
}

// TestRedactSecretsAuthorizationTrailingQuote covers the Authorization
// trailing-quote fix. The value group in secretAuthorizationRe is greedy
// and historically swallowed a closing `'` or `"`, leaving the surrounding
// shell line syntactically broken. The substitution now preserves one
// trailing quote.
func TestRedactSecretsAuthorizationTrailingQuote(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "single quote preserved",
			content: "curl -H 'Authorization: Bearer FAKEtoken' x",
			want:    "curl -H 'Authorization: Bearer [REDACTED]' x",
		},
		{
			name:    "double quote preserved",
			content: `curl -H "Authorization: Bearer FAKEtoken" x`,
			want:    `curl -H "Authorization: Bearer [REDACTED]" x`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLearnedSecrets(t)
			got := RedactSecrets(tc.content)
			if got != tc.want {
				t.Fatalf("RedactSecrets(%q) = %q, want %q", tc.content, got, tc.want)
			}
			if again := RedactSecrets(got); again != got {
				t.Fatalf("RedactSecrets is not idempotent for %q: once=%q twice=%q", tc.content, got, again)
			}
		})
	}
}

// TestRedactSecretsAuthorizationUnchanged pins that the Authorization
// rewrite keeps doing exactly what it did before for inputs without
// trailing quotes. The fix must not regress quote-less headers or basic
// auth.
func TestRedactSecretsAuthorizationUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{name: "bearer no quotes", content: "Authorization: Bearer FAKEtoken", want: "Authorization: Bearer [REDACTED]"},
		{name: "basic no quotes", content: "Authorization: Basic dXNlcjpwYXNz", want: "Authorization: Basic [REDACTED]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLearnedSecrets(t)
			got := RedactSecrets(tc.content)
			if got != tc.want {
				t.Fatalf("RedactSecrets(%q) = %q, want %q", tc.content, got, tc.want)
			}
			if again := RedactSecrets(got); again != got {
				t.Fatalf("RedactSecrets is not idempotent for %q: once=%q twice=%q", tc.content, got, again)
			}
		})
	}
}

// TestRedactSecretsAuthorizationIdempotent pins idempotency for every
// Authorization case individually so a regression in either the
// quote-trim path or the bare-value path surfaces.
func TestRedactSecretsAuthorizationIdempotent(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "single quote", in: "curl -H 'Authorization: Bearer FAKEtoken' x"},
		{name: "double quote", in: `curl -H "Authorization: Bearer FAKEtoken" x`},
		{name: "bearer no quote", in: "Authorization: Bearer FAKEtoken"},
		{name: "basic no quote", in: "Authorization: Basic dXNlcjpwYXNz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLearnedSecrets(t)
			once := RedactSecrets(tc.in)
			twice := RedactSecrets(once)
			if once != twice {
				t.Fatalf("idempotency violated: in=%q once=%q twice=%q", tc.in, once, twice)
			}
		})
	}
}

// TestRedactSecretsLearnsURLPassword covers the learning side of the URL
// pass. The first call must redact the URL and learn the bare password; a
// second call on a different line that prints the bare password alone
// must therefore also be redacted. The outer t.Run uses
// resetLearnedSecrets exactly once; the inner calls share the learned set
// so the second pass relies on the value captured during the URL pass.
func TestRedactSecretsLearnsURLPassword(t *testing.T) {
	resetLearnedSecrets(t)

	first := RedactSecrets("https://user:FAKEpass123@example.invalid/")
	if strings.Contains(first, "FAKEpass123") {
		t.Fatalf("first call did not redact URL password: %q", first)
	}

	bare := "subsequent log line: FAKEpass123"
	got := RedactSecrets(bare)
	if strings.Contains(got, "FAKEpass123") {
		t.Fatalf("bare password leak after URL pass learned it: in=%q got=%q", bare, got)
	}
	if !strings.Contains(got, redactedSecret) {
		t.Fatalf("bare password leak: in=%q got=%q, expected [REDACTED]", bare, got)
	}
}
