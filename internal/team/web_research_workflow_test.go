package team

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/mcp"
)

func webResearchWorkflow(t *testing.T) (*TeamSession, *runtimeWorkflow, []TaskDef) {
	t.Helper()
	session, err := LoadTeam(filepath.Join(bundledTeamsDir(t), "web-research"), nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	session.Workspace = t.TempDir()
	if shouldWarnPromptWorkflowDeprecation(session) {
		t.Fatal("bundled web research still relies on prompt-defined workflow")
	}
	requests := []TaskDef{
		{Agent: "researcher", ContractID: "primary-research"},
		{Agent: "countercheck", ContractID: "counter-research"},
		{Agent: "verifier", ContractID: "evidence-audit", EvidenceFrom: []string{"1", "2"}},
	}
	bound, _, err := CompileTaskGoalContracts(session, requests)
	if err != nil {
		t.Fatal(err)
	}
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	return session, w, bound
}

func TestWebResearchWorkflowRetainsSearchAndIsolatedBrowserTools(t *testing.T) {
	session, w, bound := webResearchWorkflow(t)
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	c.session, c.phaseWorkflow = session, w
	c.ollamaWebKeyPresent = true // Tool resolution only; no hosted search call.
	c.mcpManager = mcp.NewMCPToolManager("", "")
	t.Cleanup(func() { _ = c.mcpManager.Close() })
	for name, cfg := range session.MCPServers {
		fake := newMCPActionFake(mcpgo.NewTool("browser_navigate", mcpgo.WithString("url", mcpgo.Required())))
		fake.server.AddTool(mcpgo.NewTool("browser_snapshot"), func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText("offline snapshot"), nil
		})
		attachMCPActionFake(t, c.mcpManager, name, cfg, fake)
	}
	for _, task := range bound {
		if task.Phase == PhaseVerify {
			if err := w.observe([]*TodoItem{
				{ContractID: bound[0].ContractID, Phase: PhasePrepare, Status: TaskDone},
				{ContractID: bound[1].ContractID, Phase: PhasePrepare, Status: TaskDone},
			}); err != nil {
				t.Fatal(err)
			}
		}
		def := session.Agents[task.Agent]
		def.Generation.Model = "test"
		snapshot, err := c.resolveNewTaskToolAuthorization(t.Context(), task, def)
		if err != nil {
			t.Fatal(err)
		}
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: task.Agent, Desc: "offline tool admission", DynamicToolAuthorization: snapshot}})[0]
		resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{Task: task, TodoID: item.ID})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(resolved.Names, "web_search") || !slices.Contains(resolved.Names, submitResultToolName) {
			t.Fatalf("%s lost search/result tools: %v", task.Agent, resolved.Names)
		}
		want := []string{"playwright_" + task.Agent + "__browser_navigate", "playwright_" + task.Agent + "__browser_snapshot"}
		if len(snapshot.Targets) != len(want) {
			t.Fatalf("%s browser authority = %#v", task.Agent, snapshot.Targets)
		}
		for _, name := range want {
			if !slices.Contains(resolved.AuthorizedNames, name) {
				t.Fatalf("%s lost its isolated browser %q: %v", task.Agent, name, resolved.AuthorizedNames)
			}
		}
	}
}

func TestWebResearchWorkflowRequiresBothIndependentSearchesAndRestoresAudit(t *testing.T) {
	session, w, bound := webResearchWorkflow(t)
	if err := w.requireFinished(); err == nil {
		t.Fatal("empty research satisfied finish gate")
	}
	if err := w.validateTasks(bound[2:]); err == nil {
		t.Fatal("evidence audit admitted before research")
	}
	if err := w.validateTasks(bound[:1]); err == nil || !strings.Contains(err.Error(), "counter-research") {
		t.Fatalf("single-track dispatch = %v, want missing counter-research", err)
	}
	if err := w.validateTasks([]TaskDef{bound[0], bound[0]}); err == nil {
		t.Fatal("duplicate primary research substituted for independent countercheck")
	}
	if err := w.validateTasks(bound[:2]); err != nil {
		t.Fatal(err)
	}
	items := []*TodoItem{
		{ID: "1", Agent: "researcher", ContractID: bound[0].ContractID, Phase: bound[0].Phase, Status: TaskDone,
			TypedResult: &TaskResult{Status: TaskResultStatusSuccess, Source: "submitted"}},
		{ID: "2", Agent: "countercheck", ContractID: bound[1].ContractID, Phase: bound[1].Phase, Status: TaskInProgress},
	}
	if err := w.observe(items); err != nil || w.State() != PhasePrepare {
		t.Fatalf("unfinished countercheck advanced research: phase=%s, err=%v", w.State(), err)
	}
	items[1].Status = TaskDone
	items[1].TypedResult = &TaskResult{Status: TaskResultStatusCompletedWithGaps, Source: "submitted"}
	if err := validateCompletedTaskResult(items[1].TypedResult); err != nil {
		t.Fatal(err)
	}
	if err := w.observe(items); err != nil || w.State() != PhaseVerify {
		t.Fatalf("completed search with gaps did not advance: phase=%s, err=%v", w.State(), err)
	}

	state, results, root, retries := w.snapshot()
	restored, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.restore(state, results, root, retries); err != nil {
		t.Fatal(err)
	}
	if got := restored.activeWorkerNames(); !slices.Equal(got, []string{"verifier"}) {
		t.Fatalf("resumed active workers = %v", got)
	}
	if err := restored.validateTasks(bound[:2]); err == nil {
		t.Fatal("resumed audit admitted repeated independent searches")
	}
	if err := restored.validateTasks(bound[2:]); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(bound[2].EvidenceFrom, []string{"1", "2"}) {
		t.Fatal("static binding lost explicit research evidence inputs")
	}
	if err := restored.requireFinished(); err == nil {
		t.Fatal("resumed workflow finished without the audit")
	}
	items = append(items, &TodoItem{
		ID: "3", Agent: "verifier", ContractID: bound[2].ContractID, Phase: bound[2].Phase, Status: TaskDone,
		TypedResult:  &TaskResult{Status: TaskResultStatusCompletedWithGaps, Source: "submitted"},
		VerifyResult: &VerificationResult{ExitCode: 0},
	})
	if err := restored.observe(items); err != nil || restored.State() != PhaseDone {
		t.Fatalf("verified audit did not finish: phase=%s, err=%v", restored.State(), err)
	}
	if err := restored.requireFinished(); err != nil {
		t.Fatal(err)
	}
}

func TestWebResearchWorkflowStopsOnUnresolvedResearch(t *testing.T) {
	for _, status := range []TaskStatus{TaskError, TaskBlocked, TaskProtocolIncomplete} {
		t.Run(string(status), func(t *testing.T) {
			_, w, bound := webResearchWorkflow(t)
			items := []*TodoItem{
				{ID: "1", Agent: "researcher", ContractID: bound[0].ContractID, Phase: bound[0].Phase, Status: TaskDone},
				{ID: "2", Agent: "countercheck", ContractID: bound[1].ContractID, Phase: bound[1].Phase, Status: status, Detail: "research input unavailable"},
			}
			if err := w.observe(items); err == nil || !strings.Contains(err.Error(), "research input unavailable") {
				t.Fatalf("unresolved research diagnostic = %v", err)
			}
			if w.State() != PhaseFailed || w.failedTask() != "2" {
				t.Fatalf("unresolved search phase=%s, failed task=%q", w.State(), w.failedTask())
			}
			if err := w.validateTasks(bound[2:]); err == nil {
				t.Fatal("verifier admitted after unresolved research")
			}
			if err := w.requireFinished(); err == nil {
				t.Fatal("unresolved research satisfied finish gate")
			}
		})
	}
}

func TestWebResearchWorkflowRequiresObjectiveAuditVerification(t *testing.T) {
	for _, name := range []string{"missing_verification", "incomplete_audit", "empty_findings", "complete_with_gaps"} {
		t.Run(name, func(t *testing.T) {
			session, w, bound := webResearchWorkflow(t)
			items := []*TodoItem{
				{ID: "1", Agent: "researcher", ContractID: bound[0].ContractID, Phase: bound[0].Phase, Status: TaskDone},
				{ID: "2", Agent: "countercheck", ContractID: bound[1].ContractID, Phase: bound[1].Phase, Status: TaskDone},
			}
			if err := w.observe(items); err != nil {
				t.Fatal(err)
			}
			// These minimal canonical payloads exercise the task-result assertions.
			// Full schema, claim accounting and browser evidence have separate tests.
			payload := []byte(`{"audit_complete":true,"findings":[{"verdict":"unverified"}]}`)
			if name == "incomplete_audit" {
				payload = []byte(`{"audit_complete":false,"findings":[{"verdict":"unverified"}]}`)
			} else if name == "empty_findings" {
				payload = []byte(`{"audit_complete":true,"findings":[]}`)
			}
			audit := &TodoItem{
				ID: "3", Agent: "verifier", ContractID: bound[2].ContractID, Phase: bound[2].Phase, Status: TaskDone,
				TypedResult: &TaskResult{Status: TaskResultStatusCompletedWithGaps, Source: "submitted", StructuredPayload: &ResultPayload{Value: payload}},
			}
			if name != "missing_verification" {
				var err error
				audit.VerifyResult, err = ExecuteVerificationSpecWithTaskResult(t.Context(), "sh", session.Workspace, *bound[2].VerifySpec, audit.TypedResult)
				if wantPass := name == "complete_with_gaps"; isVerifySuccess(audit.VerifyResult) != wantPass || (err == nil) != wantPass {
					t.Fatalf("audit verification = %#v, err=%v", audit.VerifyResult, err)
				}
			}
			err := w.observe(append(items, audit))
			if name == "complete_with_gaps" {
				if err != nil || w.State() != PhaseDone {
					t.Fatalf("complete inconclusive audit rejected: phase=%s, err=%v", w.State(), err)
				}
			} else if err == nil || w.State() != PhaseFailed {
				t.Fatalf("unverified audit finished: phase=%s, err=%v", w.State(), err)
			}
		})
	}
}
