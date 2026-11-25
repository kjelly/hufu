package utils

import (
	"strings"
	"testing"
)

func TestRedactSubprocessOutputRedactsSensitiveEnvironmentValues(t *testing.T) {
	secret := "env-boundary-secret-7f9c"
	env := []string{
		"PATH=/bin",
		"PILOT_TEST_PASSWORD=" + secret,
		"PILOT_TEST_MODE=staging",
		"PILOT_TEST_AWS_SECRET_ACCESS_KEY=another-secret-42",
	}

	got := RedactSubprocessOutput(
		"PILOT_TEST_PASSWORD="+secret+"\n"+secret+"\nPILOT_TEST_MODE=staging\n",
		env,
	)
	for _, leaked := range []string{secret, "another-secret-42"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("subprocess secret leaked in output: %q", got)
		}
	}
	if !strings.Contains(got, "PILOT_TEST_MODE=staging") {
		t.Fatalf("ordinary environment output was unexpectedly changed: %q", got)
	}
}

// TestRedactSubprocessOutputCoversSSHPASSAndPreservesPWD asserts the env-name
// vocabulary covers the sshpass tool's SSHPASS variable, that PWD remains
// visible (it is the shell's current-directory marker, not a secret), and
// that a very short sensitive value is still redacted (no length floor).
func TestRedactSubprocessOutputCoversSSHPASSAndPreservesPWD(t *testing.T) {
	sshpass := "s3cr3t-pass"
	shortOTP := "987654" // 6 chars: must not be exempted by a length heuristic
	home := "/home/ubuntu"
	env := []string{
		"SSHPASS=" + sshpass,
		"PWD=" + home,
		"DB_PASSWORD=" + shortOTP,
		"PATH=/bin",
	}

	got := RedactSubprocessOutput(
		"SSHPASS="+sshpass+"\n"+"cd "+home+"\n"+"otp="+shortOTP+"\n",
		env,
	)
	if strings.Contains(got, sshpass) {
		t.Fatalf("SSHPASS value leaked: %q", got)
	}
	if strings.Contains(got, shortOTP) {
		t.Fatalf("short sensitive value leaked (length floor should not exempt it): %q", got)
	}
	if !strings.Contains(got, home) {
		t.Fatalf("PWD value was redacted but PWD is not a secret: %q", got)
	}
}

// TestRedactSubprocessOutputRedactsDSNURICredentials asserts a credential
// carried by a sensitive-named env var is scrubbed even when it appears
// embedded inside a DSN/connection URI, where the text redactor's pattern
// pass alone cannot discover it from the output.
func TestRedactSubprocessOutputRedactsDSNURICredentials(t *testing.T) {
	dbpass := "db-pw-1234"
	env := []string{"DB_PASSWORD=" + dbpass}
	got := RedactSubprocessOutput(
		"connecting with postgres://user:"+dbpass+"@host:5432/db\n",
		env,
	)
	if strings.Contains(got, dbpass) {
		t.Fatalf("DSN-embedded password leaked: %q", got)
	}
}
func TestRedactSubprocessOutputMatchesDecodedJSONStrings(t *testing.T) {
	env := []string{"FIXTURE_API_TOKEN=value\nline"}
	input := `message {"other":"unrelated\u0020escape","value":"value\u000aline"}`
	want := `message {"other":"unrelated\u0020escape","value":"[REDACTED]"}`
	if got := RedactSubprocessOutput(input, env); got != want {
		t.Fatalf("unexpected diagnostic: %s", got)
	}
}

func TestRedactSubprocessOutputMasksOverlappingEnvironmentValues(t *testing.T) {
	const prefix = "fixture value\n"
	const longer = prefix + "joined overlap"
	const crossing = "overlap ends here"
	for _, env := range [][]string{
		{"PREFIX_API_TOKEN=" + prefix, "LONG_API_TOKEN=" + longer, "CROSSING_API_TOKEN=" + crossing},
		{"CROSSING_API_TOKEN=" + crossing, "LONG_API_TOKEN=" + longer, "PREFIX_API_TOKEN=" + prefix},
	} {
		for _, tc := range []struct{ input, want string }{
			{longer + " ends here", "[REDACTED]"},
			{`{"value":"fixture value\njoined overlap ends here"}`, `{"value":"[REDACTED]"}`},
			{`API_TOKEN="fixture value\njoined overlap ends here"`, `API_TOKEN=[REDACTED]`},
		} {
			if got := RedactSubprocessOutput(tc.input, env); got != tc.want {
				t.Fatalf("overlapping values were not completely masked: %q", got)
			}
		}
	}
}

func TestRedactSubprocessOutputMergesRawAndDecodedMatches(t *testing.T) {
	for _, env := range [][]string{
		{`DECODED_API_TOKEN=abcdefghij`, `RAW_API_TOKEN=abcdefgh\u0069jUNIQUE_SUFFIX`},
		{`RAW_API_TOKEN=abcdefgh\u0069jUNIQUE_SUFFIX`, `DECODED_API_TOKEN=abcdefghij`},
	} {
		const input = `{"value":"abcdefgh\u0069jUNIQUE_SUFFIX","context":"before abcdefgh\u0069j after"}`
		const want = `{"value":"[REDACTED]","context":"before [REDACTED] after"}`
		if got := RedactSubprocessOutput(input, env); got != want {
			t.Fatalf("redaction missed overlapping representations or altered unrelated evidence: %q", got)
		}
	}
}

func TestRedactSubprocessOutputDecodesEscapesAfterUnrelatedQuote(t *testing.T) {
	env := []string{"FIXTURE_API_TOKEN=prefix&unique-suffix", "UNICODE_API_TOKEN=fixture-😀-end"}
	const input = `prefix " {"diagnostic":"prefix\u0026unique-suffix","unicode":"fixture-\uD83D\uDE00-end","other":"preserve\u0020this"}`
	const want = `prefix " {"diagnostic":"[REDACTED]","unicode":"[REDACTED]","other":"preserve\u0020this"}`
	if got := RedactSubprocessOutput(input, env); got != want {
		t.Fatalf("redacted diagnostic = %q, want %q", got, want)
	}
}

func TestRedactSubprocessOutputPrefixMasksAcrossCut(t *testing.T) {
	const visible = "kept context "
	env := []string{"FIXTURE_API_TOKEN=fixture&unique"}
	for _, credential := range []string{"fixture&unique", `fixture\u0026unique`} {
		input := visible + credential + " unrelated tail"
		for cut := len(visible) + 1; cut < len(visible)+len(credential); cut++ {
			capture := input[:min(len(input), SubprocessOutputRedactionWindow(cut, env))]
			if got := RedactSubprocessOutputPrefix(capture, env, cut); got != visible+"[REDACTED]" {
				t.Fatalf("redaction at raw offset %d = %q", cut, got)
			}
		}
	}
}
