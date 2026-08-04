package utils

import (
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
		{"filesystem path", "/etc/pilot/vault.yml"},
		{"already redacted", redactedSecret},
		{"truncated redaction marker", "[REDACTED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if isLearnableSecret(unquoteSecretValue(tc.value)) {
				t.Errorf("%q must not be learned as a credential", tc.value)
			}
		})
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
		learnSecretValue(strings.Repeat("a", 10) + string(rune('A'+i%26)) + strings.Repeat("z", i%7+1))
	}
	learnedSecrets.RLock()
	defer learnedSecrets.RUnlock()
	if len(learnedSecrets.order) > maxLearnedSecrets || len(learnedSecrets.values) != len(learnedSecrets.order) {
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
