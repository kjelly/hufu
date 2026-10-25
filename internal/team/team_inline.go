package team

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	FindingInlineAgentInvalid    = "inline_agent_invalid"
	FindingInlineAgentToolsEmpty = "inline_agent_tools_empty"
)

// teamAgentAuthoring retains only authored data; derived defaults never serialize.
type teamAgentAuthoring struct {
	node            *yaml.Node
	frontmatter     agentFrontmatter
	instructions    string
	hasInstructions bool
}

type teamAgentDeclarations map[string]teamAgentAuthoring

type inlineAgentDecodeError struct {
	field        string
	line, column int
	cause        error
}

func (e *inlineAgentDecodeError) Error() string {
	return fmt.Sprintf("%s (line %d, column %d): %v", e.field, e.line, e.column, e.cause)
}

func (agents *teamAgentDeclarations) UnmarshalYAML(node *yaml.Node) error {
	fields, err := manifestMapping(node, make(map[*yaml.Node]bool))
	if err != nil {
		return &inlineAgentDecodeError{field: "agents", line: node.Line, column: node.Column, cause: err}
	}
	result := make(teamAgentDeclarations, len(fields))
	seen := make(map[string]string)
	for _, id := range slices.Sorted(maps.Keys(fields)) {
		pair := fields[id]
		fail := func(err error) error {
			return &inlineAgentDecodeError{field: "agents." + id, line: pair[0].Line, column: pair[0].Column, cause: err}
		}
		if pair[0].Tag != "!!str" || !inferredAgentNamePattern.MatchString(id) {
			return fail(fmt.Errorf("invalid worker identifier; use a letter followed by letters, digits, '_' or '-'"))
		}
		key := normalizedName(id)
		if prior, exists := seen[key]; exists {
			return fail(fmt.Errorf("duplicate worker identity %q and %q", prior, id))
		}
		seen[key] = id
		declaration, err := decodeInlineAgent(pair[1])
		if err != nil {
			if located, ok := errors.AsType[*inlineAgentDecodeError](err); ok {
				copy := *located
				copy.field = "agents." + id + "." + located.field
				return &copy
			}
			return fail(err)
		}
		result[id] = declaration
	}
	*agents = result
	return nil
}

func (agents teamAgentDeclarations) MarshalYAML() (any, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, id := range slices.Sorted(maps.Keys(agents)) {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: id}, agents[id].node)
	}
	return root, nil
}

func decodeInlineAgent(node *yaml.Node) (teamAgentAuthoring, error) {
	literal, err := cloneInlineAuthoringNode(node, make(map[*yaml.Node]bool))
	if err != nil {
		return teamAgentAuthoring{}, err
	}
	result := teamAgentAuthoring{node: literal}
	if literal.Kind == yaml.ScalarNode {
		if literal.Tag != "!!str" || strings.TrimSpace(literal.Value) == "" {
			return result, fmt.Errorf("expected non-empty Agent Preset string or mapping")
		}
		result.frontmatter.Preset = literal.Value
		return result, nil
	}
	if literal.Kind != yaml.MappingNode {
		return result, fmt.Errorf("expected Agent Preset string or mapping")
	}
	fields, err := manifestMapping(literal, make(map[*yaml.Node]bool))
	if err != nil {
		return result, err
	}
	for _, field := range slices.Sorted(maps.Keys(fields)) {
		value := fields[field][1]
		fail := func(err error) (teamAgentAuthoring, error) {
			return result, &inlineAgentDecodeError{field: field, line: value.Line, column: value.Column, cause: err}
		}
		switch field {
		case "preset", "description", "instructions", "model", "reasoning-effort":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return fail(fmt.Errorf("%s must be a string", field))
			}
			if (field == "preset" || field == "description" || field == "instructions") && strings.TrimSpace(value.Value) == "" {
				return fail(fmt.Errorf("%s must not be empty", field))
			}
		case "skills", "allowed-paths":
			if err := validateInlineStringList(value, false); err != nil {
				return fail(fmt.Errorf("%s: %w", field, err))
			}
		case "tools":
			if err := validateInlineTools(value); err != nil {
				return fail(fmt.Errorf("tools: %w", err))
			}
		case "timeout", "max-steps", "max-retries":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!int" {
				return fail(fmt.Errorf("%s must be a non-negative integer", field))
			}
			var number int64
			if err := value.Decode(&number); err != nil || number < 0 {
				return fail(fmt.Errorf("%s must be a non-negative integer within range", field))
			}
			if field != "timeout" {
				var count int
				if err := value.Decode(&count); err != nil {
					return fail(fmt.Errorf("%s: %w", field, err))
				}
			}
		case "no-net":
			if value.Kind != yaml.ScalarNode || value.Tag != "!!bool" {
				return fail(fmt.Errorf("no-net must be a boolean"))
			}
		default:
			return fail(fmt.Errorf("unsupported inline field %q; use Markdown for advanced Agent contracts", field))
		}
	}
	if _, exists := fields["preset"]; !exists {
		return result, fmt.Errorf("preset is required")
	}
	if err := literal.Decode(&result.frontmatter); err != nil {
		return result, err
	}
	if pair, exists := fields["instructions"]; exists {
		result.instructions = pair[1].Value
		result.hasInstructions = true
	}
	return result, nil
}

func validateInlineStringList(node *yaml.Node, tools bool) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return fmt.Errorf("expected string or string list")
		}
		if tools {
			for name := range strings.SplitSeq(node.Value, ",") {
				if strings.TrimSpace(name) == "" || strings.EqualFold(strings.TrimSpace(name), "all") {
					return fmt.Errorf("tools must be named and non-empty; wildcard 'all' is not supported")
				}
			}
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
				return fmt.Errorf("list items must be strings")
			}
			if tools {
				if err := validateInlineStringList(item, true); err != nil {
					return err
				}
			}
		}
	default:
		return fmt.Errorf("expected string or string list")
	}
	return nil
}

func validateInlineTools(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return validateInlineStringList(node, true)
	}
	fields, err := manifestMapping(node, make(map[*yaml.Node]bool))
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if name != "allowed" && name != "denied" {
			return fmt.Errorf("unknown tools field %q; use allowed or denied", name)
		}
		if err := validateInlineStringList(fields[name][1], true); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// Expand aliases into self-contained declarations before migration serializes them.
// This keeps literal template values and field presence without dangling anchors.
func cloneInlineAuthoringNode(node *yaml.Node, active map[*yaml.Node]bool) (*yaml.Node, error) {
	if node == nil || active[node] {
		return nil, fmt.Errorf("cyclic or missing YAML declaration")
	}
	active[node] = true
	defer delete(active, node)
	if node.Kind == yaml.AliasNode {
		return cloneInlineAuthoringNode(node.Alias, active)
	}
	copy := *node
	copy.Anchor = ""
	copy.Alias = nil
	copy.Content = nil
	if node.Kind == yaml.MappingNode {
		fields, err := manifestMapping(node, make(map[*yaml.Node]bool))
		if err != nil {
			return nil, err
		}
		for _, name := range slices.Sorted(maps.Keys(fields)) {
			pair := fields[name]
			if pair[0].Tag != "!!str" {
				return nil, fmt.Errorf("mapping keys must be strings")
			}
			value, err := cloneInlineAuthoringNode(pair[1], active)
			if err != nil {
				return nil, err
			}
			copy.Content = append(copy.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, value)
		}
	} else {
		for _, child := range node.Content {
			value, err := cloneInlineAuthoringNode(child, active)
			if err != nil {
				return nil, err
			}
			copy.Content = append(copy.Content, value)
		}
	}
	return &copy, nil
}

func teamManifestDecodeError(file, prefix string, err error) error {
	return fmt.Errorf("failed to parse team config: %w", teamManifestInlineError(file, prefix, err))
}

func teamManifestInlineError(file, prefix string, err error) error {
	if located, ok := errors.AsType[*inlineAgentDecodeError](err); ok {
		copy := *located
		copy.field = prefix + copy.field
		return fmt.Errorf("%s: %w", file, &copy)
	}
	return err
}
