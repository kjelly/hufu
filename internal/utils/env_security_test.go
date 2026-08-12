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
