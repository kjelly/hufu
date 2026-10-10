package utils

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// resetLearnedSecrets isolates each case: the learned set is process-wide by
// design, so a leak between tests would make them pass for the wrong reason.
func resetLearnedSecrets(t *testing.T) {
	t.Helper()
	learnedSecrets.Lock()
	learnedSecrets.values = nil
	learnedSecrets.order = nil
	learnedSecrets.Unlock()
	t.Cleanup(func() {
		learnedSecrets.Lock()
		learnedSecrets.values = nil
		learnedSecrets.order = nil
		learnedSecrets.Unlock()
	})
}

// TestLearnedSecretIsRedactedWithoutItsKey is the regression for the real leak:
// a worker reads a credential out of a vault file (key present, redacted fine),
// then greps the bare value out of the same file. Every later copy — tool
// output, LLM log, audit record, task note — carried it in plaintext because
// the value no longer had a key beside it.
func TestLearnedSecretIsRedactedWithoutItsKey(t *testing.T) {
	resetLearnedSecrets(t)

	withKey := RedactSecrets("ipa_admin_password: RealPassword2024!\n")
	if strings.Contains(withKey, "RealPassword2024!") {
		t.Fatalf("key/value form must already be redacted: %q", withKey)
	}

	for _, bare := range []string{
		"RealPassword2024!\n",
		"export ipa_admin_password=RealPassword2024!",
		"REPLACE_TEXT_AND_ENTER RealPassword2024!",
		`{"output":"RealPassword2024!\n"}`,
	} {
		if got := RedactSecrets(bare); strings.Contains(got, "RealPassword2024") {
			t.Errorf("bare occurrence still leaked: input %q -> %q", bare, got)
		}
	}
}

// TestLearnedSecretRedactsProgressiveEcho covers an interactive TUI echoing a
// typed secret one character at a time. Matching only the exact value would
// leave "RealPassword2024" — one character short of the real credential — in
// the log.
func TestLearnedSecretRedactsProgressiveEcho(t *testing.T) {
	resetLearnedSecrets(t)
	RedactSecrets("ipa_admin_password: RealPassword2024!")

	echo := "> RealPa\n> RealPassword\n> RealPassword20\n> RealPassword2024\n> RealPassword2024!\n"
	got := RedactSecrets(echo)
	// The complete value, and anything within two characters of it, is close
	// enough to be usable and must be gone.
	for _, usable := range []string{"RealPassword2024!", "RealPassword2024", "RealPassword202"} {
		if strings.Contains(got, usable) {
			t.Errorf("progressive echo still leaks a usable prefix %q: %q", usable, got)
		}
	}
	// Shorter prefixes are deliberately left alone: matching those needs a
	// short mandatory literal, which is what rewrites unrelated text.
	if !strings.Contains(got, "> RealPassword\n") {
		t.Errorf("a short prefix should survive so ordinary text is not rewritten: %q", got)
	}
}

// TestLearnedSecretDoesNotRedactAnOrdinaryWordPrefix is the regression for a
// false positive this feature caused on its first pass: a credential value of
// "protocol-secret" was matched by its first eight characters, so the word
// "protocol" was redacted out of unrelated identifiers — including a run ID in
// a log path. Redaction must never rewrite evidence that is not a credential.
func TestLearnedSecretDoesNotRedactAnOrdinaryWordPrefix(t *testing.T) {
	resetLearnedSecrets(t)
	RedactSecrets("api_token=protocol-secret")

	got := RedactSecrets("logs/task-output/1-run-wp13-protocol-attempt-1.jsonl reports failure_class=protocol")
	if !strings.Contains(got, "run-wp13-protocol-attempt-1") || !strings.Contains(got, "failure_class=protocol") {
		t.Errorf("an ordinary word that happens to prefix a secret must survive: %q", got)
	}
	if strings.Contains(RedactSecrets("value was protocol-secret"), "protocol-secret") {
		t.Error("the complete credential must still be redacted")
	}
}

func TestLearnSecretValueRejectsNonCredentials(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"too short", "abc123"},
		{"pure counter", "1048576"},
		{"unresolved shell reference", "${VAULT_PASSWORD}"},
		{"ansible template", "{{ vault_admin_password }}"},
		{"unfilled placeholder", "CHANGE-ME-please"},
		{"filesystem path", "/etc/application/vault.yml"},
		{"already redacted", redactedSecret},
		{"truncated redaction marker", "[REDACTED"},
		{"separated number", "2_000_000"},
		{"parent-relative path", "../reference/action-providers.md"},
		{"url", "https://example.com/reference.md"},
		{"file name", "team.yaml"},
		{"relative document path", "docs/reference/action-providers.md"},
		{"source path with line", "internal/team/runtime.go:42"},
		{"english word", "requires"},
		{"identifier", "requestTokens"},
		{"environment variable identifier", "SERVICE_API_KEY"},
		{"snake case identifier", "provider_api_key"},
		{"credential failure policy", "fail-closed"},
		{"credential warning policy", "fail-open"},
		{"credential admission policy", "deny-by-default"},
		{"credential permissive policy", "allow-by-default"},
		{"call expression", "filepath.ToSlash(token)"},
		{"unclosed call", "append(tokenSteps"},
		{"index expression", "token[lastSeparator+1:"},
		{"selector", "cfg.ProviderAPIKey"},
		{"concatenation operand", "+ secret +"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if isLearnableSecret(unquoteSecretValue(tc.value)) {
				t.Errorf("%q must not be learned as a credential", tc.value)
			}
		})
	}
}

func TestLearnedSecretsDoNotRewriteEnvironmentVariableReferences(t *testing.T) {
	resetLearnedSecrets(t)
	for _, source := range []string{
		"api_key: SERVICE_API_KEY", "credential=provider_api_key", "api_key: `SERVICE_API_KEY`",
	} {
		RedactSecrets(source)
	}
	const later = "SERVICE_API_KEY, `SERVICE_API_KEY`, and provider_api_key are documented configuration identifiers"
	if got := RedactSecrets(later); got != later {
		t.Fatalf("source identifiers changed after learning: %q", got)
	}
	// Symbolic values remain masked beside credential keys; this exception
	// concerns only process-wide bare-value learning from source text.
	if got := RedactSecrets("api_key: SERVICE_API_KEY"); strings.Contains(got, "SERVICE_API_KEY") {
		t.Fatalf("keyed credential value escaped redaction: %q", got)
	}
	RedactSecrets("api_key: ActualCredential123!")
	if got := RedactSecrets("value was ActualCredential123!"); strings.Contains(got, "ActualCredential123!") {
		t.Fatalf("actual learned credential escaped redaction: %q", got)
	}
	RedactSecrets("api_key: `MarkdownCredential123!`")
	if got := RedactSecrets("value was MarkdownCredential123!"); strings.Contains(got, "MarkdownCredential123!") {
		t.Fatalf("markdown-quoted credential escaped redaction: %q", got)
	}
}

func TestRegisteredSecretWithIdentifierShapeRemainsRedacted(t *testing.T) {
	const secret = "REGISTERED_IDENTIFIER_SHAPED_CREDENTIAL"
	RegisterSecretRedactor(testSecretRedactor{value: secret})
	if got := RedactSecrets("the actual value is " + secret); strings.Contains(got, secret) {
		t.Fatalf("registered exact credential escaped redaction: %q", got)
	}
	data, err := RedactJSON([]byte(`{"description":"REGISTERED_IDENTIFIER_SHAPED_CREDENTIAL"}`))
	if err != nil || strings.Contains(string(data), secret) {
		t.Fatalf("registered credential escaped JSON redaction: %s, error=%v", data, err)
	}
}

func TestCredentialPolicyModesDoNotBecomeBareSecrets(t *testing.T) {
	resetLearnedSecrets(t)
	for _, mode := range []string{"fail-closed", "fail-open", "deny-by-default", "allow-by-default"} {
		if got := RedactSecrets("Missing credential: " + mode); strings.Contains(got, mode) {
			t.Fatalf("keyed value escaped masking: %q", got)
		}
		const prefix = "The documented policy is "
		if got := RedactSecrets(prefix + mode); got != prefix+mode {
			t.Fatalf("public policy mode rewrote an unrelated record: %q", got)
		}
	}
	RedactSecrets("credential: ActualCredential123!")
	if got := RedactSecrets("value was ActualCredential123!"); strings.Contains(got, "ActualCredential123!") {
		t.Fatalf("actual bare credential escaped masking: %q", got)
	}
}

func TestRegisteredCredentialOverridesPublicPolicyMode(t *testing.T) {
	processRedactors.Lock()
	before := slices.Clone(processRedactors.items)
	processRedactors.Unlock()
	t.Cleanup(func() {
		processRedactors.Lock()
		processRedactors.items = before
		processRedactors.Unlock()
	})
	const secret = "fail-closed"
	RegisterSecretRedactor(testSecretRedactor{value: secret})
	if got := RedactSecrets("value was " + secret); strings.Contains(got, secret) {
		t.Fatalf("explicit registered credential escaped masking: %q", got)
	}
	got, err := RedactJSON([]byte(`{"output":"fail-closed"}`))
	if err != nil || strings.Contains(string(got), secret) {
		t.Fatalf("explicit registered credential escaped JSON masking: %s; %v", got, err)
	}
}

func TestLearnSecretValueAcceptsCredentialShapes(t *testing.T) {
	for _, value := range []string{
		"RealPassword2024!",
		"Sup3rSecretValue",
		"hunter2hunter2",
		"protocol-secret",
		"audit-secret-7f3c-9a1e",
		"abcdef1234567890",
		"sk-proj-abcdefghijklmnopqrstuvwxyz123456",
		// AWS secret access keys contain slashes but no file extension.
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		// A JWT is dotted like a selector, but its segments carry digits.
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
	} {
		if !isLearnableSecret(value) {
			t.Errorf("%q must stay learnable as a credential", value)
		}
	}
}

// TestLearnedSecretsIgnoreReviewedSourceText replays what a code review run
// fed the learner: source lines and model reasoning in which a value merely
// sits beside a credential-named key. None of those values may be redacted
// from later records, where they name a document, a contract field, or code.
func TestLearnedSecretsIgnoreReviewedSourceText(t *testing.T) {
	resetLearnedSecrets(t)
	for _, seen := range []string{
		"    5. secretKeyValueRe: requires key names ✓.",
		`		{name: "nested path", doc: "README.md", token: "docs/reference/action-providers.md"},`,
		`		{name: "bare yaml filename", doc: "README.md", token: "team.yaml"},`,
		"	token = filepath.ToSlash(token)",
		"		providerAPIKey:    providerConfig.ProviderAPIKey,",
		"			activeTokenStep = admission",
		"the secret: `install.go` handles it",
		"		MaxTokensWithoutProgress: 2_000_000,",
	} {
		RedactSecrets(seen)
	}
	later := "critic-review execution.requires-evidence: true reads docs/reference/action-providers.md, team.yaml, and `install.go`; " +
		"admission uses filepath.ToSlash(token) and providerConfig.ProviderAPIKey with a 2_000_000 budget"
	if got := RedactSecrets(later); got != later {
		t.Fatalf("source text learned as credentials rewrote a later record:\n got = %q\nwant = %q", got, later)
	}
}

// TestLearnedSecretsDoNotRewriteTelemetry pins the hazard that the existing
// numericTelemetryKeys exception exists for: "token" appears in counter names,
// and learning a counter value would silently rewrite unrelated numbers in
// every later log line.
func TestLearnedSecretsDoNotRewriteTelemetry(t *testing.T) {
	resetLearnedSecrets(t)
	RedactSecrets("tokens_since_progress: 1048576")
	if got := RedactSecrets("elapsed_ms: 1048576"); !strings.Contains(got, "1048576") {
		t.Errorf("a counter value must not become a redaction rule: %q", got)
	}
}

func TestLearnedSecretsAreBounded(t *testing.T) {
	resetLearnedSecrets(t)
	for i := 0; i < maxLearnedSecrets+50; i++ {
		learnSecretValue(fmt.Sprintf("bounded-secret-%04d", i))
	}
	learnedSecrets.RLock()
	defer learnedSecrets.RUnlock()
	if len(learnedSecrets.order) != maxLearnedSecrets || len(learnedSecrets.values) != len(learnedSecrets.order) {
		t.Fatalf("learned set unbounded or inconsistent: order=%d values=%d", len(learnedSecrets.order), len(learnedSecrets.values))
	}
}

// TestRedactSecretsIsIdempotent pins the property that value learning is most
// likely to break. Redacted text is re-redacted all over the codebase — status
// projections, failure events, and session entries all pass through more than
// one call — so a second pass must be a no-op.
func TestRedactSecretsIsIdempotent(t *testing.T) {
	resetLearnedSecrets(t)
	for _, input := range []string{
		"api_token=super-secret-value",
		"request failed\nnext: line\napi_token=super-secret-value",
		`{"grafana_admin_password":"Sup3rSecretValue"}`,
		"export ADMIN_PASSWORD=hunter2hunter2",
		"Authorization: Bearer abcdef1234567890",
	} {
		once := RedactSecrets(input)
		twice := RedactSecrets(once)
		if once != twice {
			t.Errorf("redaction is not idempotent for %q:\n once = %q\ntwice = %q", input, once, twice)
		}
	}
}

// TestRedactJSONLearnsKeyedValues covers the session-persistence path, which
// redacts by JSON key rather than by text pattern.
func TestRedactJSONLearnsKeyedValues(t *testing.T) {
	resetLearnedSecrets(t)
	if _, err := RedactJSON([]byte(`{"grafana_admin_password":"Sup3rSecretValue"}`)); err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	if got := RedactSecrets("the deploy used Sup3rSecretValue as the argument"); strings.Contains(got, "Sup3rSecretValue") {
		t.Errorf("a value redacted by JSON key must stay redacted as text: %q", got)
	}
}
