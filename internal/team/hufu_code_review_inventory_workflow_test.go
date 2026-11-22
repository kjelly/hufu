package team

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	sidecarbackend "github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/tools"
)

const offlineReviewScope = `{"kind":"last_n","count":1,"history":"first_parent","head":"HEAD"}`

func TestHufuCodeReviewCompletesReportFromAcceptedInventory(t *testing.T) {
	t.Run("accepted report under the default task budget", func(t *testing.T) { runOfflineReviewReport(t, "", 7) })
	t.Run("blocked report", func(t *testing.T) { runOfflineReviewReport(t, "The assigned snapshot evidence is incomplete.", 1) })
}

func runOfflineReviewReport(t *testing.T, blocker string, primaryCount int) {
	t.Helper()
	session := loadHufuCodeReviewTeam(t)
	session.Workspace = t.TempDir()
	if err := session.SetCompatibilityWorkspaceScope(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	session.Config.ExecutionRoute = ""
	session.Config.DefaultLLMBackend = "ollama"
	session.Config.CoordinatorModel = "ollama/test"
	session.Config.Generation.ContextWindow = 131072
	session.Config.Reliability.MaxTasksWithoutProgress = agent.DefaultReliabilityConfig().MaxTasksWithoutProgress
	for _, def := range session.Agents {
		def.ExecutionRoute, def.Generation.Model = "", "ollama/test"
	}
	session.Agents["synthesizer"].Generation.ContextWindow = 131072
	session.Agents["synthesizer"].Generation.MaxTokens = "2048"
	// Produce fixed offline handoffs. The report and critique keep their
	// production transports. Preparation and source review use local fixtures.
	for _, role := range []string{"reviewer", "documentation-reviewer"} {
		session.Agents[role].ResultTransport = agent.ResultTransportTool
	}
	session.InvariantCatalog = nil
	preparation := offlineReviewPreparation{primaryCount: primaryCount}
	session.ProviderRegistry.Register("produce-workset", preparation)
	session.ProviderRegistry.Register("verify-review-tests", preparation)

	var collections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		assertCollectorResponseChannel(t, request)
		if !strings.Contains(fmt.Sprint(request["messages"]), resultCollectorSystem) {
			t.Error("provider did not receive a collector request")
		}
		collections.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		arguments := `{"result":{"structured_payload":{"gap_resolutions":{}}}}`
		if blocker != "" {
			arguments = fmt.Sprintf(`{"result":{"status":"blocked","summary":%q}}`, blocker)
		}
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"report\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"return\",\"type\":\"function\",\"function\":{\"name\":\"return_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"report\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", arguments)
	}))
	t.Cleanup(server.Close)

	c, err := NewCoordinator(session, server.URL+"/v1", "", nil, nil, RoleModels{}, 1, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	decisionConfig, err := session.ResolveDecisionRuntime()
	if err != nil {
		t.Fatal(err)
	}
	var decisions atomic.Int32
	c.decisionPrimitives, err = catalog.New(session.Config.DecisionPrimitives, catalog.WithRuntimeConfig(decisionConfig), catalog.WithSidecarGenerator(func(model string) (sidecarbackend.Generator, error) {
		return offlineReviewDecision{countingGenerator: countingGenerator{calls: &decisions}, model: model}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	c.providerBoundaryStart = func(context.Context, string) error { return nil }
	c.SetRunInputAssignments([]RunInputAssignment{{Name: "review.scope", RawValue: []byte(offlineReviewScope), Source: RunInputSourceCLI}})
	c.SetSessionData(NewSession())
	ctx, endRun := c.beginInvocationExecutionRun(t.Context())
	t.Cleanup(endRun)
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveRunInputsForInvocation(ctx, "review bounded fixture"); err != nil {
		t.Fatal(err)
	}
	if err := c.phaseWorkflow.Start(); err != nil {
		t.Fatal(err)
	}
	c.workerAgentOverride = scriptWorker(scriptedTurn{stream: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		return returnOfflineReviewWork(ctx, c)
	}})
	requests := func(ids []string) []TaskDef {
		request := make([]TaskDef, 0, len(ids))
		for _, id := range ids {
			task := c.inventoryContract(id)
			request = append(request, TaskDef{Agent: task.Agent, ContractID: id, Goal: id})
		}
		return request
	}
	dispatch := func(ids ...string) (string, error) {
		t.Helper()
		return c.ExecuteTasks(ctx, requests(ids))
	}
	delegate := func(ids ...string) (fantasy.ToolResponse, error) {
		t.Helper()
		modelRequests := make([]map[string]string, 0, len(ids))
		for _, task := range requests(ids) {
			modelRequests = append(modelRequests, map[string]string{"agent": task.Agent, "contract_id": task.ContractID, "goal": task.Goal})
		}
		body, err := json.Marshal(map[string]any{"tasks": modelRequests})
		if err != nil {
			t.Fatal(err)
		}
		return (&runAgentsTool{coordinator: c}).Run(ctx, fantasy.ToolCall{ID: ids[0], Name: "agent", Input: string(body)})
	}
	mustDispatch := func(ids ...string) {
		t.Helper()
		output, err := dispatch(ids...)
		if err != nil {
			if events, readErr := c.EventJournal().ReadEvents(ctx); readErr == nil {
				for _, event := range events {
					if event.Type == string(EventContextWindowAdmission) {
						var admission ContextWindowTelemetryEvent
						if json.Unmarshal(event.Payload, &admission) == nil && admission.Decision == string(ContextWindowCannotFit) {
							t.Logf("%s context admission for %s: window=%d, output=%d, margin=%d, input=%d", admission.Phase, admission.Model, admission.WindowTokens, admission.ReservedTokens, admission.SafetyTokens, admission.RequestedTokens)
						}
					}
				}
			}
			t.Fatalf("dispatch %v: %v (%s)", ids, err, output)
		}
		for _, id := range ids {
			items := inventoryWorkflowOccurrences(c, id)
			count := 1
			if id == "review-primary-workset" {
				count = primaryCount
			}
			if len(items) != count {
				t.Fatalf("%s produced %d tasks, want %d", id, len(items), count)
			}
			for _, item := range items {
				if item.Status != TaskDone {
					t.Fatalf("%s ended %s: %s / %s", id, item.Status, item.Detail, item.Output)
				}
			}
		}
	}
	mustDispatch("produce-workset", "verify-targeted-go-tests")
	mustDispatch("review-primary-workset", "review-documentation-workset", "review-documentation-escalation")
	mustDispatch("inventory-review-handoffs")
	reportBatch := []string{"derive-review-report", "synthesize-review", "verify-review-report"}
	if response, err := delegate(reportBatch...); err != nil || !response.IsError ||
		!strings.Contains(response.Content, "has no accepted critic-review child") || !c.coordinatorPolicyRepairPending.Load() {
		t.Fatalf("report admission with a missing critique: %s (%v)", response.Content, err)
	}
	if acceptance, err := c.runTeamAcceptance(ctx); err == nil || acceptance == nil || acceptance.Passed {
		t.Fatalf("acceptance passed without an audit: %+v, %v", acceptance, err)
	}
	if blocker == "" {
		// Exercise the real coordinator boundary after some unrelated task debits.
		// Counting each critique would hit the default limit before the report.
		c.recordNoProgressTasks(c.noProgressLimits().MaxTasks - primaryCount)
	}
	priorTasks := c.noProgressCounters().Tasks
	if response, err := delegate("critic-review"); err != nil || response.IsError || c.coordinatorPolicyRepairPending.Load() {
		t.Fatalf("corrected inventory delegation: %s (%v)", response.Content, err)
	}
	critics := inventoryWorkflowOccurrences(c, "critic-review")
	if len(critics) != primaryCount || c.noProgressCounters().Tasks != priorTasks+1 || c.noProgressStopPending() || c.noProgressReplanPending() {
		t.Fatalf("expected %d critiques and room for the report, got %d with counters %+v", primaryCount, len(critics), c.noProgressCounters())
	}
	if blocker != "" {
		_, dispatchErr := dispatch(reportBatch...)
		report := inventoryWorkflowOccurrences(c, "synthesize-review")[0]
		audit := inventoryWorkflowOccurrences(c, "verify-review-report")[0]
		checkpoint := currentResultCollection(report)
		if dispatchErr == nil || report.Status == TaskDone || checkpoint == nil || checkpoint.State != ResultCollectionCommitted ||
			audit.Status != TaskBlocked || audit.ExecutionReceipt != nil || collections.Load() != 1 {
			t.Fatalf("unsuccessful report was not collected and blocked: report=%s audit=%s calls=%d err=%v", report.Status, audit.Status, collections.Load(), dispatchErr)
		}
		result, err := c.acceptedCollectionResult(ctx, report, checkpoint)
		if err != nil || result.Status != TaskResultStatusBlocked || result.StructuredPayload != nil || result.Summary != blocker {
			t.Fatalf("collector did not retain the concrete failure reason: %+v, %v", result, err)
		}
		if acceptance, err := c.runTeamAcceptance(ctx); err == nil || acceptance == nil || acceptance.Passed {
			t.Fatalf("acceptance passed with an unsuccessful report: %+v, %v", acceptance, err)
		}
		return
	}
	mustDispatch(reportBatch...)
	acceptance, err := c.runTeamAcceptance(ctx)
	if err != nil || !acceptance.Passed {
		t.Fatalf("production audit did not satisfy blocking acceptance: %+v, %v", acceptance, err)
	}
	report := inventoryWorkflowOccurrences(c, "synthesize-review")[0]
	checkpoint := currentResultCollection(report)
	if decisions.Load() != int32(primaryCount) || collections.Load() != 1 || checkpoint == nil || checkpoint.State != ResultCollectionCommitted ||
		report.TypedResult.Status != "success" || len(report.EvidenceFrom) != 2*primaryCount+2 ||
		!strings.Contains(string(report.TypedResult.StructuredPayload.Value), `"audit_complete":true`) ||
		!strings.Contains(report.TypedResult.Summary, fmt.Sprintf("%d sources with %d independent critiques", primaryCount+2, primaryCount)) {
		t.Fatalf("incorrect v3 report completion, evidence, or transport: %s (%v, calls %d/%d)", report.Detail, report.EvidenceFrom, decisions.Load(), collections.Load())
	}
	for _, critic := range critics {
		if !slices.Contains(report.EvidenceFrom, critic.ID) {
			t.Fatalf("report is missing critique %s", critic.ID)
		}
	}
	final := c.finalizeRunPrepared(ctx, &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true, Response: report.TypedResult.Summary}, acceptance)
	if final.Outcome != RunOutcomeCompleted || !c.TerminalLifecycleConfirmed() {
		t.Fatalf("review finalization failed: %+v", final)
	}
	saved := loadTerminalReport(t, c)
	if len(saved.Sections) != 1 || saved.Sections[0].UnavailableReason != "" ||
		!strings.Contains(saved.Sections[0].Markdown, "last 1 commit ending at `HEAD`") ||
		!strings.Contains(saved.Sections[0].Markdown, "`base..head`") ||
		saved.Sections[0].Source.Task != "produce-workset" {
		t.Fatalf("review scope section: %+v", saved.Sections)
	}
}

type offlineReviewDecision struct {
	countingGenerator
	model string
}

func (g offlineReviewDecision) ModelID() string { return g.model }

type offlineReviewPreparation struct {
	primaryCount int
}

func (offlineReviewPreparation) Validate(Action) error { return nil }

func (p offlineReviewPreparation) Execute(ctx context.Context, action Action) (any, error) {
	var arguments struct{ Scope map[string]any }
	if err := json.Unmarshal([]byte(action.Payload), &arguments); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(arguments.Scope)
	if err != nil {
		return nil, err
	}
	inputHash := runInputHash(encoded)
	scope := map[string]any{"requested": arguments.Scope, "requested_input_hash": inputHash, "satisfied": true,
		"resolved":        map[string]any{"base": "base", "head": "head", "selected_commit_count": 1, "available_commit_count": 1, "history_exhausted": false, "repository_shallow": false},
		"observed_budget": map[string]any{"changed_paths": 1, "total_diff_bytes": 20, "total_diff_lines": 1, "workset_items": p.primaryCount + 2}}
	env := ActionEnvironmentFromContext(ctx)
	if action.Type == "verify_review_tests" {
		content := []byte("ok fixture\n")
		if err := os.WriteFile(filepath.Join(env.Workspace, "tests.txt"), content, 0o600); err != nil {
			return nil, err
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(content))
		tests := map[string]any{"completed": true, "passed": true, "reviewed_revision": "head", "base_revision": "base", "packages": []string{"./example"},
			"command": []string{"go", "test", "./example"}, "requested_input_hash": inputHash, "exit_code": 0, "timed_out": false, "output_truncated": false,
			"artifact_id": "sha256-" + hash, "artifact_sha256": hash, "output_artifact_id": "sha256-" + hash}
		return offlineReviewActionResult(map[string]any{"scope": scope, "targeted_go_tests": tests, "targeted_go_test_status": "completed", "test_execution_completed": true},
			[]ArtifactRef{{Path: "tests.txt", Kind: "test_output"}})
	}
	content := []byte("diff --git a/fixture.go b/fixture.go\n--- a/fixture.go\n+++ b/fixture.go\n@@ -1 +1 @@\n-return err\n+return nil\n")
	path := "source.diff"
	if err := os.WriteFile(filepath.Join(env.Workspace, path), content, 0o600); err != nil {
		return nil, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	artifacts := []ArtifactRef{{Path: path, Kind: "diff"}}
	for index, name := range []string{"primary review workset manifest", "documentation review workset manifest", "documentation escalation workset manifest"} {
		count, base, lens := 1, p.primaryCount+index-1, "documentation-risk"
		if index == 0 {
			count, base, lens = p.primaryCount, 0, "general"
		}
		manifest := WorksetManifest{SchemaVersion: WorksetSchemaVersion}
		for offset := range count {
			key := fmt.Sprintf("unit-%04d", base+offset)
			manifest.Items = append(manifest.Items, WorksetItem{
				Key: key, Bindings: map[string]string{"key": key, "review_revision": "head", "lens": lens, "reference_candidate_ids": "[]"},
				Inputs:       []ArtifactRef{{ID: "sha256-" + hash, SHA256: hash, Path: path, Kind: "diff"}},
				TouchedPaths: []string{"fixture.go"},
			})
		}
		value, err := json.Marshal(manifest)
		if err != nil {
			return nil, err
		}
		manifestPath := fmt.Sprintf("manifest-%d.json", index)
		if err := os.WriteFile(filepath.Join(env.Workspace, manifestPath), value, 0o600); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, ArtifactRef{Path: manifestPath, Kind: "workset_manifest", Description: name})
	}
	return offlineReviewActionResult(map[string]any{"scope": scope, "scope_context": "bounded fixture", "documentation_verification": map[string]any{"passed": true}}, artifacts)
}

func offlineReviewActionResult(outputs map[string]any, artifacts []ArtifactRef) (ActionResult, error) {
	outputs, err := inventoryFixtureJSON(outputs)
	return ActionResult{Outputs: outputs, Artifacts: artifacts}, err
}

func returnOfflineReviewWork(ctx context.Context, c *Coordinator) (*fantasy.AgentResult, error) {
	item, _ := ctx.Value(todoIDKey{}).(string)
	todo := c.todoItemByID(item)
	ctx = tools.SetToolsAllowed(ctx, []string{"view", decisionPrimitiveToolName, submitResultToolName})
	refs := todo.WorksetBinding
	if refs == nil {
		for _, id := range todo.EvidenceFrom {
			if source := c.todoItemByID(id); source.WorksetBinding != nil {
				refs = source.WorksetBinding
				break
			}
		}
	}
	if refs == nil || len(refs.Inputs) != 1 {
		return nil, fmt.Errorf("%s lacks its source artifact", todo.ContractID)
	}
	artifact := refs.Inputs[0].ID
	view := c.gatePolicyTools([]fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))})[0]
	response, err := view.Run(ctx, fantasy.ToolCall{ID: "read-source", Name: "view", Input: fmt.Sprintf(`{"artifact_ref":%q,"byte_limit":1024}`, artifact)})
	if err != nil || response.IsError {
		return nil, fmt.Errorf("view %s: %s (%v)", todo.ContractID, response.Content, err)
	}
	if todo.ContractID == "synthesize-review" {
		return textAgentResult("Reviewed the source records and the confirmed finding. There are no source gaps to resolve."), nil
	}
	var record map[string]any
	if todo.ContractID == "critic-review" {
		plans, err := c.fixedCollectionPlans(item)
		if err != nil || len(plans) != 1 || len(plans[0].rows) != 1 {
			return nil, fmt.Errorf("expected one critique slot: %v", err)
		}
		key := plans[0].rows[0].Key
		call := fantasy.ToolCall{ID: "assess", Name: decisionPrimitiveToolName, Input: fmt.Sprintf(`{"collection":"/record/decisions","row_key":%q,"group":"assessment"}`, key)}
		decision, err := (&decisionPrimitiveTool{coordinator: c}).Run(ctx, call)
		if err != nil || decision.IsError {
			return nil, fmt.Errorf("critique decision: %s (%v)", decision.Content, err)
		}
		record = map[string]any{"decisions": map[string]any{key: map[string]any{
			"rationale": "The observed diff drops the original error.", "evidence": []string{artifact}, "missing_evidence": "",
		}}}
	} else {
		findings := []any{}
		if todo.ContractID == "review-primary-workset" {
			findings = append(findings, map[string]any{"summary": "Dropped error", "detail": "The changed branch drops its original error.", "severity": "error",
				"location": "fixture.go:1", "evidence": []string{artifact}, "requires_critique": true})
		}
		record = map[string]any{"findings": findings, "gaps": []any{}, "reference_assessments": []any{}, "invariant_assessments": []any{}}
	}
	claims, err := json.Marshal(map[string]any{"status": "success", "summary": "Reviewed assigned evidence", "structured_payload": map[string]any{"record": record}})
	if err != nil {
		return nil, err
	}
	result, err := (&submitResultTool{coordinator: c, todoID: item}).Run(ctx, fantasy.ToolCall{ID: "submit", Name: submitResultToolName, Input: string(claims)})
	if err != nil || result.IsError {
		return nil, fmt.Errorf("submit %s: %s (%v)", todo.ContractID, result.Content, err)
	}
	return textAgentResult("Submitted result."), nil
}
