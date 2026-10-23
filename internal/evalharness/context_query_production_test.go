package evalharness

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestContextQueryProductionContinuationReceiverUsesItsOwnManifest(t *testing.T) {
	suite, c, _ := pairedQueryFixture(t, "constraints", "")
	activateFixtureContextQuery(t, suite)
	c.Expect.Tasks = nil
	c.Expect.TaskCount = new(0)
	c.Expect.ProviderCallCount = new(2)
	c.Expect.ContextDelivery = []ContextDeliveryExpect{
		{Agent: "coordinator", Purpose: "coordinator_start", Include: []string{"current_task"}, RequiredFragments: map[string]string{"current_task": "inspect router"}},
		{Agent: "coordinator", Purpose: "coordinator_continuation", Include: []string{"current_task"}, RequiredFragments: map[string]string{"current_task": "inspect next router"}},
	}
	provider := newScriptedProvider(&ProviderFixture{Steps: []ProviderStep{
		{ToolCall: &ToolCallStep{Name: "finish", Arguments: `{"response":"first complete"}`}},
		{ToolCall: &ToolCallStep{Name: "finish", Arguments: `{"response":"second complete"}`}},
	}})
	var manifests []team.ContextInjectionManifest
	result, err := runCaseWithExecution(t.Context(), suite, c, provider, provider.scriptFindings, func(ctx context.Context, coordinator *team.Coordinator, prompt string) error {
		if _, err := coordinator.Run(ctx, prompt); err != nil {
			if result := coordinator.LastRunResult(); result == nil || result.Outcome != team.RunOutcomeUnverified {
				return err
			}
		}
		_, err := coordinator.ContinueWithPrompt(ctx, "inspect next router")
		manifests = coordinator.SessionData().CoordinatorContextManifests
		return err
	})
	if err != nil || !result.Passed {
		t.Fatalf("continuation: %v %+v", err, result.Findings)
	}
	requests := result.Metrics.ContextDelivery.Requests
	if len(requests) != 2 {
		t.Fatalf("receiver streams=%d", len(requests))
	}
	if len(manifests) != 2 || manifests[0].RequestID == manifests[1].RequestID {
		t.Fatal("continuation reused old request identity")
	}
	for _, request := range requests {
		if !request.ManifestMatched || request.ManifestSchema != 3 || request.QueryAttribution == nil {
			t.Fatal("continuation lost exact active manifest binding")
		}
	}
}

// Copy the team so production activation never changes a repository team.
func activateFixtureContextQuery(t testing.TB, suite *SuiteFixture) {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(suite.TeamDir())); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "team.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		path = filepath.Join(dir, "team.yml")
		data, err = os.ReadFile(path)
	}
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\ncontext-routing:\n  query-mode: active\n")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	suite.Team = dir
}

func TestContextQueryProductionExistingLifecycleSuites(t *testing.T) {
	for _, suite := range []string{"core-lifecycle", "retry-recovery", "memory-learning", "memory-fresh-session", "memory-fresh-session-off", "plan-first-dependency", "verbatim-dependency", "legacy-execution-replay"} {
		t.Run(suite, func(t *testing.T) {
			fixture, err := LoadSuiteFixture(filepath.Join(repoRoot(t), "evals", suite, "cases.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			activateFixtureContextQuery(t, fixture)
			for index := range fixture.Cases {
				fixture.Cases[index].Expect.ContextDelivery = []ContextDeliveryExpect{{Agent: "coordinator", Purpose: "coordinator_start", Include: []string{"current_task"}, RequiredFragments: map[string]string{"current_task": fixture.Cases[index].Prompt}}}
			}
			result, err := RunSuite(t.Context(), fixture, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range result.Cases {
				if !c.Passed {
					t.Errorf("%s: %+v", c.CaseID, c.Findings)
				}
				if c.Metrics.ContextDelivery == nil || len(c.Metrics.ContextDelivery.Requests) == 0 {
					t.Errorf("%s: missing provider receiver", c.CaseID)
					continue
				}
				for _, request := range c.Metrics.ContextDelivery.Requests {
					if !request.ManifestMatched || request.ManifestSchema != 3 || request.QueryAttribution == nil {
						t.Errorf("%s: active policy was not compiled at provider boundary", c.CaseID)
					}
				}
			}
		})
	}
}
