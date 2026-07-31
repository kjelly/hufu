package utils

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

const redactedSecret = "[REDACTED]"

var (
	// Matches YAML and JSON key/value pairs for the credential names commonly
	// passed through agent prompts. The value is deliberately limited to one
	// line: multiline configuration bodies are not credentials by themselves.
	secretKeyValueRe      = regexp.MustCompile(`(?im)(\b(?:[a-z0-9_.-]*(?:password|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[a-z0-9_.-]*)\b\s*[:=]\s*)(\\"(?:\\.|[^"\\])*\\"|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,}\]"']+)`)
	secretJSONRe          = regexp.MustCompile(`(?i)("[a-z0-9_.-]*(?:password|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[a-z0-9_.-]*"\s*:\s*)("(?:\\.|[^"\\])*")`)
	secretAuthorizationRe = regexp.MustCompile(`(?im)(\bauthorization\s*:\s*(?:bearer|basic)\s+)([^\s]+)`)
	privateKeyBlockRe     = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// Environment assignments may use all-caps names and optional export.
	secretEnvRe     = regexp.MustCompile(`(?m)(\b(?:export\s+)?[A-Z][A-Z0-9_]*(?:PASSWORD|SECRET|TOKEN|API_KEY|ACCESS_KEY|PRIVATE_KEY)[A-Z0-9_]*=)([^\s]+)`)
	secretKeyNameRe = regexp.MustCompile(`(?i)(?:password|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)`)
)

// RedactSecrets removes recognizable credential values before content is
// persisted to workspace logs or session state. It intentionally preserves
// keys and surrounding prose so diagnostics remain useful.
func RedactSecrets(content string) string {
	content = privateKeyBlockRe.ReplaceAllString(content, redactedSecret)
	content = secretAuthorizationRe.ReplaceAllString(content, "${1}"+redactedSecret)
	content = secretJSONRe.ReplaceAllString(content, `${1}"`+redactedSecret+`"`)
	content = secretKeyValueRe.ReplaceAllStringFunc(content, redactKeyValue)
	return secretEnvRe.ReplaceAllString(content, "${1}"+redactedSecret)
}

// RedactJSON redacts secrets in a JSON document without applying text
// substitutions to the JSON syntax itself. String values are redacted as
// prose, while credential-looking object keys have their values replaced.
// The returned document is always freshly marshaled JSON.
func RedactJSON(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	value = redactJSONValue(value, "")
	return json.MarshalIndent(value, "", "  ")
}

func redactJSONValue(value any, key string) any {
	if key != "" && secretKeyNameRe.MatchString(key) {
		return redactedSecret
	}
	switch v := value.(type) {
	case string:
		return RedactSecrets(v)
	case []any:
		for i := range v {
			v[i] = redactJSONValue(v[i], "")
		}
	case map[string]any:
		for k, item := range v {
			v[k] = redactJSONValue(item, k)
		}
	}
	return value
}

func redactKeyValue(match string) string {
	parts := secretKeyValueRe.FindStringSubmatch(match)
	if len(parts) != 3 {
		return match
	}
	value := parts[2]
	if strings.HasPrefix(value, `"`) {
		return parts[1] + `"` + redactedSecret + `"`
	}
	if strings.HasPrefix(value, `\"`) {
		return parts[1] + `\"` + redactedSecret + `\"`
	}
	if strings.HasPrefix(value, "'") {
		return parts[1] + "'" + redactedSecret + "'"
	}
	return parts[1] + redactedSecret
}
