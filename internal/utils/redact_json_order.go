package utils

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

type redactedJSONNode struct {
	kind   jsontext.Kind
	value  jsontext.Value
	fields map[string]*redactedJSONNode
	items  []*redactedJSONNode
}

type redactedJSONReplacement struct {
	start, end int
	value      jsontext.Value
}

// Apply the JSON redactor's values to the original serialization. Reordering a
// large diagnostic can move its identifying fields past a bounded preview.
// Each document is traversed once, without copying every enclosing subtree.
func redactedJSONInOriginalOrder(original, redacted jsontext.Value) ([]byte, error) {
	if bytes.Equal(original, redacted) {
		return original, nil
	}
	target, err := readRedactedJSONNode(jsontext.NewDecoder(bytes.NewReader(redacted)))
	if err != nil {
		return nil, err
	}
	var replacements []redactedJSONReplacement
	if err := collectJSONRedactions(jsontext.NewDecoder(bytes.NewReader(original)), target, &replacements); err != nil {
		return nil, err
	}
	if len(replacements) == 0 {
		return original, nil
	}
	var out bytes.Buffer
	position := 0
	for _, replacement := range replacements {
		out.Write(original[position:replacement.start])
		out.Write(replacement.value)
		position = replacement.end
	}
	out.Write(original[position:])
	return out.Bytes(), nil
}

func readRedactedJSONNode(decoder *jsontext.Decoder) (*redactedJSONNode, error) {
	node := &redactedJSONNode{kind: decoder.PeekKind()}
	switch node.kind {
	case '{', '[':
		if _, err := decoder.ReadToken(); err != nil {
			return nil, err
		}
		closeKind := jsontext.Kind(']')
		if node.kind == '{' {
			closeKind = '}'
			node.fields = make(map[string]*redactedJSONNode)
		}
		for decoder.PeekKind() != closeKind {
			key := ""
			if node.kind == '{' {
				token, err := decoder.ReadToken()
				if err != nil {
					return nil, err
				}
				key = token.String()
			}
			child, err := readRedactedJSONNode(decoder)
			if err != nil {
				return nil, err
			}
			if node.kind == '{' {
				node.fields[key] = child
			} else {
				node.items = append(node.items, child)
			}
		}
		if _, err := decoder.ReadToken(); err != nil {
			return nil, err
		}
	default:
		value, err := decoder.ReadValue()
		if err != nil {
			return nil, err
		}
		node.value = bytes.Clone(value)
	}
	return node, nil
}

func collectJSONRedactions(decoder *jsontext.Decoder, target *redactedJSONNode, replacements *[]redactedJSONReplacement) error {
	kind := decoder.PeekKind()
	if kind == target.kind && (kind == '{' || kind == '[') {
		if _, err := decoder.ReadToken(); err != nil {
			return err
		}
		closeKind, length := jsontext.Kind(']'), len(target.items)
		if kind == '{' {
			closeKind, length = '}', len(target.fields)
		}
		index := 0
		for decoder.PeekKind() != closeKind {
			var child *redactedJSONNode
			if kind == '{' {
				key, err := decoder.ReadToken()
				if err != nil {
					return err
				}
				child = target.fields[key.String()]
			} else if index < length {
				child = target.items[index]
			}
			if child == nil {
				return fmt.Errorf("redaction changed JSON container members")
			}
			if err := collectJSONRedactions(decoder, child, replacements); err != nil {
				return err
			}
			index++
		}
		if index != length {
			return fmt.Errorf("redaction changed JSON container length")
		}
		_, err := decoder.ReadToken()
		return err
	}
	if target.value == nil {
		return fmt.Errorf("redaction changed a scalar into a container")
	}
	value, err := decoder.ReadValue()
	if err != nil {
		return err
	}
	if bytes.Equal(value, target.value) {
		return nil
	}
	if kind == '"' && target.kind == '"' {
		var before, after string
		if err := json.Unmarshal(value, &before); err != nil {
			return err
		}
		if err := json.Unmarshal(target.value, &after); err != nil {
			return err
		}
		if before == after {
			return nil
		}
	}
	end := int(decoder.InputOffset())
	*replacements = append(*replacements, redactedJSONReplacement{start: end - len(value), end: end, value: target.value})
	return nil
}
