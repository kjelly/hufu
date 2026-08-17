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
