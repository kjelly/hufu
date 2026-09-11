package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// CanonicalFormVersion identifies the encoder that produced a hash.
//
// The digest changes whenever this file's rules change, which is correct: a
// different byte form is a different hash. What the digest alone cannot say is
// *why* it changed — evidence that really moved, or an encoder upgrade. Both
// look like "the evidence changed" to every comparison in the runtime, so a
// release that touches the encoder would otherwise appear to invalidate every
// decision ever recorded.
//
// The version is stamped on the packet and folded into the hash input. In the
// hash so two encoders can never produce the same digest for the same evidence
// and be mistaken for one another; on the packet so a reader can explain the
// difference instead of only observing it.
//
// Bump this whenever the canonical byte form changes, including when a field
// joins or leaves the material set (§15.2).
const CanonicalFormVersion = 1

// Canonical encoding for decision evidence
// (docs/architecture/decision-runtime.md §15.3).
//
// encoding/json is not used directly for the sealed form: struct field order,
// HTML escaping and float formatting are all implementation details that could
// change the hash without any evidence changing. This writer fixes them.
//
// Rules:
//  1. strings are UTF-8, Unicode NFC, trimmed
//  2. object keys are sorted byte-wise ascending, no insignificant whitespace
//  3. floats use strconv.FormatFloat(v, 'f', 6, 64) after rounding
//  4. integers are decimal with no plus sign
//  5. nil, empty strings and empty collections omit their key entirely, so
//     "absent" and "empty" can never hash differently
//  6. the digest is lowercase hex SHA-256 over this form

// canonicalString normalizes a string for hashing.
func canonicalString(s string) string {
	return norm.NFC.String(strings.TrimSpace(s))
}

// canonicalObject is an ordered-on-write map. Keys are sorted at encode time,
// so callers may add fields in whatever order reads best.
type canonicalObject map[string]any

// set stores a value unless it is empty by rule 5.
func (o canonicalObject) set(key string, value any) {
	if canonicalIsEmpty(value) {
		return
	}
	o[key] = value
}

// setString stores a normalized string unless it is empty after trimming.
func (o canonicalObject) setString(key, value string) {
	o.set(key, canonicalString(value))
}

func canonicalIsEmpty(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case canonicalObject:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

// canonicalEncode writes the canonical byte form of value.
func canonicalEncode(value any) ([]byte, error) {
	var buf bytes.Buffer
	if err := canonicalWrite(&buf, value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func canonicalWrite(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		buf.WriteString(strconv.FormatBool(v))
		return nil
	case string:
		return canonicalWriteString(buf, v)
	case float64:
		buf.WriteString(strconv.FormatFloat(roundDecision(v), 'f', decisionRoundDigits, 64))
		return nil
	case float32:
		buf.WriteString(strconv.FormatFloat(roundDecision(float64(v)), 'f', decisionRoundDigits, 64))
		return nil
	case int:
		buf.WriteString(strconv.FormatInt(int64(v), 10))
		return nil
	case int64:
		buf.WriteString(strconv.FormatInt(v, 10))
		return nil
	case json.Number:
		return canonicalWriteNumber(buf, v.String())
	case canonicalObject:
		return canonicalWriteObject(buf, v)
	case map[string]any:
		return canonicalWriteObject(buf, canonicalObject(v))
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := canonicalWrite(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	}
	// Anything else (a caller's own struct, a typed number) is routed through
	// JSON into the generic shapes above so one code path decides formatting.
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("canonicalizing %T: %w", value, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return fmt.Errorf("canonicalizing %T: %w", value, err)
	}
	return canonicalWrite(buf, generic)
}

func canonicalWriteString(buf *bytes.Buffer, s string) error {
	normalized := canonicalString(s)
	// json.Marshal escapes control characters and quotes correctly; HTML
	// escaping is disabled so the byte form depends only on the text.
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalized); err != nil {
		return fmt.Errorf("canonicalizing string: %w", err)
	}
	buf.Write(bytes.TrimRight(encoded.Bytes(), "\n"))
	return nil
}

func canonicalWriteNumber(buf *bytes.Buffer, text string) error {
	if i, err := strconv.ParseInt(text, 10, 64); err == nil {
		buf.WriteString(strconv.FormatInt(i, 10))
		return nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return fmt.Errorf("canonicalizing number %q: %w", text, err)
	}
	buf.WriteString(strconv.FormatFloat(roundDecision(f), 'f', decisionRoundDigits, 64))
	return nil
}

func canonicalWriteObject(buf *bytes.Buffer, obj canonicalObject) error {
	keys := make([]string, 0, len(obj))
	for key := range obj {
		if canonicalIsEmpty(obj[key]) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := canonicalWriteString(buf, key); err != nil {
			return err
		}
		buf.WriteByte(':')
		if err := canonicalWrite(buf, obj[key]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}
