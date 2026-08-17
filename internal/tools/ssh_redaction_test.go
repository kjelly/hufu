//go:build linux || darwin
// +build linux darwin

package tools

import (
	"os"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/utils"
)

// TestSSHErrorPathRedactsPasswordFromStderr guards the SSH/SCP error branch.
// executeSSH builds the model-facing response from output that was first
// scrubbed with utils.RedactSubprocessOutput(output, cmd.Env) and then passed
// through buildBashResponse. When sshpass authenticates via the SSHPASS
// environment variable, an SSH error diagnostic may echo the connection
// context; the raw password must never reach the worker. This test exercises
// the same seam (RedactSubprocessOutput -> buildBashResponse) that executeSSH
// and executeSCP use, with an sshpass-shaped environment and a non-zero
// stderr that mentions the secret, and asserts the final response carries no
// secret while still reporting the diagnostic.
func TestSSHErrorPathRedactsPasswordFromStderr(t *testing.T) {
	password := "hunter2-sshpass-secret"
	// Mirror the environment executeSSH constructs for sshpass -e ssh.
	env := utils.SanitizeSubprocessEnv(append(os.Environ(), "SSHPASS="+password))

	// A non-zero-stderr diagnostic that references the password, as a real
	// SSH client might when echoing a rejected credential or a verbose trace.
	stderr := "Permission denied (publickey,password).\n" +
		"debug1: authenticating with password " + password + " for host 10.0.0.1\n"
	stdout := ""

	redactedStdout := utils.RedactSubprocessOutput(stdout, env)
	redactedStderr := utils.RedactSubprocessOutput(stderr, env)
	response := buildBashResponse(redactedStdout, redactedStderr, 255)

	content := response.Content
	if strings.Contains(content, password) {
		t.Fatalf("SSH error response leaked password: %q", content)
	}
	// The diagnostic text must survive redaction so the worker can act on it.
	if !strings.Contains(content, "Permission denied") {
		t.Fatalf("SSH error diagnostic was lost after redaction: %q", content)
	}
}

// TestSSHErrorPathPreservesNonSecretStderr ensures that ordinary non-zero
// stderr with no env-bound secret is reported verbatim (modulo the text
// redactor), so the redaction seam does not erase useful diagnostics when
// there is nothing to scrub.
func TestSSHErrorPathPreservesNonSecretStderr(t *testing.T) {
	env := utils.SanitizeSubprocessEnv(os.Environ())
	stderr := "ssh: connect to host example.com port 22: Connection refused\n"

	redactedStderr := utils.RedactSubprocessOutput(stderr, env)
	response := buildBashResponse("", redactedStderr, 255)

	if !strings.Contains(response.Content, "Connection refused") {
		t.Fatalf("non-secret stderr diagnostic was dropped: %q", response.Content)
	}
}
