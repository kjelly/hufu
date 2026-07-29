package utils

import (
	"os"
	"strings"
)

// SanitizeSubprocessEnv strips secret environment variables like HUFU_HMAC_SECRET from subprocess environments.
// If env is nil, os.Environ() is sanitized.
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
