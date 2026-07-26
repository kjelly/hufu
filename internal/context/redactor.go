package context

import (
	"crypto/sha256"
	"fmt"
	"regexp"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?im)\b(authorization\s*:\s*(?:bearer|basic)\s+)([^\s]+)`),
	// Matches both "key=value" and "Header: value" forms for any identifier
	// containing a secret-shaped keyword, whether the keyword is the whole
	// identifier ("password=", "token:") or embedded in a longer one
	// ("X-Api-Key:", "DATABASE_PASSWORD=", "Set-Cookie:"). The identifier
	// part is optional on both sides of the keyword, so the keyword itself
	// is never required to have a preceding character to match.
	regexp.MustCompile(`(?im)\b([\w-]*(?:api[_-]?key|access[_-]?key|secret[_-]?key|token|password|secret|cookie|session)[\w-]*["']?\s*[:=]\s*["']?)([^\s"';,]+)`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
}

// RedactSecrets replaces likely credentials before they can reach SQLite,
// projection files, logs, or errors. The digest allows duplicate detection
// without retaining the secret.
func RedactSecrets(in string) string {
	out := in
	for i, re := range secretPatterns {
		out = re.ReplaceAllStringFunc(out, func(match string) string {
			sum := sha256.Sum256([]byte(match))
			if i == 2 {
				return fmt.Sprintf("<REDACTED:PRIVATE_KEY:sha256=%x>", sum[:8])
			}
			parts := re.FindStringSubmatch(match)
			if len(parts) >= 2 {
				return parts[1] + fmt.Sprintf("<REDACTED:sha256=%x>", sum[:8])
			}
			return fmt.Sprintf("<REDACTED:SECRET:sha256=%x>", sum[:8])
		})
	}
	return out
}
