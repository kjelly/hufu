package team

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

const maxCollectionFeedbackBytes = 8192

type collectionSchemaHint struct {
	Pointer       string   `json:"pointer"`
	AllowedFields []string `json:"allowed_fields"`
	Example       any      `json:"example,omitempty"`
}

type collectionFormatFeedback struct {
	Stage     string                    `json:"stage,omitempty"`
	Issues    []ResultSubmissionIssue   `json:"issues"`
	Objects   []collectionSchemaHint    `json:"objects,omitempty"`
	Truncated bool                      `json:"truncated"`
	Syntax    *collectionSyntaxFeedback `json:"syntax,omitempty"`
}

type collectionSyntaxFeedback struct {
	ByteOffset    int64 `json:"byte_offset"`
	Line          int   `json:"line"`
	ByteColumn    int   `json:"byte_column"`
	FirstValueEnd int64 `json:"first_value_end_byte_offset,omitzero"`
}

// Locate grammar failures without interpreting or repairing the claims. Only
// coordinates are copied; the complete rejected response remains authoritative.
func resultCollectionSyntaxFeedback(raw []byte) *collectionSyntaxFeedback {
	decoder := jsontext.NewDecoder(bytes.NewReader(raw))
	_, err := decoder.ReadValue()
	var firstEnd int64
	if err == nil {
		firstEnd = decoder.InputOffset()
		_, err = decoder.ReadToken()
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
	offset := decoder.InputOffset()
	if syntax, ok := errors.AsType[*jsontext.SyntacticError](err); ok {
		offset = syntax.ByteOffset
	}
	offset = int64(max(0, min(int(offset), len(raw))))
	prefix := raw[:offset]
	return &collectionSyntaxFeedback{
		ByteOffset: offset, Line: bytes.Count(prefix, []byte{'\n'}) + 1,
		ByteColumn: len(prefix) - bytes.LastIndexByte(prefix, '\n'), FirstValueEnd: firstEnd,
	}
}

// Feedback describes the rejected representation without changing its claims.
// Keep the full response durable; only this model-facing diagnostic is bounded.
func resultCollectionFormatFeedback(raw []byte, info fantasy.ToolInfo) *collectionFormatFeedback {
	if len(raw) == 0 {
		return nil
	}
	raw, _ = collectionResponseJSON(raw)
	issues, truncated, err := resultSubmissionIssues(string(raw), info)
	if err != nil || len(issues) == 0 {
		return nil // Preserve the original diagnostic when schema analysis is unavailable.
	}
	feedback := &collectionFormatFeedback{Truncated: truncated}
	if len(issues) == 1 && issues[0].Code == "invalid_json" {
		feedback.Syntax = resultCollectionSyntaxFeedback(raw)
	}
	root := submissionSchema(info)
	seen := make(map[string]bool)
	for _, issue := range issues {
		candidate := *feedback
		candidate.Issues = append(slices.Clone(feedback.Issues), issue)
		encoded, err := json.Marshal(candidate)
		if err != nil || len(encoded) > maxCollectionFeedbackBytes {
			feedback.Truncated = true
			break
		}
		*feedback = candidate
	}
	// Prioritize the error list over optional examples. Missing/extra property
	// pointers identify the parent object whose legal fields should be shown.
	for _, issue := range feedback.Issues {
		if issue.Code != "additional_property" && issue.Code != "missing_property" {
			continue
		}
		parent, _, _ := strings.CutLast(issue.Pointer, "/")
		if seen[parent] {
			continue
		}
		seen[parent] = true
		schema := collectionFeedbackSchemaAt(root, parent)
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			continue
		}
		hint := collectionSchemaHint{Pointer: parent, AllowedFields: slices.Sorted(maps.Keys(properties))}
		if example := generateCompactExampleWithArrayDefaults(schema, true); len(compactJSON(example)) <= maxToolRepairExampleBytes {
			hint.Example = example
		}
		candidate := *feedback
		candidate.Objects = append(slices.Clone(feedback.Objects), hint)
		encoded, err := json.Marshal(candidate)
		if err != nil || len(encoded) > maxCollectionFeedbackBytes {
			feedback.Truncated = true
			continue
		}
		*feedback = candidate
	}
	return feedback
}

// Follow directly declared object properties and homogeneous array items.
// The full published schema remains authoritative for refs and alternatives.
func collectionFeedbackSchemaAt(schema map[string]any, pointer string) map[string]any {
	if pointer == "" {
		return schema
	}
	for token := range strings.SplitSeq(strings.TrimPrefix(pointer, "/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		if schema["type"] == "array" {
			if index, err := strconv.Atoi(token); err != nil || index < 0 {
				return nil
			}
			schema, _ = schema["items"].(map[string]any)
		} else {
			properties, _ := schema["properties"].(map[string]any)
			schema, _ = properties[token].(map[string]any)
		}
		if schema == nil {
			return nil
		}
	}
	return schema
}
