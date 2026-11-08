package utils

import (
	"encoding/json/jsontext"
	"strings"
)

type redactionJSONSpan struct{ start, end int }

// Keep serialized JSON out of the prose regular expressions: a credential
// example inside an escaped transcript must not destroy the enclosing JSON.
// Duplicate names are deliberately not decoded into maps and collapsed.
func redactionJSONSpans(content string) []redactionJSONSpan {
	if span, ok := redactionJSONDocument(content, 0); ok {
		return []redactionJSONSpan{span}
	}
	if !strings.ContainsAny(content, "`\n") {
		return nil
	}
	var spans []redactionJSONSpan
	fenceWidth, bodyStart := 0, 0
	position := 0
	for line := range strings.SplitAfterSeq(content, "\n") {
		text := strings.TrimSpace(line)
		width := len(text) - len(strings.TrimLeft(text, "`"))
		if fenceWidth == 0 {
			if width >= 3 && strings.EqualFold(strings.TrimSpace(text[width:]), "json") {
				fenceWidth, bodyStart = width, position+len(line)
			} else if span, ok := redactionJSONDocument(line, position); ok {
				spans = append(spans, span)
			}
		} else if width >= fenceWidth && width == len(text) {
			if span, ok := redactionJSONDocument(content[bodyStart:position], bodyStart); ok {
				spans = append(spans, span)
			}
			fenceWidth = 0
		}
		position += len(line)
	}
	return spans
}

func redactionJSONDocument(content string, offset int) (redactionJSONSpan, bool) {
	trimmed := strings.Trim(content, " \t\r\n")
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') || !jsontext.Value(trimmed).IsValid() {
		return redactionJSONSpan{}, false
	}
	start := len(content) - len(strings.TrimLeft(content, " \t\r\n"))
	return redactionJSONSpan{start: offset + start, end: offset + start + len(trimmed)}, true
}

func discoverTextSecrets(content string) {
	spans := redactionJSONSpans(content)
	position := 0
	for _, span := range spans {
		learnSecretsFrom(content[position:span.start])
		if value, err := decodeRedactionJSON([]byte(content[span.start:span.end])); err == nil {
			discoverJSONSecrets(value, "")
		}
		position = span.end
	}
	learnSecretsFrom(content[position:])
}

func redactStructuredText(content string, learn bool) (string, bool) {
	spans := redactionJSONSpans(content)
	if len(spans) == 0 {
		return "", false
	}
	if learn {
		// Discover across all blocks before replacing any occurrence, including
		// a bare value printed before its credential-keyed JSON definition.
		discoverTextSecrets(content)
	}
	var out strings.Builder
	position := 0
	for _, span := range spans {
		out.WriteString(redactSecretsPlainText(content[position:span.start], false))
		raw := content[span.start:span.end]
		redacted, err := RedactJSONCompact([]byte(raw))
		if err == nil {
			if ordered, orderErr := redactedJSONInOriginalOrder(jsontext.Value(raw), redacted); orderErr == nil {
				redacted = ordered
			}
		}
		if err != nil {
			// No malformed or ambiguous JSON bypasses ordinary redaction.
			out.WriteString(redactSecretsPlainText(raw, false))
		} else {
			out.Write(redacted)
		}
		position = span.end
	}
	out.WriteString(redactSecretsPlainText(content[position:], false))
	result := out.String()
	// Preserve protections whose scope can cross a fence boundary. Ordinary
	// key/value expressions must never run over the serialized JSON again.
	if strings.Contains(result, "PRIVATE KEY-----") {
		result = privateKeyBlockRe.ReplaceAllString(result, redactedSecret)
	}
	return redactRegisteredSecrets(redactLearnedSecrets(result)), true
}
