package team

import (
	"context"
	"strings"
	"testing"
)

func TestParseJudgeVerdict(t *testing.T) {
	cases := []struct {
		name       string
		response   string
		candidates int
		wantBest   int
		wantGrafts int
		wantErr    bool
	}{
		{
			name:       "plain JSON",
			response:   `{"best_index": 1, "reason": "more complete"}`,
			candidates: 2,
			wantBest:   1,
		},
		{
			name:       "fenced JSON",
			response:   "Here is my verdict:\n```json\n{\"best_index\": 0, \"reason\": \"correct\"}\n```",
			candidates: 2,
			wantBest:   0,
		},
		{
			name:       "valid graft kept",
			response:   `{"best_index": 0, "reason": "r", "grafts": [{"from_index": 1, "content": "extra detail"}]}`,
			candidates: 2,
			wantBest:   0,
			wantGrafts: 1,
		},
		{
			name:       "out-of-range graft dropped",
			response:   `{"best_index": 0, "reason": "r", "grafts": [{"from_index": 5, "content": "x"}]}`,
			candidates: 2,
			wantBest:   0,
			wantGrafts: 0,
		},
		{
			name:       "graft from winner dropped",
			response:   `{"best_index": 0, "reason": "r", "grafts": [{"from_index": 0, "content": "x"}]}`,
			candidates: 2,
			wantBest:   0,
			wantGrafts: 0,
		},
		{
			name:       "best_index out of range",
			response:   `{"best_index": 3, "reason": "r"}`,
			candidates: 2,
			wantErr:    true,
		},
		{
			name:       "negative best_index",
			response:   `{"best_index": -1, "reason": "r"}`,
			candidates: 2,
			wantErr:    true,
		},
		{
			name:       "malformed JSON",
			response:   `the best one is candidate 1`,
			candidates: 2,
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseJudgeVerdict(tc.response, tc.candidates)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", v)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if v.BestIndex != tc.wantBest {
				t.Errorf("BestIndex = %d, want %d", v.BestIndex, tc.wantBest)
			}
			if len(v.Grafts) != tc.wantGrafts {
				t.Errorf("Grafts = %d, want %d", len(v.Grafts), tc.wantGrafts)
			}
		})
	}
}

func TestBuildJudgePrompt(t *testing.T) {
	candidates := []*agentResult{
		{model: "model-a", output: "output A"},
		{model: "model-b", output: strings.Repeat("y", judgeCandidateMaxRunes+500)},
	}
	prompt := buildJudgePrompt("achieve the goal", candidates)

	for _, want := range []string{"achieve the goal", "Candidate 0 (model: model-a)", "output A", "Candidate 1 (model: model-b)", "best_index"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if len([]rune(prompt)) > 2*judgeCandidateMaxRunes {
		t.Errorf("candidate truncation not applied: prompt is %d runes", len([]rune(prompt)))
	}
}

func TestComposeJudgedOutput(t *testing.T) {
	candidates := []*agentResult{
		{model: "model-a", output: "winner output"},
		{model: "model-b", output: "loser output"},
	}

	got := composeJudgedOutput(candidates, judgeVerdict{BestIndex: 0, Reason: "clearer"})
	if !strings.HasPrefix(got, "winner output") || !strings.Contains(got, "selected model-a") {
		t.Errorf("winner-only output wrong:\n%s", got)
	}
	if strings.Contains(got, "loser output") {
		t.Errorf("runner-up leaked into winner-only output:\n%s", got)
	}

	got = composeJudgedOutput(candidates, judgeVerdict{
		BestIndex: 0,
		Reason:    "clearer",
		Grafts:    []judgeGraft{{FromIndex: 1, Content: "useful bit"}},
	})
	if !strings.Contains(got, "## Grafted from model-b") || !strings.Contains(got, "useful bit") {
		t.Errorf("graft missing:\n%s", got)
	}
}

func TestJudgeAgentResultsFallsBackWithoutJudge(t *testing.T) {
	c := &Coordinator{reportStatus: func(StatusEvent) {}}
	results := []*agentResult{
		{model: "a", output: "out a"},
		{model: "b", output: "out b"},
	}

	_, err := c.judgeAgentResults(context.Background(), "goal", "1", results)
	if err == nil {
		t.Fatal("expected error when no judge model configured")
	}

	// The caller's fallback path must still produce the concatenation merge.
	merged := c.mergeAgentResults(results)
	if !strings.Contains(merged, "out a") || !strings.Contains(merged, "out b") {
		t.Errorf("merge fallback missing outputs:\n%s", merged)
	}
}

func TestExtractJSONPayload(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"prose\n```\n{\"a\":1}\n```\nmore", `{"a":1}`},
		{"  {\"a\":1}  ", `{"a":1}`},
	}
	for _, tc := range cases {
		if got := extractJSONPayload(tc.in); got != tc.want {
			t.Errorf("extractJSONPayload(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
