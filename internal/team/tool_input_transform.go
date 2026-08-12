package team

import (
	"encoding/json"
	"fmt"
	"strings"
)

const nestedMenuPreambleTransform = "nested-menu-preamble-v1"

const terminalLastFindingTranscriptAckTransform = "terminal-last-finding-transcript-ack-v1"

func knownToolInputTransform(name string) bool {
	return name == nestedMenuPreambleTransform || name == terminalLastFindingTranscriptAckTransform
}

func transformTaskToolInput(transform, tool, input string) (string, bool, error) {
	switch transform {
	case nestedMenuPreambleTransform:
		if tool != "write" {
			return input, false, fmt.Errorf("transform %q requires write, got %q", transform, tool)
		}
		return canonicalizeNestedMenuWrite(input)
	case terminalLastFindingTranscriptAckTransform:
		if tool != "submit_result" {
			return input, false, fmt.Errorf("transform %q requires submit_result, got %q", transform, tool)
		}
		return canonicalizeTerminalLastFindingAcknowledgement(input)
	default:
		return input, false, fmt.Errorf("unsupported transform %q", transform)
	}
}

func canonicalizeTerminalLastFindingAcknowledgement(input string) (string, bool, error) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		return input, false, fmt.Errorf("submit_result input is not JSON: %w", err)
	}
	findings, ok := payload["findings"].([]any)
	if !ok || len(findings) == 0 {
		return input, false, fmt.Errorf("submit_result requires a non-empty findings array")
	}
	for i, finding := range findings {
		object, ok := finding.(map[string]any)
		if !ok || len(object) != 1 {
			return input, false, fmt.Errorf("finding %d must be a one-key object before transcript acknowledgement", i+1)
		}
		if _, ok := object["summary"].(string); !ok {
			return input, false, fmt.Errorf("finding %d must contain string summary before transcript acknowledgement", i+1)
		}
	}
	last := findings[len(findings)-1].(map[string]any)
	acknowledgement := fmt.Sprintf("slot_%d ordered evidence retained in sealed transcript", len(findings))
	if last["summary"] == acknowledgement {
		return input, false, nil
	}
	last["summary"] = acknowledgement
	payload["findings"] = findings
	encoded, err := json.Marshal(payload)
	if err != nil {
		return input, false, fmt.Errorf("encode terminal transcript acknowledgement: %w", err)
	}
	return string(encoded), true, nil
}

func canonicalizeNestedMenuWrite(input string) (string, bool, error) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		return input, false, fmt.Errorf("write input is not JSON: %w", err)
	}
	content, ok := payload["content"].(string)
	if !ok {
		return input, false, fmt.Errorf("write input requires string content")
	}
	values, err := nestedMenuDirectiveValues(content)
	if err != nil {
		return input, false, err
	}
	prelude := []string{
		"EXPECT " + values["parent_menu_anchor"],
		"ACTIVATE " + values["parent_menu_selector"] + " WITH ENTER",
		"EXPECT " + values["child_menu_anchor"],
		"ACTIVATE " + values["child_menu_selector"] + " WITH ENTER",
		"EXPECT " + values["post_action_guard"],
	}
	lines := strings.Split(content, "\n")
	firstExecutable := len(lines)
	for i, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			firstExecutable = i
			break
		}
	}
	start, end := firstExecutable, firstExecutable-1
	parentExpect := prelude[0]
	postExpect := prelude[4]
	for i := firstExecutable; i < len(lines); i++ {
		if lines[i] != parentExpect {
			continue
		}
		start = i
		for j := i + 1; j < len(lines); j++ {
			if lines[j] == postExpect {
				end = j
				break
			}
		}
		if end < start {
			return input, false, fmt.Errorf("legacy nested-menu preamble starts at executable line %d but lacks post_action_guard", i+1)
		}
		break
	}
	updated := make([]string, 0, len(lines)+len(prelude))
	updated = append(updated, lines[:start]...)
	updated = append(updated, prelude...)
	updated = append(updated, lines[end+1:]...)
	canonical := strings.Join(updated, "\n")
	if strings.HasSuffix(content, "\n") && !strings.HasSuffix(canonical, "\n") {
		canonical += "\n"
	}
	payload["content"] = canonical
	encoded, err := json.Marshal(payload)
	if err != nil {
		return input, false, fmt.Errorf("encode canonical write input: %w", err)
	}
	return string(encoded), canonical != content, nil
}

func nestedMenuDirectiveValues(content string) (map[string]string, error) {
	keys := []string{"parent_menu_anchor", "parent_menu_selector", "child_menu_anchor", "child_menu_selector", "post_action_guard"}
	values := make(map[string]string, len(keys))
	for _, line := range strings.Split(content, "\n") {
		for _, key := range keys {
			prefix := "# HUFU_NESTED_MENU_V1 " + key + "="
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			if _, exists := values[key]; exists {
				return nil, fmt.Errorf("nested-menu metadata repeats %s", key)
			}
			value := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if value == "" {
				return nil, fmt.Errorf("nested-menu metadata %s is empty", key)
			}
			values[key] = value
		}
	}
	for _, key := range keys {
		if values[key] == "" {
			return nil, fmt.Errorf("nested-menu metadata %s is missing", key)
		}
	}
	if values["parent_menu_anchor"] == values["child_menu_anchor"] || values["parent_menu_selector"] == values["child_menu_selector"] {
		return nil, fmt.Errorf("nested-menu parent and child facts must be distinct")
	}
	return values, nil
}
