package utils

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	maxScopedCredentialBytes  = 64 * 1024
	maxScopedRedactionMatches = 4096
)

type scopedCredential struct {
	probe   string
	pattern *regexp.Regexp // nil for an exact byte match
}

// SecretRedactionScope retains credentials during a bounded diagnostic
// operation. It does not depend on the process-wide learning cache.
type SecretRedactionScope struct {
	values      map[string]struct{}
	credentials []scopedCredential
	bytes       int
	incomplete  bool
}

func (s *SecretRedactionScope) DiscoverText(text string) {
	if !s.incomplete {
		discoverTextSecrets(text, s.record)
	}
}

// DiscoverEnvironment registers known credentials from a subprocess's
// allowlisted environment, including values that do not look like tokens.
func (s *SecretRedactionScope) DiscoverEnvironment(env []string) {
	for _, value := range subprocessSecrets(env) {
		s.recordValue(value, true)
	}
}

func (s *SecretRedactionScope) DiscoverJSON(data []byte) error {
	value, err := decodeRedactionJSON(data)
	if err != nil {
		return err
	}
	if !s.incomplete {
		discoverJSONSecrets(value, "", s.record)
	}
	return nil
}

func (s *SecretRedactionScope) Complete() bool { return !s.incomplete }

// Discard marks discovery incomplete when upstream diagnostic data was lost.
func (s *SecretRedactionScope) Discard() {
	s.incomplete = true
	s.values, s.credentials = nil, nil
}

func (s *SecretRedactionScope) RedactText(text string) string {
	if s != nil && s.incomplete {
		return redactedSecret
	}
	return redactSecretsText(text, s == nil, s)
}

func (s *SecretRedactionScope) RedactJSONCompact(data []byte) ([]byte, error) {
	if s != nil && s.incomplete {
		return []byte(`"[REDACTED]"`), nil
	}
	return redactJSONCompact(data, s)
}

func (s *SecretRedactionScope) matchesJSONScalar(value any) bool {
	var encoded string
	switch scalar := value.(type) {
	case json.Number:
		encoded = scalar.String()
	case bool:
		encoded = strconv.FormatBool(scalar)
	case nil:
		encoded = "null"
	default:
		return false
	}
	return s.mask(encoded) != encoded
}

func (s *SecretRedactionScope) record(raw string) {
	if value := learnableSecretValue(raw); value != "" {
		s.recordValue(value, false)
	}
}

func (s *SecretRedactionScope) recordValue(value string, exact bool) {
	if s.incomplete {
		return
	}
	if _, exists := s.values[value]; exists {
		return
	}
	if len(s.values) >= maxLearnedSecrets || len(value) > maxScopedCredentialBytes-s.bytes {
		s.Discard()
		return
	}
	// Protocol decoding errors may quote a rejected value as a Go string.
	// Keep that spelling alongside the decoded spelling used by JSON sinks.
	quoted := strconv.Quote(value)
	escaped := quoted[1 : len(quoted)-1]
	cost := len(value)
	if escaped != value {
		cost += len(escaped)
	}
	if cost > maxScopedCredentialBytes-s.bytes {
		s.Discard()
		return
	}
	if s.values == nil {
		s.values = make(map[string]struct{})
	}
	s.values[value] = struct{}{}
	s.bytes += cost
	if exact {
		s.credentials = append(s.credentials, scopedCredential{probe: value})
	} else {
		s.credentials = append(s.credentials, scopedCredential{learnedSecretProbe(value), learnedSecretPattern(value)})
	}
	if escaped != value {
		s.credentials = append(s.credentials, scopedCredential{probe: escaped})
	}
}

func (c scopedCredential) appendMatches(matches [][]int, text string) [][]int {
	if c.pattern != nil {
		return append(matches, c.pattern.FindAllStringIndex(text, maxScopedRedactionMatches-len(matches))...)
	}
	// Environment values are arbitrary bytes. Match them literally, including
	// overlapping occurrences, without compiling them as UTF-8 patterns.
	for offset := 0; len(matches) < maxScopedRedactionMatches; {
		index := strings.Index(text[offset:], c.probe)
		if index < 0 {
			break
		}
		start := offset + index
		matches = append(matches, []int{start, start + len(c.probe)})
		offset = start + 1
	}
	return matches
}

func (s *SecretRedactionScope) mask(text string) string {
	if s == nil || text == "" {
		return text
	}
	var matches [][]int
	for _, credential := range s.credentials {
		if !strings.Contains(text, credential.probe) {
			continue
		}
		matches = credential.appendMatches(matches, text)
		if len(matches) >= maxScopedRedactionMatches {
			return redactedSecret
		}
	}
	if len(matches) == 0 {
		return text
	}
	// Merge overlapping occurrences before adding markers so one replacement
	// cannot invalidate a match for a different credential.
	slices.SortFunc(matches, func(a, b []int) int { return a[0] - b[0] })
	var out strings.Builder
	end := 0
	for _, match := range matches {
		if match[0] >= end {
			out.WriteString(text[end:match[0]])
			out.WriteString(redactedSecret)
		}
		end = max(end, match[1])
	}
	out.WriteString(text[end:])
	return out.String()
}
