package paired

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// DecodeYAML rejects coercion (for example 123 into a string), unknown fields,
// duplicate keys, nulls and alias cycles before decoding into the typed target.
// Explicit null has no meaning in authored probe contracts; omission expresses
// optional fields. YAML merge keys are deliberately not a contract feature.
func DecodeYAML(raw []byte, target any) error {
	if len(raw) > MaxDataBytes || !utf8.Valid(raw) {
		return errors.New("document: exceeds size limit or is not UTF-8")
	}
	typ := reflect.TypeOf(target)
	if typ == nil || typ.Kind() != reflect.Pointer || reflect.ValueOf(target).IsNil() {
		return errors.New("document: target must be a non-nil pointer")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return fmt.Errorf("document: %w", err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("document: exactly one YAML document required")
	}
	if len(doc.Content) != 1 {
		return errors.New("document: value required")
	}
	check := yamlShape{active: map[*yaml.Node]bool{}}
	if err := check.validate(doc.Content[0], typ.Elem(), "$"); err != nil {
		return err
	}
	if err := doc.Decode(target); err != nil {
		return fmt.Errorf("document: %w", err)
	}
	return nil
}

type yamlShape struct {
	active map[*yaml.Node]bool
	nodes  int
	depth  int
}

func (s *yamlShape) validate(n *yaml.Node, typ reflect.Type, field string) error {
	s.nodes++
	s.depth++
	defer func() { s.depth-- }()
	if s.nodes > 100_000 || s.depth > 128 {
		return fmt.Errorf("%s: YAML expansion limit exceeded", field)
	}
	if s.active[n] {
		return fmt.Errorf("%s: alias cycle", field)
	}
	s.active[n] = true
	defer delete(s.active, n)
	if n.Kind == yaml.AliasNode {
		return s.validate(n.Alias, typ, field)
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.Interface {
		return s.validateValue(n, field)
	}
	if n.Tag == "!!null" {
		return fmt.Errorf("%s: null is not permitted", field)
	}
	switch typ.Kind() {
	case reflect.Struct, reflect.Map:
		return s.validateMapping(n, typ, field)
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			return fmt.Errorf("%s: expected sequence", field)
		}
		for i, child := range n.Content {
			if err := s.validate(child, typ.Elem(), fmt.Sprintf("%s[%d]", field, i)); err != nil {
				return err
			}
		}
		return nil
	default:
		return validateScalar(n, typ.Kind(), field)
	}
}

func (s *yamlShape) validateMapping(n *yaml.Node, typ reflect.Type, field string) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: expected mapping", field)
	}
	fields := map[string]reflect.Type{}
	if typ.Kind() == reflect.Struct {
		for f := range typ.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			if name != "-" && f.IsExported() {
				fields[name] = f.Type
			}
		}
	}
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
			return fmt.Errorf("%s: duplicate or non-string key %q", field, key.Value)
		}
		seen[key.Value] = true
		var childType reflect.Type
		if typ.Kind() == reflect.Map {
			childType = typ.Elem()
		} else {
			childType = fields[key.Value]
			if childType == nil {
				return fmt.Errorf("%s.%s: unknown field", field, key.Value)
			}
		}
		if err := s.validate(value, childType, field+"."+key.Value); err != nil {
			return err
		}
	}
	return nil
}

func (s *yamlShape) validateValue(n *yaml.Node, field string) error {
	switch n.Kind {
	case yaml.MappingNode:
		return s.validateMapping(n, reflect.TypeFor[map[string]any](), field)
	case yaml.SequenceNode:
		for i, child := range n.Content {
			if err := s.validate(child, reflect.TypeFor[any](), fmt.Sprintf("%s[%d]", field, i)); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str", "!!bool", "!!int", "!!null":
			return nil
		case "!!float":
			var value float64
			if err := n.Decode(&value); err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
				return fmt.Errorf("%s: non-finite number", field)
			}
			return nil
		default:
			return fmt.Errorf("%s: expected JSON string, boolean or integer", field)
		}
	default:
		return fmt.Errorf("%s: unsupported value", field)
	}
	return nil
}

func validateScalar(n *yaml.Node, kind reflect.Kind, field string) error {
	expected := ""
	switch kind {
	case reflect.String:
		expected = "!!str"
	case reflect.Bool:
		expected = "!!bool"
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint, reflect.Uint64:
		expected = "!!int"
	default:
		return fmt.Errorf("%s: unsupported contract type %s", field, kind)
	}
	if n.Kind != yaml.ScalarNode || n.Tag != expected {
		return fmt.Errorf("%s: expected %s", field, strings.TrimPrefix(expected, "!!"))
	}
	return nil
}
