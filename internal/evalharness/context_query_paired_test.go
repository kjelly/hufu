package evalharness

import (
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// Each arm uses a real coordinator and a fresh receiver/workspace. Task
// contracts, accepted results and workset receipts supply the runtime hints.
func TestContextQueryPairedProviderDeliveryUsesAuthorizedSignals(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "evals/context-routing/fixtures/query-signals-baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SchemaVersion int    `json:"schema_version"`
		OracleSource  string `json:"oracle_source"`
		Cases         []struct {
			ID                   string   `json:"id"`
			Signal               string   `json:"signal"`
			Hint                 string   `json:"hint"`
			Content              string   `json:"content"`
			LegacyExpectedIDs    []string `json:"legacy_expected_ids"`
			CandidateExpectedIDs []string `json:"candidate_expected_ids"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var report []struct {
		CaseID     string                `json:"case_id"`
		Legacy     ContextRequestMetrics `json:"legacy"`
		Candidate  ContextRequestMetrics `json:"candidate"`
		Production ContextRequestMetrics `json:"production"`
	}
	for _, signal := range fixture.Cases {
		t.Run(signal.ID, func(t *testing.T) {
			suite, c, steps := pairedQueryFixture(t, signal.Signal, signal.Hint)
			c.ContextItems = []ContextItemFixture{{ID: signal.ID, Content: signal.Content}}
			for index := range 8 {
				c.ContextItems = append(c.ContextItems, ContextItemFixture{ID: fmt.Sprintf("noise-%d", index), Content: fmt.Sprintf("inspect router distractor %d", index)})
			}
			c.ContextItems = append(c.ContextItems, ContextItemFixture{ID: "foreign", Content: signal.Content + " forbidden foreign", Scope: "other_worker"})
			c.Expect.ForbiddenContext = []string{"foreign"}
			c.Expect.ContextDelivery[0].Relevant = []string{signal.ID}
			var metrics []ContextRequestMetrics
			for _, profile := range []string{"legacy", "candidate", "team_active"} {
				c.QueryProfileOverride = profile
				if profile == "team_active" {
					c.QueryProfileOverride = ""
					path := filepath.Join(suite.TeamDir(), "team.yaml")
					config, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					config = append(config, []byte("context-routing:\n  query-mode: active\n")...)
					if err := os.WriteFile(path, config, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				c.Expect.ContextDelivery[0].Include, c.Expect.ContextDelivery[0].Exclude = nil, nil
				if profile != "legacy" {
					c.Expect.ContextDelivery[0].Include = []string{signal.ID}
				} else {
					c.Expect.ContextDelivery[0].Exclude = []string{signal.ID}
				}
				provider := newScriptedProvider(&ProviderFixture{Steps: steps})
				result, err := runCaseWithHandler(t.Context(), suite, c, provider, provider.scriptFindings)
				if err != nil || !result.Passed {
					t.Fatalf("%s: err=%v findings=%+v", profile, err, result.Findings)
				}
				if result.Metrics.ContextDelivery == nil || len(result.Metrics.ContextDelivery.Requests) != 1 {
					t.Fatalf("%s missing receiver evidence", profile)
				}
				got := result.Metrics.ContextDelivery.Requests[0]
				if !got.ManifestMatched || got.RequiredDeliveryRecall == nil || *got.RequiredDeliveryRecall != 1 || got.ForbiddenDeliveryCount != 0 {
					t.Fatal("delivery contract failed")
				}
				if profile != "legacy" {
					if got.ManifestSchema != 3 || got.QueryAttribution == nil || got.QueryAttribution.Mode != team.QueryModeActive {
						t.Fatalf("candidate did not bind actual query metadata: %+v", got.QueryAttribution)
					}
				} else if got.QueryAttribution != nil {
					t.Fatal("legacy attribution changed")
				}
				metrics = append(metrics, got)
			}
			if metrics[0].DeliveryRecall == nil || metrics[1].DeliveryRecall == nil || *metrics[0].DeliveryRecall != 0 || *metrics[1].DeliveryRecall != 1 {
				t.Fatal("receiver-visible miss was not repaired")
			}
			if metrics[1].IrrelevantOptionalTokens > metrics[0].IrrelevantOptionalTokens {
				t.Fatalf("optional cost grew: %d -> %d", metrics[0].IrrelevantOptionalTokens, metrics[1].IrrelevantOptionalTokens)
			}
			// Runtime-created workset context IDs include each isolated run's
			// identity. Compare oracle seed IDs and actual token cost; required
			// runtime fragments were independently checked at both receivers.
			seedIDs := func(delivered []string) []string {
				var ids []string
				for _, id := range delivered {
					if slices.ContainsFunc(c.ContextItems, func(item ContextItemFixture) bool { return item.ID == id }) {
						ids = append(ids, id)
					}
				}
				return ids
			}
			if !slices.Equal(seedIDs(metrics[1].DeliveredIDs), seedIDs(metrics[2].DeliveredIDs)) || metrics[2].QueryAttribution == nil || metrics[2].QueryAttribution.Mode != team.QueryModeActive || metrics[1].IrrelevantOptionalTokens != metrics[2].IrrelevantOptionalTokens {
				t.Fatalf("production opt-in differs: offline IDs=%v tokens=%d; production IDs=%v tokens=%d query=%+v", metrics[1].DeliveredIDs, metrics[1].IrrelevantOptionalTokens, metrics[2].DeliveredIDs, metrics[2].IrrelevantOptionalTokens, metrics[2].QueryAttribution)
			}
			if metrics[0].RetrievalCalls == nil || metrics[1].RetrievalCalls == nil || metrics[1].RetrievalCalls.Vector != metrics[0].RetrievalCalls.Vector {
				t.Fatal("candidate increased vector calls or lacks evidence")
			}
			if !slices.Contains(metrics[1].RetrievedIDs, signal.ID) {
				t.Fatal("effective retrieval observation is missing")
			}
			if signal.ID == "constraint-miss" {
				if len(metrics[0].RetrievedIDs) != 8 || len(metrics[1].RetrievedIDs) != 9 || !slices.Contains(metrics[0].RetrievedIDs, "noise-4") || !slices.Contains(metrics[1].RetrievedIDs, "noise-4") || slices.Contains(metrics[1].DeliveredIDs, "noise-4") {
					t.Fatal("retrieval candidates were confused with post-injection delivery")
				}
			}
			t.Logf("delivery_recall=0->1 irrelevant_tokens=%d->%d legacy_calls=%+v candidate_calls=%+v", metrics[0].IrrelevantOptionalTokens, metrics[1].IrrelevantOptionalTokens, metrics[0].RetrievalCalls, metrics[1].RetrievalCalls)
			report = append(report, struct {
				CaseID     string                `json:"case_id"`
				Legacy     ContextRequestMetrics `json:"legacy"`
				Candidate  ContextRequestMetrics `json:"candidate"`
				Production ContextRequestMetrics `json:"production"`
			}{signal.ID, metrics[0], metrics[1], metrics[2]})
		})
	}
	if path := os.Getenv("HUFU_CONTEXT_QUERY_PAIRED_REPORT"); path != "" {
		data, err := json.Marshal(report, json.Deterministic(true))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func pairedQueryFixture(t *testing.T, signal, hint string) (*SuiteFixture, CaseFixture, []ProviderStep) {
	t.Helper()
	dir := t.TempDir()
	config := "name: paired-context-query\nmodel: eval-harness-model\nmemory-learning:\n  mode: observe\n"
	task := map[string]any{"agent": "worker", "goal": "inspect router"}
	workspace := map[string]string{}
	index := 0
	var sourceSteps []ProviderStep
	switch signal {
	case "constraints":
		task["constraints"] = hint
	case "verification":
		task["verify"] = "true # " + hint
	case "dependency":
		task["depends_on"] = []int{0}
		index = 1
		sourceSteps = []ProviderStep{{ToolCall: &ToolCallStep{Name: "submit_result", Arguments: queryFixtureJSON(t, map[string]any{"status": "success", "summary": hint})}}}
	case "paths":
		config += "delegation:\n  bind-task-goal-contracts: true\ntasks:\n  - id: query-source\n    agent: worker\n    verify: 'true'\n  - id: query-workset\n    agent: worker\n    fan_out:\n      source-artifact:\n        task_id: query-source\n        artifact: workset.json\n      goal-template: inspect router\n"
		task["contract_id"] = "query-workset"
		index = 1
		workspace["workset.json"] = queryFixtureJSON(t, team.WorksetManifest{SchemaVersion: 3, Items: []team.WorksetItem{{Key: "one", TouchedPaths: []string{hint}}}})
	default:
		t.Fatal("unknown signal")
	}
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte("---\nname: worker\nrole: worker\ntools: view\nmodel: eval-harness-model\n---\nComplete the assigned task and submit_result.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tasks := []any{task}
	if signal == "dependency" {
		tasks = append([]any{map[string]any{"agent": "worker", "goal": "prepare prerequisite", "verify": "true"}}, tasks...)
	}
	steps := []ProviderStep{{ToolCall: &ToolCallStep{Name: "agent", Arguments: queryFixtureJSON(t, map[string]any{"tasks": tasks})}}}
	if signal == "paths" {
		// Import through an actual accepted producer result. An expansion
		// receipt alone cannot substitute for current artifact provenance.
		steps = []ProviderStep{
			{ToolCall: &ToolCallStep{Name: "agent", Arguments: queryFixtureJSON(t, map[string]any{"tasks": []any{map[string]any{"agent": "worker", "goal": "prepare workset", "contract_id": "query-source"}}})}},
			{ToolCall: &ToolCallStep{Name: "submit_result", Arguments: `{"status":"success","summary":"prepared","artifacts":[{"path":"workset.json","description":"workset.json"}]}`}},
			{ToolCall: &ToolCallStep{Name: "agent", Arguments: queryFixtureJSON(t, map[string]any{"tasks": tasks})}},
		}
	}
	steps = append(steps, sourceSteps...)
	steps = append(steps, ProviderStep{ToolCall: &ToolCallStep{Name: "submit_result", Arguments: `{"status":"success","summary":"inspected"}`}}, ProviderStep{ToolCall: &ToolCallStep{Name: "finish", Arguments: `{"response":"finished"}`}})
	count := len(tasks)
	if signal == "paths" {
		count++
	}
	expected := []TaskExpect{{Agent: "worker", Status: "done"}}
	if signal == "dependency" || signal == "paths" {
		expected = append(expected, TaskExpect{Agent: "worker", Status: "done"})
	}
	clock := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	c := CaseFixture{ID: signal, Prompt: "inspect router", EvaluationTime: &clock, WorkspaceFiles: workspace, Expect: ExpectSpec{TaskCount: &count, Tasks: expected, ContextDelivery: []ContextDeliveryExpect{{Agent: "worker", TaskIndex: &index, Attempt: 1, Step: 1, RequiredFragments: map[string]string{"current_task": "inspect router"}}}}}
	c.Expect.ProviderCallCount = new(len(steps))
	return &SuiteFixture{Version: 1, Name: "paired-query", Team: dir, Mode: "deterministic", Path: filepath.Join(dir, "cases.yaml")}, c, steps
}

func queryFixtureJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
