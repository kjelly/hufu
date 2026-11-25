package utils

import (
	"cmp"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
)

var sensitiveEnvironmentName = regexp.MustCompile(`(?i)(?:^|_)(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?|authorization|sshpass)(?:_|$)`)
var jsonOutputEscape = regexp.MustCompile(`\\(?:["\\/bfnrt]|u(?:[dD][89a-bA-B][0-9a-fA-F]{2}\\u[dD][c-fC-F][0-9a-fA-F]{2}|[0-9a-fA-F]{4}))`)

// nonSecretEnvNames lists environment variable names that look sensitive to
// the heuristic above but are explicitly not secrets. PWD is the shell's
// current-directory marker and must remain visible in subprocess output.
var nonSecretEnvNames = map[string]bool{
	"PWD": true,
}

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
//
// A sensitive-named variable whose value is very short is still redacted: the
// boundary is the key/name context, not a minimum-length heuristic, so short
// passwords or OTPs are not leaked. PWD is kept as an explicit non-secret
// exception.
func RedactSubprocessOutput(output string, env []string) string {
	return redactSubprocessOutput(output, subprocessSecrets(env), len(output))
}

// SubprocessOutputRedactionWindow includes enough lookahead to authenticate an
// environment value beginning before the visible prefix. JSON escapes can use
// six wire bytes for each byte of the underlying value.
func SubprocessOutputRedactionWindow(prefixBytes int, env []string) int {
	longest := 0
	for _, secret := range subprocessSecrets(env) {
		longest = max(longest, len(secret))
	}
	return prefixBytes + 6*longest
}

// RedactSubprocessOutputPrefix uses the retained lookahead for matching, then
// publishes only the requested raw prefix with any overlapping matches masked.
func RedactSubprocessOutputPrefix(output string, env []string, prefixBytes int) string {
	return redactSubprocessOutput(output, subprocessSecrets(env), min(max(0, prefixBytes), len(output)))
}

func subprocessSecrets(env []string) []string {
	var secrets []string
	seen := make(map[string]bool)
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || value == "" || seen[value] {
			continue
		}
		if nonSecretEnvNames[strings.ToUpper(name)] {
			continue
		}
		if !sensitiveEnvironmentName.MatchString(name) {
			continue
		}
		secrets = append(secrets, value)
		seen[value] = true
	}
	return secrets
}

func redactSubprocessOutput(output string, secrets []string, prefixBytes int) string {
	if len(secrets) == 0 {
		return RedactSecrets(output[:prefixBytes])
	}
	spans := subprocessSecretSpans(output, secrets, prefixBytes)
	if decoded, escapes := decodeOutputEscapes(output); len(escapes) != 0 {
		for _, match := range subprocessSecretSpans(decoded, secrets, prefixBytes) {
			start := outputWireOffset(escapes, match.start, false)
			if start >= prefixBytes {
				continue
			}
			spans = append(spans, subprocessRedaction{
				start: start,
				end:   outputWireOffset(escapes, match.end, true),
			})
		}
	}
	return RedactSecrets(applySubprocessRedactions(output, spans, prefixBytes))
}

type subprocessRedaction struct {
	start, end int
}

type outputEscape struct {
	wire, decoded subprocessRedaction
}

// Build a second search view without interpreting the surrounding output as
// JSON. A logging prefix or unmatched quote can precede an escaped value.
func decodeOutputEscapes(output string) (string, []outputEscape) {
	if !strings.Contains(output, `\`) {
		return output, nil
	}
	var decoded strings.Builder
	var escapes []outputEscape
	last := 0
	for _, bounds := range jsonOutputEscape.FindAllStringIndex(output, -1) {
		start, end := bounds[0], bounds[1]
		var value string
		if err := json.Unmarshal([]byte("\""+output[start:end]+"\""), &value); err != nil {
			continue
		}
		decoded.WriteString(output[last:start])
		decodedStart := decoded.Len()
		decoded.WriteString(value)
		escapes = append(escapes, outputEscape{
			wire:    subprocessRedaction{start, end},
			decoded: subprocessRedaction{decodedStart, decoded.Len()},
		})
		last = end
	}
	if len(escapes) == 0 {
		return output, nil
	}
	decoded.WriteString(output[last:])
	return decoded.String(), escapes
}

func outputWireOffset(escapes []outputEscape, offset int, upperBound bool) int {
	index := sort.Search(len(escapes), func(i int) bool { return escapes[i].decoded.end > offset })
	if index < len(escapes) {
		next := escapes[index]
		if offset >= next.decoded.start {
			if upperBound && offset > next.decoded.start {
				return next.wire.end
			}
			return next.wire.start
		}
	}
	if index == 0 {
		return offset
	}
	previous := escapes[index-1]
	return offset + previous.wire.end - previous.decoded.end
}

// Locate all raw matches before replacing any. A short or overlapping match
// must not prevent discovery of the rest of another sensitive value.
func subprocessSecretSpans(content string, secrets []string, startLimit int) []subprocessRedaction {
	var matches []subprocessRedaction
	for _, secret := range secrets {
		for offset := 0; offset < min(startLimit, len(content)); {
			index := strings.Index(content[offset:], secret)
			if index < 0 || offset+index >= startLimit {
				break
			}
			start := offset + index
			matches = append(matches, subprocessRedaction{start, start + len(secret)})
			offset = start + 1
		}
	}
	return matches
}

func applySubprocessRedactions(content string, spans []subprocessRedaction, prefixBytes int) string {
	if len(spans) == 0 {
		return content[:prefixBytes]
	}
	slices.SortFunc(spans, func(a, b subprocessRedaction) int { return cmp.Compare(a.start, b.start) })
	var cleaned strings.Builder
	lastEnd := 0
	for i := 0; i < len(spans) && spans[i].start < prefixBytes; {
		current := spans[i]
		i++
		// Raw and decoded matches can overlap in the original output.
		for i < len(spans) && spans[i].start < current.end {
			current.end = max(current.end, spans[i].end)
			i++
		}
		cleaned.WriteString(content[lastEnd:current.start])
		cleaned.WriteString(redactedSecret)
		lastEnd = current.end
	}
	if lastEnd < prefixBytes {
		cleaned.WriteString(content[lastEnd:prefixBytes])
	}
	return cleaned.String()
}
