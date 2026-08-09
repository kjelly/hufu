package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

func TestNextStrongerModel(t *testing.T) {
	list := []config.ModelEntry{{ID: "fast"}, {ID: "mid"}, {ID: "strong"}}

	cases := []struct {
		name    string
		list    []config.ModelEntry
		current string
		want    string
	}{
		{name: "empty list", list: nil, current: "fast", want: ""},
		{name: "single entry", list: list[:1], current: "fast", want: ""},
		{name: "first escalates to second", list: list, current: "fast", want: "mid"},
		{name: "middle escalates to next", list: list, current: "mid", want: "strong"},
		{name: "strongest stays", list: list, current: "strong", want: ""},
		{name: "unknown model", list: list, current: "other", want: ""},
		{name: "empty current", list: list, current: "", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextStrongerModel(tc.list, tc.current); got != tc.want {
				t.Errorf("nextStrongerModel(%q) = %q, want %q", tc.current, got, tc.want)
			}
		})
	}
}

func TestTaskEscalationEnabled(t *testing.T) {
	cases := []struct {
		name         string
		task         TaskDef
		cfg          *agent.TeamConfig
		modelListLen int
		want         bool
	}{
		{name: "task flag", task: TaskDef{Escalate: true}, modelListLen: 2, want: true},
		{name: "team flag", cfg: &agent.TeamConfig{EscalateOnRetry: true}, modelListLen: 2, want: true},
		{name: "no flags", modelListLen: 2, want: false},
		{name: "too few models", task: TaskDef{Escalate: true}, modelListLen: 1, want: false},
		{name: "nil config no task flag", modelListLen: 3, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskEscalationEnabled(tc.task, tc.cfg, tc.modelListLen); got != tc.want {
				t.Errorf("taskEscalationEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEscalateTaskModelForRetry(t *testing.T) {
	list := []config.ModelEntry{{ID: "fast"}, {ID: "strong"}}
	c := &Coordinator{
		session:   &TeamSession{Config: agent.TeamConfig{Name: "test"}},
		modelList: list,
	}

	// Explicit task model escalates to the next entry.
	if got := c.escalateTaskModelForRetry(TaskDef{Agent: "a", Model: "fast", Escalate: true}); got != "strong" {
		t.Errorf("expected escalation to strong, got %q", got)
	}
	// Already strongest: no escalation.
	if got := c.escalateTaskModelForRetry(TaskDef{Agent: "a", Model: "strong", Escalate: true}); got != "" {
		t.Errorf("expected no escalation from strongest, got %q", got)
	}
	// Escalation disabled: no-op.
	if got := c.escalateTaskModelForRetry(TaskDef{Agent: "a", Model: "fast"}); got != "" {
		t.Errorf("expected no escalation without flag, got %q", got)
	}
}
