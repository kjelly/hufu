package utils

import (
	"os"
	"regexp"
	"strings"
)

var sensitiveEnvironmentName = regexp.MustCompile(`(?i)(?:^|_)(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?|authorization)(?:_|$)`)

// SanitizeSubprocessEnv strips Hufu's process-only secret from subprocess
// environments. Application credentials remain available to the subprocess
// when the command needs them; RedactSubprocessOutput is the model-facing
// boundary that prevents those values from being returned to a worker.
func SanitizeSubprocessEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	sanitized := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "HUFU_HMAC_SECRET=") {
			continue
		}
		sanitized = append(sanitized, e)
	}
	return sanitized
}

// RedactSubprocessOutput removes values belonging to sensitive environment
// variables from command output before it is exposed to an agent or persisted
// in a tool result. This covers both keyed output ("SECRET=value") and bare
// output ("printf %s \"$SECRET\"") where the normal text redactor cannot
// discover the value from the output itself.
func RedactSubprocessOutput(output string, env []string) string {
	output = RedactSecrets(output)
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" || !sensitiveEnvironmentName.MatchString(name) {
			continue
		}
		output = strings.ReplaceAll(output, value, "[REDACTED]")
	}
	return RedactSecrets(output)
}
