package tools

import (
	"encoding/json"
	"fmt"
	"testing"

	"charm.land/fantasy"
)

func TestRandomToolInfo(t *testing.T) {
	tool := NewRandomTool()
	info := tool.Info()
	if info.Name != "random" {
		t.Errorf("Name = %q, want %q", info.Name, "random")
	}
	if !info.Parallel {
		t.Error("Parallel should be true")
	}
}

func TestRandomStringDefault(t *testing.T) {
	tool := NewRandomTool()
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: "{}"})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if len(result.Content) != 16 {
		t.Errorf("default string length = %d, want 16", len(result.Content))
	}
	for _, c := range result.Content {
		if !isAlphanumeric(c) {
			t.Errorf("unexpected character in default alphanumeric string: %c", c)
		}
	}
}

func TestRandomStringCustomLength(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"string","length":32}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if len(result.Content) != 32 {
		t.Errorf("string length = %d, want 32", len(result.Content))
	}
}

func TestRandomStringHexCharset(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"string","length":8,"charset":"hex"}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	if len(result.Content) != 8 {
		t.Errorf("string length = %d, want 8", len(result.Content))
	}
	for _, c := range result.Content {
		if !isHex(c) {
			t.Errorf("unexpected character in hex string: %c", c)
		}
	}
}

func TestRandomStringAlphaCharset(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"string","length":20,"charset":"alpha"}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	for _, c := range result.Content {
		if !isAlpha(c) {
			t.Errorf("unexpected non-alpha character: %c", c)
		}
	}
}

func TestRandomStringNumericCharset(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"string","length":10,"charset":"numeric"}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	for _, c := range result.Content {
		if c < '0' || c > '9' {
			t.Errorf("unexpected non-numeric character: %c", c)
		}
	}
}

func TestRandomStringInvalidCharset(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"string","charset":"invalid"}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for invalid charset")
	}
}

func TestRandomStringLengthTooLarge(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"string","length":2000}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for length too large")
	}
}

func TestRandomIntegerSingle(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"integer","min":0,"max":10}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	var n int
	if _, err := fmt.Sscanf(result.Content, "%d", &n); err != nil {
		t.Fatalf("expected integer output, got %q", result.Content)
	}
	if n < 0 || n > 10 {
		t.Errorf("integer %d out of range [0, 10]", n)
	}
}

func TestRandomIntegerMultiple(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"integer","min":1,"max":100,"count":5}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	var values []int
	if err := json.Unmarshal([]byte(result.Content), &values); err != nil {
		t.Fatalf("expected JSON array, got %q", result.Content)
	}
	if len(values) != 5 {
		t.Errorf("count = %d, want 5", len(values))
	}
	for _, v := range values {
		if v < 1 || v > 100 {
			t.Errorf("integer %d out of range [1, 100]", v)
		}
	}
}

func TestRandomIntegerMinGreaterThanMax(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"integer","min":10,"max":5}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for min > max")
	}
}

func TestRandomIntegerDefaultRange(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"integer"}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	var n int
	if _, err := fmt.Sscanf(result.Content, "%d", &n); err != nil {
		t.Fatalf("expected integer output, got %q", result.Content)
	}
	if n < 0 || n > 100 {
		t.Errorf("integer %d out of default range [0, 100]", n)
	}
}

func TestRandomChoiceSingle(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"choice","items":["red","green","blue"]}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	var choice string
	if err := json.Unmarshal([]byte(result.Content), &choice); err != nil {
		t.Fatalf("expected JSON string, got %q", result.Content)
	}
	valid := map[string]bool{"red": true, "green": true, "blue": true}
	if !valid[choice] {
		t.Errorf("choice %q not in items list", choice)
	}
}

func TestRandomChoiceMultiple(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"choice","items":["a","b","c","d","e"],"count":3}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	var choices []string
	if err := json.Unmarshal([]byte(result.Content), &choices); err != nil {
		t.Fatalf("expected JSON array, got %q", result.Content)
	}
	if len(choices) != 3 {
		t.Errorf("count = %d, want 3", len(choices))
	}
	valid := map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": true}
	for _, c := range choices {
		if !valid[c] {
			t.Errorf("choice %q not in items list", c)
		}
	}
}

func TestRandomChoiceNoDuplicates(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"choice","items":["a","b","c"],"count":3,"allow_duplicates":false}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	var choices []string
	if err := json.Unmarshal([]byte(result.Content), &choices); err != nil {
		t.Fatalf("expected JSON array, got %q", result.Content)
	}
	if len(choices) != 3 {
		t.Errorf("count = %d, want 3", len(choices))
	}
	seen := map[string]bool{}
	for _, c := range choices {
		if seen[c] {
			t.Errorf("duplicate choice %q when allow_duplicates=false", c)
		}
		seen[c] = true
	}
}

func TestRandomChoiceNoDuplicatesExceedsItems(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"choice","items":["a","b"],"count":3,"allow_duplicates":false}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when count > len(items) and allow_duplicates=false")
	}
}

func TestRandomChoiceEmptyItems(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"choice","items":[]}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for empty items")
	}
}

func TestRandomChoiceWithNumbers(t *testing.T) {
	tool := NewRandomTool()
	input := `{"type":"choice","items":[1,2,3,4,5],"count":2}`
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	var choices []interface{}
	if err := json.Unmarshal([]byte(result.Content), &choices); err != nil {
		t.Fatalf("expected JSON array, got %q", result.Content)
	}
	if len(choices) != 2 {
		t.Errorf("count = %d, want 2", len(choices))
	}
}

func TestRandomUniqueness(t *testing.T) {
	tool := NewRandomTool()
	results := map[string]bool{}
	for i := 0; i < 10; i++ {
		result, _ := tool.Run(t.Context(), fantasy.ToolCall{Input: `{}`})
		results[result.Content] = true
	}
	if len(results) < 8 {
		t.Errorf("expected near-unique results, got only %d unique out of 10", len(results))
	}
}

func isAlphanumeric(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isAlpha(c rune) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isHex(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}
