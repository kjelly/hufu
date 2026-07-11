package team

import (
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// Error-type tool results used to render as empty strings in every log view
// (LLM request dump, stream events, audit previews), hiding permission
// denials and command failures from all diagnostics.
func TestToolResultOutputText(t *testing.T) {
	cases := []struct {
		name    string
		output  fantasy.ToolResultOutputContent
		want    string
		wantErr bool
	}{
		{"text result", fantasy.ToolResultOutputContentText{Text: "hello"}, "hello", false},
		{"error result carries message", fantasy.ToolResultOutputContentError{Error: errors.New("tool 'bash' is not permitted")}, "tool 'bash' is not permitted", true},
		{"nil error", fantasy.ToolResultOutputContentError{}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, isErr := toolResultOutputText(tc.output)
			if got != tc.want {
				t.Errorf("text = %q, want %q", got, tc.want)
			}
			if isErr != tc.wantErr {
				t.Errorf("isErr = %v, want %v", isErr, tc.wantErr)
			}
		})
	}
}

func TestFormatToolResultContent_RendersErrors(t *testing.T) {
	got := formatToolResultContent(fantasy.ToolResultContent{
		ToolCallID: "call_1",
		ToolName:   "bash",
		Result:     fantasy.ToolResultOutputContentError{Error: errors.New("exit code 1: no such file")},
	})
	if !strings.Contains(got, "exit code 1: no such file") {
		t.Errorf("error text missing from stream log rendering: %q", got)
	}
	if !strings.Contains(got, `error="true"`) {
		t.Errorf("error attribute missing: %q", got)
	}
}
