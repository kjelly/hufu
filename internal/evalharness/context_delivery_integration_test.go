package evalharness

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Reuse the canonical scripts instead of duplicating their task, verification,
// recovery and durable-evidence contracts in a second eval framework.
func TestContextDeliveryExistingLifecycleSuites(t *testing.T) {
	for _, suite := range []string{"core-lifecycle", "retry-recovery", "memory-learning", "memory-fresh-session", "memory-fresh-session-off", "plan-first-dependency", "verbatim-dependency", "legacy-execution-replay"} {
		t.Run(suite, func(t *testing.T) {
			fixture, err := LoadSuiteFixture(filepath.Join(repoRoot(t), "evals", suite, "cases.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			for index := range fixture.Cases {
				fixture.Cases[index].Expect.ContextDelivery = []ContextDeliveryExpect{{
					Agent: "coordinator", Include: []string{"current_task"},
					RequiredFragments: map[string]string{"current_task": fixture.Cases[index].Prompt},
				}}
				var agents []string
				for _, task := range fixture.Cases[index].Expect.Tasks {
					if (task.Status != "done" && task.Status != "error") || slices.Contains(agents, task.Agent) {
						continue
					}
					agents = append(agents, task.Agent)
					fixture.Cases[index].Expect.ContextDelivery = append(fixture.Cases[index].Expect.ContextDelivery, ContextDeliveryExpect{
						Agent: task.Agent, Purpose: "task_execution", Include: []string{"current_task"}, RequiredFragments: map[string]string{"current_task": "## Goal"},
					})
					if task.Attempts != nil && *task.Attempts > 1 {
						fixture.Cases[index].Expect.ContextDelivery = append(fixture.Cases[index].Expect.ContextDelivery, ContextDeliveryExpect{
							Agent: task.Agent, Purpose: "task_retry", Attempt: 2, Include: []string{"current_task", "retry_failure_context"}, RequiredFragments: map[string]string{"current_task": "## Goal"},
						})
					}
				}
			}
			result, err := RunSuite(t.Context(), fixture, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range result.Cases {
				if !c.Passed {
					t.Errorf("%s: %+v", c.CaseID, c.Findings)
				}
				if c.Metrics.ContextDelivery == nil || c.Metrics.ContextDelivery.ProviderCalls == 0 {
					t.Errorf("%s: no receiver evidence", c.CaseID)
				}
			}
		})
	}
}

func TestContextDeliveryExtraModelBaselineRejectsBeforeWorkerProvider(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: eval-extra-context\nmodel: eval-harness-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := "---\nname: worker\nrole: worker\ntools: view\nmodel: qwen3:8b\nextra-models: [qwen3:14b]\n---\nComplete the assigned task and submit_result.\n"
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte(worker), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &SuiteFixture{Version: 1, Name: "extra-context", Team: dir, Mode: "deterministic", Path: filepath.Join(dir, "cases.yaml")}
	provider := newScriptedProvider(&ProviderFixture{Steps: []ProviderStep{
		{ToolCall: &ToolCallStep{Name: "agent", Arguments: `{"tasks":[{"agent":"worker","goal":"inspect router"}]}`}},
		{ToolCall: &ToolCallStep{Name: "finish", Arguments: `{"response":"baseline blocked","acknowledge_failed_tasks":true}`}},
	}})
	c := CaseFixture{ID: "parallel-extra-provider-context", Prompt: "inspect router", Expect: ExpectSpec{RunOutcome: "partial", TaskCount: new(1), Tasks: []TaskExpect{{Agent: "worker", Status: "error"}}, ProviderCallCount: new(2), ContextDelivery: []ContextDeliveryExpect{{Agent: "worker", Include: []string{"current_task"}, RequiredFragments: map[string]string{"current_task": "inspect router"}}}}}
	result, err := runCaseWithHandler(t.Context(), fixture, c, provider, provider.scriptFindings)
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed || !containsDimension(result.Findings, "dual-write") || !containsDimension(result.Findings, "context-delivery[0]") {
		t.Fatalf("known extra-model baseline failure was hidden: %+v", result.Findings)
	}
	if result.Metrics.ContextDelivery.ProviderCalls != 2 || len(result.Metrics.ContextDelivery.Requests) != 0 {
		t.Fatalf("a rejected leaf was counted as delivered: %+v", result.Metrics.ContextDelivery)
	}
	for _, finding := range result.Findings {
		if finding.Dimension != "dual-write" && finding.Dimension != "context-delivery[0]" {
			t.Fatalf("unexpected extra-model baseline failure: %+v", finding)
		}
	}
}
