package utils

import (
	"strings"
	"testing"
)

// fakePrefixCredentials returns obviously-fake values matching each of the
// four prefix-only credential formats. They are constructed so the body
// lengths sit on the floor of their format's regex, so a regression in the
// regex's length handling surfaces immediately.
func fakePrefixCredentials(t *testing.T) (string, string, string, string, string) {
	t.Helper()
	return "sk-" + strings.Repeat("a", 48),
		"sk-proj-" + strings.Repeat("a", 19) + "b",
		"ghp_" + strings.Repeat("a", 36),
		"AKIA" + strings.Repeat("A", 16),
		"xoxb-1234567890-1234-AB"
}

// TestRedactSecretsPrefixOnlyCredentialsPositive covers each of the four
// prefix-only credential formats in three realistic shapes: bare in a
// log-style line, inside a JSON value, and bare in command output. None of
// these inputs carries a credential-named key, so the key/value patterns
// cannot reach them — the new pass must.
func TestRedactSecretsPrefixOnlyCredentialsPositive(t *testing.T) {
	resetLearnedSecrets(t)
	fakeOpenAI, fakeOpenAIProj, fakeGitHubPAT, fakeAWSAKIA, fakeSlack := fakePrefixCredentials(t)

	cases := []struct {
		name    string
		content string
		secret  string
	}{
		{name: "openai_sk_log_line", content: "client=openai key=" + fakeOpenAI, secret: fakeOpenAI},
		{name: "openai_sk_json_value", content: `{"data":"` + fakeOpenAI + `"}`, secret: fakeOpenAI},
		{name: "openai_sk_command_output", content: "echo " + fakeOpenAI, secret: fakeOpenAI},

		{name: "openai_proj_log_line", content: "client=openai key=" + fakeOpenAIProj, secret: fakeOpenAIProj},
		{name: "openai_proj_json_value", content: `{"data":"` + fakeOpenAIProj + `"}`, secret: fakeOpenAIProj},
		{name: "openai_proj_command_output", content: "echo " + fakeOpenAIProj, secret: fakeOpenAIProj},

		{name: "github_pat_log_line", content: "client=github key=" + fakeGitHubPAT, secret: fakeGitHubPAT},
		{name: "github_pat_json_value", content: `{"data":"` + fakeGitHubPAT + `"}`, secret: fakeGitHubPAT},
		{name: "github_pat_command_output", content: "echo " + fakeGitHubPAT, secret: fakeGitHubPAT},

		{name: "aws_akia_log_line", content: "client=aws key=" + fakeAWSAKIA, secret: fakeAWSAKIA},
		{name: "aws_akia_json_value", content: `{"data":"` + fakeAWSAKIA + `"}`, secret: fakeAWSAKIA},
		{name: "aws_akia_command_output", content: "echo " + fakeAWSAKIA, secret: fakeAWSAKIA},

		{name: "slack_token_log_line", content: "client=slack token=" + fakeSlack, secret: fakeSlack},
		{name: "slack_token_json_value", content: `{"data":"` + fakeSlack + `"}`, secret: fakeSlack},
		{name: "slack_token_command_output", content: "echo " + fakeSlack, secret: fakeSlack},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.content)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("RedactSecrets(%q) = %q, leaked credential %q", tc.content, got, tc.secret)
			}
			if !strings.Contains(got, redactedSecret) {
				t.Fatalf("RedactSecrets(%q) = %q, expected redaction marker %q", tc.content, got, redactedSecret)
			}
		})
	}
}

// TestRedactSecretsPrefixOnlyCredentialsNegative pins the false-positive
// floor. Every value here is shaped close enough to look interesting while
// not actually being one of the four formats.
func TestRedactSecretsPrefixOnlyCredentialsNegative(t *testing.T) {
	resetLearnedSecrets(t)

	cases := []struct {
		name    string
		content string
	}{
		{name: "task-1", content: "task-1 finished"},
		{name: "risk-free", content: "this is risk-free"},
		{name: "skip-ci", content: "please skip-ci on this"},
		{name: "sk-learn", content: "import sk-learn as skl"},
		{name: "sk-8-chars", content: "sk-abcdefgh"},
		{name: "sk-49-chars", content: "sk-" + strings.Repeat("a", 49)},
		{name: "ghp-35-chars", content: "ghp_" + strings.Repeat("a", 35)},
		{name: "akia-15-chars", content: "AKIA" + strings.Repeat("A", 15)},
		{name: "xoxb-short-body", content: "xoxb-short"},
		{name: "xoxb-15-body", content: "xoxb-12345678-12345"},
		{name: "go-identifier", content: "MyVar123"},
		{name: "git-sha", content: "commit a1b2c3d4e5f6789012345678901234567890abcd landed"},
		{name: "uuid", content: "user 550e8400-e29b-41d4-a716-446655440000 logged in"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSecrets(tc.content)
			if got != tc.content {
				t.Fatalf("RedactSecrets(%q) = %q, expected unchanged", tc.content, got)
			}
		})
	}
}

// TestRedactSecretsPrefixOnlyIdempotent pins idempotency: re-running
// RedactSecrets on already-redacted text must yield the same text.
func TestRedactSecretsPrefixOnlyIdempotent(t *testing.T) {
	resetLearnedSecrets(t)
	fakeOpenAI, fakeOpenAIProj, fakeGitHubPAT, fakeAWSAKIA, fakeSlack := fakePrefixCredentials(t)

	cases := []struct {
		name string
		in   string
	}{
		{name: "openai_sk", in: "key=" + fakeOpenAI},
		{name: "openai_proj", in: "key=" + fakeOpenAIProj},
		{name: "github_pat", in: "key=" + fakeGitHubPAT},
		{name: "aws_akia", in: "key=" + fakeAWSAKIA},
		{name: "slack_token", in: "key=" + fakeSlack},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			once := RedactSecrets(tc.in)
			twice := RedactSecrets(once)
			if once != twice {
				t.Fatalf("idempotency violated: once=%q twice=%q", once, twice)
			}
			if strings.Contains(once, tc.in) {
				t.Fatalf("RedactSecrets did not redact: in=%q once=%q", tc.in, once)
			}
		})
	}
}

// TestRedactSecretsPrefixOnlyMarkerPassThrough pins that the literal marker
// [REDACTED] never matches any of the new prefix patterns, so already-redacted
// text survives a redaction pass unchanged.
func TestRedactSecretsPrefixOnlyMarkerPassThrough(t *testing.T) {
	resetLearnedSecrets(t)
	cases := []string{
		"[REDACTED]",
		"[REDACTED] more text [REDACTED]",
		`{"key":"[REDACTED]","other":"value"}`,
		"echo [REDACTED]",
	}
	for _, content := range cases {
		t.Run(content, func(t *testing.T) {
			got := RedactSecrets(content)
			if got != content {
				t.Fatalf("RedactSecrets(%q) = %q, expected unchanged", content, got)
			}
			twice := RedactSecrets(got)
			if twice != got {
				t.Fatalf("idempotency violated: %q -> %q -> %q", content, got, twice)
			}
		})
	}
}

// TestRedactSecretsPrefixOnlySkDoesNotConsumeSkProj guards the boundary
// between the two OpenAI patterns. A body starting with `proj-` is too short
// to satisfy the 48-char `sk-` pattern (the `-` after `proj` breaks the run),
// so the dedicated `sk-proj-` pattern owns it instead.
func TestRedactSecretsPrefixOnlySkDoesNotConsumeSkProj(t *testing.T) {
	resetLearnedSecrets(t)
	// Body length 4 (`proj`) is below `sk-`'s 48-char floor, so the `sk-`
	// pattern must not match. The `sk-proj-` pattern also fails because the
	// body is below its 20-char floor, so the input passes through
	// unchanged.
	in := "sk-proj-aaaa extra text"
	if got := RedactSecrets(in); got != in {
		t.Fatalf("RedactSecrets(%q) = %q, expected unchanged", in, got)
	}
	// 20 `a` chars after `sk-proj-`: still below `sk-`'s 48-char floor
	// (because the `-` after `proj` breaks the alnum run), but above
	// `sk-proj-`'s floor. The whole `sk-proj-` body must be redacted.
	skProjShort := "sk-proj-" + strings.Repeat("a", 20)
	wantRedacted := "[REDACTED] extra text"
	if got := RedactSecrets(skProjShort + " extra text"); got != wantRedacted {
		t.Fatalf("RedactSecrets(%q) = %q, expected %q", skProjShort+" extra text", got, wantRedacted)
	}
	// 48 `a` chars after `sk-`: above `sk-`'s floor, body ends at a word
	// boundary, so the `sk-` pattern owns it. The trailing `proj-` block is
	// not consumed by `sk-` because the body alnum run is too short.
	skShort := "sk-" + strings.Repeat("a", 48) + " more"
	if !strings.Contains(RedactSecrets(skShort), redactedSecret) {
		t.Fatalf("RedactSecrets(%q) did not redact", skShort)
	}
}
