package team

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
)

func strategyTestItem() *TodoItem {
	return &TodoItem{
		ID: "7", Agent: "implementer", Goal: "Fix the parser", Desc: "Fix the parser",
		ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: "minimax"},
		SideEffect:      SideEffectWorkspaceWrite, Verify: "go test ./parser",
		DependsOn: []string{"3"}, ContextFiles: []string{"parser/lex.go"},
	}
}

func strategyTestAgents(id string) string {
	return map[string]string{"3": "researcher", "4": "tester", "13": "researcher"}[id]
}

func strategyTestReceipt(tools ...string) *ExecutionReceipt {
	return &ExecutionReceipt{ToolSequence: &ToolSequenceRecord{Tools: tools}}
}

// TestMateriallyDifferentStrategy covers which changes between a failed
// attempt and its replacement count as a different strategy.
func TestMateriallyDifferentStrategy(t *testing.T) {
	cases := []struct {
		name string
		// edit changes the candidate (and its receipt) relative to the failed task.
		edit        func(candidate *TodoItem) *ExecutionReceipt
		wantChanged []StrategyDimension
		wantUnknown []StrategyDimension
	}{
		{
			name: "strategy name and prose alone are not material",
			edit: func(candidate *TodoItem) *ExecutionReceipt {
				candidate.ID, candidate.Goal, candidate.Desc = "12", "Fix the parser another way", "Fix the parser another way"
				candidate.Constraints = "try a different approach"
				candidate.RecoveryHypothesis = &RecoveryHypothesis{Strategy: RecoveryStrategyReflection, DifferenceFromPrior: "try another way"}
				return strategyTestReceipt("view", "bash", "submit_result")
			},
		},
		{
			name:        "a different tool sequence is material",
			edit:        func(*TodoItem) *ExecutionReceipt { return strategyTestReceipt("grep", "edit", "bash", "submit_result") },
			wantChanged: []StrategyDimension{StrategyDimensionToolSequence},
		},
		{
			name:        "an unrecorded tool sequence cannot show a change",
			edit:        func(*TodoItem) *ExecutionReceipt { return nil },
			wantUnknown: []StrategyDimension{StrategyDimensionToolSequence},
		},
		{
			name: "a different execution target is material",
			edit: func(candidate *TodoItem) *ExecutionReceipt {
				candidate.ExecutionTarget = execution.ExecutionTarget{Backend: "ollama", Model: "glm"}
				return strategyTestReceipt("view", "bash", "submit_result")
			},
			wantChanged: []StrategyDimension{StrategyDimensionExecutionTarget},
		},
		{
			name: "a different agent is material",
			edit: func(candidate *TodoItem) *ExecutionReceipt {
				candidate.Agent = "debugger"
				return strategyTestReceipt("view", "bash", "submit_result")
			},
			wantChanged: []StrategyDimension{StrategyDimensionTaskShape},
		},
		{
			name: "depending on another agent's work is material",
			edit: func(candidate *TodoItem) *ExecutionReceipt {
				candidate.DependsOn = []string{"3", "4"}
				return strategyTestReceipt("view", "bash", "submit_result")
			},
			wantChanged: []StrategyDimension{StrategyDimensionDependencyShape},
		},
		{
			name: "the same dependency agents under new todo IDs are not material",
			edit: func(candidate *TodoItem) *ExecutionReceipt {
				candidate.DependsOn = []string{"13"}
				return strategyTestReceipt("view", "bash", "submit_result")
			},
		},
		{
			name: "pointing the worker at other files is material",
			edit: func(candidate *TodoItem) *ExecutionReceipt {
				candidate.ContextFiles = []string{"parser/lex.go", "parser/grammar.go"}
				return strategyTestReceipt("view", "bash", "submit_result")
			},
			wantChanged: []StrategyDimension{StrategyDimensionEvidenceShape},
		},
	}
	failed := strategyTestItem()
	failedFingerprint := strategyFingerprint(failed, strategyTestReceipt("view", "bash", "submit_result"), strategyTestAgents)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := strategyTestItem()
			receipt := tc.edit(candidate)
			fingerprint := strategyFingerprint(candidate, receipt, strategyTestAgents)
			changed, unknown := compareStrategyFingerprints(failedFingerprint, fingerprint)
			if !slices.Equal(changed, tc.wantChanged) || !slices.Equal(unknown, tc.wantUnknown) {
				t.Fatalf("changed=%v unknown=%v, want changed=%v unknown=%v", changed, unknown, tc.wantChanged, tc.wantUnknown)
			}
			if got, want := MateriallyDifferentStrategy(failedFingerprint, fingerprint), len(tc.wantChanged) > 0; got != want {
				t.Fatalf("MateriallyDifferentStrategy = %v, want %v", got, want)
			}
			if len(tc.wantChanged) == 0 && len(tc.wantUnknown) == 0 && fingerprint.Digest != failedFingerprint.Digest {
				t.Fatalf("an unchanged strategy has a different digest: %s vs %s", fingerprint.Digest, failedFingerprint.Digest)
			}
		})
	}
}

// TestStrategyFingerprintExcludesSecrets keeps goal text, arguments, file
// paths, and credentials out of the recorded fingerprint.
func TestStrategyFingerprintExcludesSecrets(t *testing.T) {
	item := strategyTestItem()
	item.Goal = "Deploy with token sk-live-SECRETVALUE"
	item.Constraints = "use password hunter2"
	item.Verify = "curl -H 'Authorization: Bearer SECRETVALUE' https://internal.example"
	item.ContextFiles = []string{"/home/someone/private/notes.txt"}
	item.RecoveryHypothesis = &RecoveryHypothesis{Strategy: RecoveryStrategyReflection, ObservedFailure: "leaked SECRETVALUE", DifferenceFromPrior: "hunter2"}
	raw, err := json.Marshal(strategyFingerprint(item, strategyTestReceipt("bash"), strategyTestAgents))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRETVALUE", "sk-live", "hunter2", "private/notes", "internal.example", "Deploy"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("fingerprint contains %q: %s", secret, raw)
		}
	}
}

// TestStrategyFingerprintReplayStable recomputes the fingerprint from a todo
// and receipt that went through their durable JSON form.
func TestStrategyFingerprintReplayStable(t *testing.T) {
	item, receipt := strategyTestItem(), strategyTestReceipt("view", "bash", "submit_result")
	item.ExecutionRoute = &ExecutionRouteBinding{Name: "fast", Candidates: []execution.ExecutionTarget{item.ExecutionTarget, {Backend: "ollama", Model: "glm"}}}
	item.DynamicToolAuthorization = &DynamicToolAuthorizationSnapshot{Version: 1, Targets: []FrozenDynamicToolTarget{{Name: "search"}}, StaticToolCeiling: []string{"bash", "view"}}
	want := strategyFingerprint(item, receipt, strategyTestAgents)
	var replayedItem TodoItem
	var replayedReceipt ExecutionReceipt
	for _, pair := range []struct {
		from any
		to   any
	}{{item, &replayedItem}, {receipt, &replayedReceipt}} {
		raw, err := json.Marshal(pair.from)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, pair.to); err != nil {
			t.Fatal(err)
		}
	}
	if got := strategyFingerprint(&replayedItem, &replayedReceipt, strategyTestAgents); got != want {
		t.Fatalf("replayed fingerprint = %+v, want %+v", got, want)
	}
}
