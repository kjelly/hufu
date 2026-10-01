package team

import (
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"gopkg.in/yaml.v3"
)

func TestRecoveryStrategyFoldsCaseWhenDecoded(t *testing.T) {
	var fromJSON RecoveryHypothesis
	if err := json.Unmarshal([]byte(`{"strategy":" Human "}`), &fromJSON); err != nil {
		t.Fatal(err)
	}
	var fromYAML RecoveryHypothesis
	if err := yaml.Unmarshal([]byte("strategy: Retry\n"), &fromYAML); err != nil {
		t.Fatal(err)
	}
	if fromJSON.Strategy != RecoveryStrategyHuman || fromYAML.Strategy != RecoveryStrategyRetry {
		t.Fatalf("strategies = %q, %q", fromJSON.Strategy, fromYAML.Strategy)
	}
	hypothesis := RecoveryHypothesis{ObservedFailure: "o", HypothesizedCause: "c", ProposedChange: "p", ExpectedChange: "e", DifferenceFromPrior: "d"}
	if err := json.Unmarshal([]byte(`{"observed_failure":"o","hypothesized_cause":"c","proposed_change":"p","expected_change":"e","difference_from_prior":"d","strategy":"RETRY"}`), &hypothesis); err != nil {
		t.Fatal(err)
	}
	if err := hypothesis.Validate(true, RecoveryStrategyRetry); err == nil {
		t.Fatal("RETRY after retry was accepted as a different strategy")
	}
}

func TestProposedOptionKindFoldsSpelling(t *testing.T) {
	tests := map[string]DecisionOptionKind{
		"Defer":               OptionDefer,
		" ABANDON ":           OptionAbandon,
		"reduce-scope":        OptionReduceScope,
		"Request_Information": OptionRequestInfo,
		"something-else":      DecisionOptionKind("something_else"),
	}
	for raw, want := range tests {
		if got := proposedOptionKind(raw); got != want {
			t.Errorf("proposedOptionKind(%q) = %q, want %q", raw, got, want)
		}
	}
	if !proposedOptionKind("DEFER").IsNoGo() {
		t.Fatal("an upper-case defer option no longer counts as a no-go alternative")
	}
}

func TestTodoToolFoldsActionAndStatus(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "researcher", Desc: "find bugs"}})
	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = "researcher" })
	tool := &todoTool{coordinator: c}
	response, err := tool.Run(t.Context(), fantasy.ToolCall{Name: "todo", Input: `{"action":"UPDATE","id":"1","status":"In_Progress"}`})
	if err != nil || response.IsError {
		t.Fatalf("response = %+v, %v", response, err)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskInProgress {
		t.Fatalf("status = %s, want in_progress", got)
	}
	if response, _ := tool.Run(t.Context(), fantasy.ToolCall{Name: "todo", Input: `{"action":"List"}`}); response.IsError {
		t.Fatalf("List rejected: %s", response.Content)
	}
}

func TestTerminalArgsFoldVocabulary(t *testing.T) {
	args, err := (&terminalTool{}).parseArgs(`{"action":"List","filter":" ALL ","target":"Exit"}`)
	if err != nil {
		t.Fatal(err)
	}
	if args.Action != "list" || args.Filter != "all" || args.Target != "exit" {
		t.Fatalf("args = %+v", args)
	}
}
