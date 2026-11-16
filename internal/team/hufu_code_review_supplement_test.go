package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// The fixture subprocess calls the team's real materializer, rather than
// synthesizing a manifest that could drift from the producer's empty path.
type supplementFixtureProvider struct {
	binary    string
	kind      string
	workspace string
	calls     int
	outputs   map[string]any
}

func (*supplementFixtureProvider) Validate(Action) error { return nil }

func (p *supplementFixtureProvider) Execute(ctx context.Context, _ Action) (any, error) {
	p.calls++
	p.workspace = ActionEnvironmentFromContext(ctx).Workspace
	cmd := exec.CommandContext(ctx, p.binary, "-test.run=^TestSupplementRuntimeActionFixture$", "-test.count=1")
	cmd.Env = append(os.Environ(), "HUFU_TEST_SUPPLEMENT_OUTPUT="+p.workspace, "HUFU_TEST_SUPPLEMENT_CASE="+p.kind)
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("producer fixture: %w: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(p.workspace, "fixture-response.json"))
	if err != nil {
		return nil, err
	}
	var response any
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	p.outputs = response.(map[string]any)["outputs"].(map[string]any)
	return response, nil
}

func TestHufuCodeReviewSupplementPublishesThroughRuntimeProjection(t *testing.T) {
	// One binary serves the complete table, then the parent removes it after
	// every subprocess has exited. Keep it under the task's private TMPDIR.
	scratch, err := os.MkdirTemp(os.TempDir(), "supplement-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	binary := filepath.Join(scratch, "producer-fixture")
	build := exec.CommandContext(t.Context(), "go", "test", "-c", "-p", "2", "-o", binary, "./.agent-teams/hufu-code-review/reviewprep")
	build.Dir = filepath.Dir(bundledTeamsDir(t))
	build.Env = append(os.Environ(), "GOMAXPROCS=2")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build producer fixture: %v\n%s", err, output)
	}
	for _, test := range []struct {
		name            string
		items, outcomes int
	}{
		{"no_requests", 0, 0}, {"no_paths", 0, 1}, {"unavailable", 0, 1}, {"budget", 0, 1}, {"mixed", 1, 2}, {"valid", 1, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := workflowTestSession(t)
			c := newTestCoordinator(t, withTeamConfig(func(config *agent.TeamConfig) { *config = base.Config }))
			c.session.Agents, c.session.ContractTasks = base.Agents, base.ContractTasks
			c.session.Dir = t.TempDir()
			c.projectDir = c.session.Dir
			c.executionRunID = "run-supplement-" + test.name
			provider := &supplementFixtureProvider{binary: binary, kind: test.name}
			c.session.ProviderRegistry = NewProviderRegistry()
			c.session.ProviderRegistry.Register("structured-actions", provider)
			workflow, err := newRuntimeWorkflow(c.session)
			if err != nil {
				t.Fatal(err)
			}
			if err := workflow.Start(); err != nil {
				t.Fatal(err)
			}
			if err := workflow.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
				t.Fatal(err)
			}
			if err := workflow.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
				t.Fatal(err)
			}
			c.phaseWorkflow = workflow
			store, err := NewEventStore(c.session.Workspace, c.executionRunID, "session-supplement")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			c.eventStore = store
			c.SetEventJournal(eventStoreJournal{store: store})
			task, item := createAdmittedTestTask(t, c, TaskDef{ID: "execute", Agent: "executor", Goal: "prepare bounded evidence", Phase: PhaseExecute, SideEffect: SideEffectNone, Action: &Action{Capability: "structured-actions", Type: "prepare_review_supplement"}})
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if _, err := c.executeTask(ctx, task, item.ID); err != nil {
				t.Fatalf("native supplement publication: %v", err)
			}
			got := todoByID(t, c, item.ID)
			if got.Status != TaskDone || got.TypedResult == nil || provider.calls != 1 {
				t.Fatalf("action did not finish exactly once: %+v, calls=%d", got, provider.calls)
			}
			outputs := got.TypedResult.RuntimeOutputs
			_, expectedHash, err := CanonicalizeRuntimeOutputs(provider.outputs)
			if err != nil || got.TypedResult.RuntimeOutputsHash != expectedHash {
				t.Fatal("native acceptance changed producer outcomes or limitations")
			}
			if !equalJSONValues(outputs["item_count"], test.items) || !equalJSONValues(outputs["supplement_prepared"], true) {
				t.Fatalf("publication lost preparation outcome: %v", outputs)
			}
			var outcomes []struct {
				WorksetKey  string `json:"workset_key"`
				Limitations []any  `json:"limitations"`
			}
			outcomeData, err := json.Marshal(outputs["supplement_outcomes"])
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(outcomeData, &outcomes); err != nil {
				t.Fatal(err)
			}
			if len(outcomes) != test.outcomes {
				t.Fatal("publication dropped original request outcomes")
			}
			for _, outcome := range outcomes {
				if outcome.WorksetKey == "" && len(outcome.Limitations) == 0 {
					t.Fatal("unmaterialized gap lost its limitations")
				}
			}
			manifests := 0
			for _, ref := range got.TypedResult.Artifacts {
				if ref.Kind == "workset_manifest" {
					manifests++
				}
			}
			pointer, err := os.ReadFile(filepath.Join(provider.workspace, "workset", "summary.json"))
			if test.items == 0 {
				if manifests != 0 || !errors.Is(err, os.ErrNotExist) {
					t.Fatal("empty supplementation published an executable workset")
				}
			} else {
				var parsed RuntimeWorksetPointer
				if err != nil || json.Unmarshal(pointer, &parsed) != nil || manifests != 1 || parsed.ItemCount != test.items {
					t.Fatalf("nonempty workset lost projection: %s, %v", pointer, err)
				}
			}
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, replayed := range ReduceToTodoList(events) {
				if replayed.ID != item.ID {
					continue
				}
				found = true
				if replayed.Status != TaskDone || replayed.TypedResult == nil || replayed.TypedResult.RuntimeOutputsHash != got.TypedResult.RuntimeOutputsHash {
					t.Fatal("canonical replay changed accepted supplement output")
				}
			}
			if !found {
				t.Fatal("completed native action missing from canonical replay")
			}
		})
	}
}
