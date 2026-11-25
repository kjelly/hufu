package team

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// A rejected provider response may contain multiple or prefixed JSON frames.
// Inspect complete values and readable truncated prefixes without treating
// any recovered frame as the canonical response.
func inspectProviderResultJSONFrames(raw string, maxBytes int, visit func(string) error) (bool, error) {
	if len(raw) > maxBytes {
		return false, fmt.Errorf("provider source inspection exceeds the response byte limit")
	}
	found := false
	for attempts := 0; ; attempts++ {
		start := strings.IndexAny(raw, "{[")
		if start < 0 {
			return found, nil
		}
		if attempts >= maxResultIssues {
			return found, fmt.Errorf("too many JSON frames to authenticate provider sources")
		}
		raw = raw[start:]
		frame, consumed := readableSourceContainerPrefix(raw)
		raw = raw[max(1, consumed):]
		if frame != "" {
			found = true
			if err := visit(frame); err != nil {
				return found, err
			}
		}
	}
}

func inspectProviderResultEnvelopeObjects(raw string, depth int, visit func([]submissionSourceField)) error {
	if depth > maxDynamicSchemaDepth {
		return fmt.Errorf("provider source inspection exceeds the response nesting limit")
	}
	switch kind := jsontext.Value(raw).Kind(); kind {
	case '{':
		visit(readableSourceMembers(raw))
	case '[':
		decoder := jsontext.NewDecoder(strings.NewReader(raw), jsontext.AllowDuplicateNames(true))
		if _, err := decoder.ReadToken(); err != nil {
			return err
		}
		for decoder.PeekKind() != ']' {
			value, err := decoder.ReadValue()
			if err != nil {
				return err
			}
			if err := inspectProviderResultEnvelopeObjects(string(value), depth+1, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func inspectReadablePayloadViews(raw jsontext.Value, inspect func([]byte) error) error {
	if raw.IsValid() {
		return inspect(raw)
	}
	views, err := duplicatePayloadSourceViews(raw)
	if err != nil {
		return rejectResultSourceRead(err)
	}
	for _, view := range views {
		if err := inspect(view); err != nil {
			return err
		}
	}
	return nil
}

// Traverse only array wrappers at a declared source field. Keep the original
// object tokens so duplicate names remain visible to the source-specific check.
func inspectProviderSourceList(raw jsontext.Value, depth int, visit func(jsontext.Value) error) error {
	if depth > maxDynamicSchemaDepth {
		return fmt.Errorf("provider source list exceeds the response nesting limit")
	}
	if raw.Kind() != '[' {
		return visit(raw)
	}
	entries, err := readSourceEntries(raw)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := inspectProviderSourceList(entry, depth+1, visit); err != nil {
			return err
		}
	}
	return nil
}

func inspectProviderPayloadObjects(raw []byte, depth int, inspect func(any, []byte) error) error {
	if depth > maxDynamicSchemaDepth {
		return rejectResultSourceRead(fmt.Errorf("provider payload source inspection exceeds the nesting limit"))
	}
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return rejectResultSourceRead(err)
	}
	if err := inspect(value, raw); err != nil {
		return err
	}
	// Numeric JSON Pointers may apply to the array itself. Also inspect
	// enclosed objects when an array was used to wrap a provider payload.
	entries, array := value.([]any)
	if !array {
		return nil
	}
	for _, entry := range entries {
		switch entry.(type) {
		case map[string]any, []any:
			encoded, err := json.Marshal(entry)
			if err != nil {
				return rejectResultSourceRead(err)
			}
			if err := inspectProviderPayloadObjects(encoded, depth+1, inspect); err != nil {
				return err
			}
		}
	}
	return nil
}

// A rejected payload with duplicate names can still contain readable source
// claims. Check every interpretation within the existing diagnostic budget.
// These views are used only to authenticate sources in rejected submissions.
func duplicatePayloadSourceViews(raw jsontext.Value) ([]jsontext.Value, error) {
	if len(raw) > resultPayloadMaxBytes {
		return nil, fmt.Errorf("source inspection exceeds the structured payload byte limit")
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(raw), jsontext.AllowDuplicateNames(true))
	views, err := readPayloadSourceViews(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.ReadToken(); err != io.EOF {
		return nil, fmt.Errorf("source inspection expected one structured payload value")
	}
	encoded := make([]jsontext.Value, 0, len(views))
	for _, view := range views {
		body, err := json.Marshal(view, json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, body)
	}
	return encoded, nil
}

func readPayloadSourceViews(decoder *jsontext.Decoder, depth int) ([]any, error) {
	if depth > maxDynamicSchemaDepth {
		return nil, fmt.Errorf("source inspection exceeds the schema depth limit")
	}
	switch decoder.PeekKind() {
	case '{':
		return readPayloadObjectSourceViews(decoder, depth)
	case '[':
		return readPayloadArraySourceViews(decoder, depth)
	default:
		value, err := decoder.ReadValue()
		return []any{value.Clone()}, err
	}
}

func readPayloadObjectSourceViews(decoder *jsontext.Decoder, depth int) ([]any, error) {
	if _, err := decoder.ReadToken(); err != nil {
		return nil, err
	}
	var names []string
	members := make(map[string][]any)
	for decoder.PeekKind() == '"' {
		key, err := decoder.ReadToken()
		if err != nil {
			return nil, err
		}
		name := key.String()
		choices, err := readPayloadSourceViews(decoder, depth+1)
		if err != nil {
			return nil, err
		}
		if _, seen := members[name]; !seen {
			names = append(names, name)
		}
		if len(members[name])+len(choices) > maxResultIssues {
			return nil, fmt.Errorf("too many interpretations to authenticate duplicate payload claims")
		}
		members[name] = append(members[name], choices...)
	}
	if _, err := decoder.ReadToken(); err != nil {
		return nil, err
	}
	all := []map[string]any{{}}
	for _, name := range names {
		var err error
		all, err = addPayloadSourceChoices(all, members[name], maps.Clone[map[string]any], func(object map[string]any, value any) map[string]any {
			object[name] = value
			return object
		})
		if err != nil {
			return nil, err
		}
	}
	views := make([]any, len(all))
	for i, object := range all {
		views[i] = object
	}
	return views, nil
}

func readPayloadArraySourceViews(decoder *jsontext.Decoder, depth int) ([]any, error) {
	if _, err := decoder.ReadToken(); err != nil {
		return nil, err
	}
	all := [][]any{{}}
	for decoder.PeekKind() != ']' {
		choices, err := readPayloadSourceViews(decoder, depth+1)
		if err != nil {
			return nil, err
		}
		all, err = addPayloadSourceChoices(all, choices, slices.Clone[[]any], func(array []any, value any) []any { return append(array, value) })
		if err != nil {
			return nil, err
		}
	}
	if _, err := decoder.ReadToken(); err != nil {
		return nil, err
	}
	views := make([]any, len(all))
	for i, array := range all {
		views[i] = array
	}
	return views, nil
}

func addPayloadSourceChoices[T any](acc []T, choices []any, clone func(T) T, add func(T, any) T) ([]T, error) {
	if len(choices) == 1 {
		for i, item := range acc {
			acc[i] = add(item, choices[0])
		}
		return acc, nil
	}
	count := len(acc) * len(choices)
	if count > maxResultIssues {
		return nil, fmt.Errorf("too many interpretations to authenticate duplicate payload claims")
	}
	branch := make([]T, 0, count)
	for _, item := range acc {
		for _, choice := range choices {
			branch = append(branch, add(clone(item), choice))
		}
	}
	return branch, nil
}
