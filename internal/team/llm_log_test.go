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

func TestLLMLogStreamFinish(t *testing.T) {
	cases := []struct {
		name     string
		usage    fantasy.Usage
		reqBytes int
		want     []string
		notWant  []string
	}{
		{
			name:     "provider reports usage",
			usage:    fantasy.Usage{InputTokens: 120, OutputTokens: 34},
			reqBytes: 4000,
			want:     []string{"tokens_in=120", "tokens_out=34"},
			notWant:  []string{"est_tokens_in"},
		},
		{
			name:     "zero usage falls back to byte estimate",
			usage:    fantasy.Usage{},
			reqBytes: 4000,
			want:     []string{"tokens_in=0", "est_tokens_in≈1000", "bytes/4 estimate"},
		},
		{
			name:    "zero usage without request size stays plain",
			usage:   fantasy.Usage{},
			want:    []string{"tokens_in=0"},
			notWant: []string{"est_tokens_in"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			llmLogStreamFinish(func(s string) { out.WriteString(s) }, fantasy.FinishReasonStop, tc.usage, tc.reqBytes)
			got := out.String()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("output missing %q, got: %s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("output should not contain %q, got: %s", nw, got)
				}
			}
		})
	}
}

func TestFlattenStatusEntry(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxRunes int
		want     string
	}{
		{"collapses newlines and headings", "deployer: done\n## Step 0\nresult ok", 100, "deployer: done ## Step 0 result ok"},
		{"truncates long entries with ellipsis", strings.Repeat("a", 50), 10, strings.Repeat("a", 10) + "…"},
		{"short entry unchanged", "helper: ok", 220, "helper: ok"},
		{"multibyte runes are not split", strings.Repeat("驗", 8), 4, strings.Repeat("驗", 4) + "…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flattenStatusEntry(tc.in, tc.maxRunes); got != tc.want {
				t.Errorf("flattenStatusEntry(%q, %d) = %q, want %q", tc.in, tc.maxRunes, got, tc.want)
			}
		})
	}
}

func TestToolUsageNotes(t *testing.T) {
	cases := []struct {
		name     string
		tools    string
		wantNote bool
		want     []string
		notWant  []string
	}{
		{"bash with sudo and ssh", "bash,sudo,ssh,view", true, []string{"sudo and ssh", "REJECTS"}, []string{"wait_for"}},
		{"bash with sudo only", "bash, sudo", true, []string{"sudo"}, nil},
		{"bash without privileged tools", "bash,view,grep", false, nil, nil},
		{"no bash or sudo at all", "ssh,view", false, nil, nil},
		{"wait_for with bash gets polling note", "bash,wait_for", true, []string{"wait_for", "polls internally"}, []string{"REJECTS"}},
		{"wait_for with sudo gets polling note", "sudo,wait_for", true, []string{"wait_for"}, nil},
		{"wait_for without shell tools stays silent", "wait_for,view", false, nil, nil},
		{"empty tools means all tools", "", true, []string{"REJECTS", "wait_for"}, nil},
		{"all keyword means all tools", "all", true, []string{"REJECTS", "wait_for"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolUsageNotes(tc.tools)
			if tc.wantNote && got == "" {
				t.Fatalf("toolUsageNotes(%q) = empty, want a note", tc.tools)
			}
			if !tc.wantNote && got != "" {
				t.Fatalf("toolUsageNotes(%q) = %q, want empty", tc.tools, got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("note missing %q, got: %s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("note should not contain %q, got: %s", nw, got)
				}
			}
		})
	}
}

func TestTotalRoundsSurvivesReset(t *testing.T) {
	c := &Coordinator{}
	c.round = 15
	if got := c.totalRounds(); got != 15 {
		t.Fatalf("totalRounds before reset = %d, want 15", got)
	}
	c.resetRoundState()
	if got := c.totalRounds(); got != 15 {
		t.Fatalf("totalRounds after reset = %d, want 15 (baseRounds must absorb prior rounds)", got)
	}
	c.round = 3
	if got := c.totalRounds(); got != 18 {
		t.Fatalf("totalRounds after 3 new rounds = %d, want 18", got)
	}
	if c.finishCalled.Load() {
		t.Error("resetRoundState must clear finishCalled")
	}
}
