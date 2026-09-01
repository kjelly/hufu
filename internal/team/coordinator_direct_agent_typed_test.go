package team

// HF-MEM5-005 + HF-MEM5-003 direct-agent typed-result regression tests.
//
// The fast path used to assemble its agent through getOrCreateAgent — which
// never appended a todo-bound submitResultTool — and its prompt through
// directAgentWorkflowPrompt, which never appended the result-protocol
// instructions. As a result, c.GetTaskResult(todoID) was always nil and the
// shared-memory reducer fell back to generic output. The tests below pin:
//   - default direct-agent tool slices exclude mutation aliases;
//   - default direct-agent prompts use the typed-result contract;
//   - explicit opt-in exposes the alias and surfaces the compatibility
//     sentence iff the alias is actually granted;
//   - deny precedence removes both the tool and the prompt mention;
//   - the submitResult → reduce pipeline yields the typed
//     observation/decision/open_question/verification/artifact items;
//   - the runtime allowlist authorizes submit_result for direct attempts.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/tools"
)

// newDirectTypedCoordinator builds the minimal coordinator state required
// to exercise createDirectAgent + directAgentWorkflowPrompt. It mirrors the
// newDirectTerminationCoordinator scaffold without depending on the
// status-projection fixtures.
func newDirectTypedCoordinator(t *testing.T, agentTools string, allowed, denied []string) *Coordinator {
	t.Helper()
	workspace := t.TempDir()
	def := &agent.AgentDef{Name: "worker", Role: "worker", Tools: agentTools}
	c := &Coordinator{
		session: &TeamSession{
			Dir:       workspace,
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name: "test", Timeout: 10,
				ToolsAllowed: allowed,
				ToolsDenied:  denied,
			},
			Agents: map[string]*agent.AgentDef{"worker": def},
		},
		sessionData:    NewSession(),
		taskTracker:    NewTaskTracker(),
		agentCache:     map[string]fantasy.Agent{},
		agentPool:      &mockAgentPool{resolveDef: def, resolveKey: "worker"},
		reportStatus:   func(StatusEvent) {},
		projectDir:     workspace,
		sessionTime:    time.Now(),
		executionRunID: "run-direct-typed",
		coreTools:      workerInvariantCoreTools(t),
	}
	return c
}

func TestDirectAgentDefaultDisabledExcludesMutationAliases(t *testing.T) {
	c := newDirectTypedCoordinator(t, "", nil, nil)
	names := resolveWithExtras(t, c, "todo-default").Names
	for _, forbidden := range []string{"stm_write", "ltm_update", "memory_save"} {
		if sliceHasString(names, forbidden) {
			t.Fatalf("default direct-agent resolver exposed %q: %v", forbidden, names)
		}
	}
	granted := toolNameSet(names)
	prompt := c.directAgentWorkflowPrompt("perform work", workerAgentDef(), "worker", "todo-1", granted, TaskDef{Execution: ExecutionContract{RequiresResult: true}})
	for _, forbidden := range []string{"stm_write", "ltm_update", "memory_save"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("default direct-agent prompt mentions %q: %q", forbidden, prompt)
		}
	}
	if !strings.Contains(prompt, "submit_result") {
		t.Fatalf("default direct-agent prompt missing submit_result guidance: %q", prompt)
	}
	if !strings.Contains(prompt, "not complete until you call `submit_result`") {
		t.Fatalf("default direct-agent prompt missing terminal-result guidance: %q", prompt)
	}
}

func TestForcePlanFirstRejectsClosedSequenceBeforeProviderConstruction(t *testing.T) {
	t.Run("resolver rejects initial-plan literal", func(t *testing.T) {
		c := newDirectTypedCoordinator(t, "view", nil, nil)
		todoID := resolverTodoID(t, c, "initial-plan-closed")
		task := TaskDef{
			Agent:     "worker",
			Goal:      "review the bounded workset",
			PlanFirst: true,
			Execution: ExecutionContract{
				RequiresResult: true,
				ToolSequence:   []string{"view", submitResultToolName},
			},
		}
		_, err := c.ToolResolver().ResolveTaskTools(t.Context(), workerAgentDef(), WorkerToolResolutionRequest{
			Task: task, TodoID: todoID, Mode: workerToolResolutionModeForTask(task),
		})
		if err == nil || !strings.Contains(err.Error(), "initial-plan mode is incompatible with closed execution tool_sequence") {
			t.Fatalf("ResolveTaskTools error = %v, want deterministic initial-plan closed-sequence rejection", err)
		}
	})

	t.Run("force-plan-first rejects before worker creation", func(t *testing.T) {
		c := newDirectTypedCoordinator(t, "view", nil, nil)
		c.forcePlanFirst = true
		c.delegatedTasks = make(map[string]int)
		c.taskResultCache = make(map[string][]cachedTaskEntry)
		task := TaskDef{
			Agent: "worker",
			Goal:  "review the bounded workset",
			Execution: ExecutionContract{
				RequiresResult: true,
				ToolSequence:   []string{"view", submitResultToolName},
			},
		}
		_, err := c.ExecuteTasks(t.Context(), []TaskDef{task})
		items := c.taskTracker.TodoList().Items()
		if err == nil || len(items) != 1 || !strings.Contains(items[0].Detail, "cannot be combined with plan_first") {
			t.Fatalf("ExecuteTasks error = %v, todo items = %#v, want deterministic force-plan-first closed-sequence rejection", err, items)
		}
	})
}

func TestDirectAgentExplicitStmWriteOptInExposesAndMentions(t *testing.T) {
	c := newDirectTypedCoordinator(t, "stm_write", []string{"stm_write"}, nil)
	names := resolveWithExtras(t, c, "todo-explicit").Names
	if !sliceHasString(names, "stm_write") {
		t.Fatalf("explicit opt-in resolver missing stm_write: %v", names)
	}
	if !sliceHasString(names, "submit_result") {
		t.Fatalf("explicit opt-in resolver missing submit_result: %v", names)
	}
	granted := toolNameSet(names)
	prompt := c.directAgentWorkflowPrompt("perform work", workerAgentDef(), "worker", "todo-1", granted, TaskDef{Execution: ExecutionContract{RequiresResult: true}})
	if !strings.Contains(prompt, "stm_write") {
		t.Fatalf("explicit-opt-in direct prompt missing stm_write mention: %q", prompt)
	}
	if !strings.Contains(prompt, "deprecated typed compatibility tool") {
		t.Fatalf("explicit-opt-in direct prompt missing deprecated-compat wording: %q", prompt)
	}
}

func TestDirectAgentToolsDeniedBlocksOptIn(t *testing.T) {
	c := newDirectTypedCoordinator(t, "stm_write", []string{"stm_write"}, []string{"stm_write"})
	names := resolveWithExtras(t, c, "todo-denied").Names
	if sliceHasString(names, "stm_write") {
		t.Fatalf("deny precedence violated: stm_write still in resolver names: %v", names)
	}
	granted := toolNameSet(names)
	prompt := c.directAgentWorkflowPrompt("perform work", workerAgentDef(), "worker", "todo-1", granted, TaskDef{Execution: ExecutionContract{RequiresResult: true}})
	if strings.Contains(prompt, "stm_write") {
		t.Fatalf("deny precedence violated: prompt mentions stm_write: %q", prompt)
	}
}

func TestDirectAgentRuntimeAllowlistAuthorizesSubmitResult(t *testing.T) {
	c := newDirectTypedCoordinator(t, "", nil, nil)
	names := resolveWithExtras(t, c, "todo-allow").Names
	if !sliceHasString(names, "submit_result") {
		t.Fatalf("resolver dropped submit_result for default direct agent: %v", names)
	}
	def := workerAgentDef()
	ctx := c.withEffectiveToolsAllowed(context.Background(), def, names)
	allowed := tools.GetToolsAllowed(ctx)
	if !sliceHasString(allowed, "submit_result") {
		t.Fatalf("runtime allowlist missing submit_result: %v", allowed)
	}
}

func TestWorkerToolResolverOwnsLifecycleProtocolSurface(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	def := c.session.Agents["worker"]
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "lifecycle surface"}})[0]
	baseTask := TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}}

	initial, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{
		Task: TaskDef{Agent: "worker", PlanFirst: true, Execution: baseTask.Execution}, TodoID: item.ID, Mode: WorkerToolResolutionInitialPlan,
	})
	if err != nil {
		t.Fatalf("initial plan resolution: %v", err)
	}
	if slices.Contains(initial.Names, submitResultToolName) || !slices.Contains(initial.Names, "submit_plan") {
		t.Fatalf("initial plan surface = %v, want submit_plan without submit_result", initial.Names)
	}
	for _, tool := range initial.Tools {
		if plan, ok := tool.(*submitPlanTool); ok && (plan.coordinator != c || plan.todoID != item.ID) {
			t.Fatalf("initial plan binding = %#v, want coordinator=%p todo=%q", plan, c, item.ID)
		}
	}

	approved, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{
		Task: TaskDef{Agent: "worker", PlanFirst: true, PlanID: item.ID, Execution: baseTask.Execution}, TodoID: item.ID, Mode: WorkerToolResolutionApprovedPlan,
	})
	if err != nil {
		t.Fatalf("approved plan resolution: %v", err)
	}
	if !slices.Contains(approved.Names, submitResultToolName) || slices.Contains(approved.Names, "submit_plan") {
		t.Fatalf("approved plan surface = %v, want submit_result without submit_plan", approved.Names)
	}

	repair, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{
		Task: baseTask, TodoID: item.ID, Mode: WorkerToolResolutionResultRepair,
	})
	if err != nil {
		t.Fatalf("result repair resolution: %v", err)
	}
	if !slices.Equal(repair.Names, []string{submitResultToolName}) || len(repair.Tools) != 1 {
		t.Fatalf("result repair surface = %v, want exactly submit_result", repair.Names)
	}
	resultTool, ok := repair.Tools[0].(*submitResultTool)
	if !ok || resultTool.coordinator != c || resultTool.todoID != item.ID {
		t.Fatalf("result repair binding = %#v, want coordinator=%p todo=%q", repair.Tools[0], c, item.ID)
	}

	if _, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{
		Task: baseTask, Mode: WorkerToolResolutionNormal,
	}); err == nil || !strings.Contains(err.Error(), "requires a Todo ID") {
		t.Fatalf("missing Todo ID error = %v, want fail-closed identity error", err)
	}
}

func TestWorkerToolResolverDoesNotBypassProtocolDenial(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, []string{submitResultToolName})
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "denied protocol"}})[0]
	_, err := c.ToolResolver().ResolveTaskTools(t.Context(), c.session.Agents["worker"], WorkerToolResolutionRequest{
		Task: TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}}, TodoID: item.ID, Mode: WorkerToolResolutionNormal,
	})
	if err == nil || !strings.Contains(err.Error(), "denied by team policy") {
		t.Fatalf("protocol denial error = %v, want team-policy failure", err)
	}
}

// directToolNotesCaptureAgent is installed only through the existing direct
// agent cache seam. RunDirectAgent still performs the production resolver,
// context construction, prompt assembly, and runAgentWithStatusAndHistory
// call before this agent observes the request. Reading the allowlist from the
// context lets this test compare the actual runtime grant with the resolver's
// final model-visible surface.
type directToolNotesCaptureAgent struct {
	prompt  string
	allowed []string
}

func (a *directToolNotesCaptureAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.prompt = call.Prompt
	a.allowed = append([]string(nil), tools.GetToolsAllowed(ctx)...)
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "direct prompt captured"},
	}}}, nil
}

func (a *directToolNotesCaptureAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func TestRunDirectAgentPromptNotesUseResolverSurface(t *testing.T) {
	for _, tc := range []struct {
		name      string
		denied    []string
		wantNotes bool
	}{
		{name: "deny filtered surface", denied: []string{"sudo", "wait_for"}},
		{name: "full surface", wantNotes: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The declared tools intentionally include capabilities that the
			// team deny filter removes in the first case. No model-visible list
			// is supplied by this test: the default resolver constructs it from
			// the real core tool registry and the synthetic result-tool extra.
			c := newDirectTypedCoordinator(t, "bash,sudo,wait_for", nil, tc.denied)
			def := c.session.Agents["worker"]
			syntheticTask := TaskDef{Agent: "worker", Goal: "perform direct work", Execution: ExecutionContract{RequiresResult: true}}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: syntheticTask.Goal}})[0]
			resolved, err := c.ToolResolver().ResolveTaskTools(context.Background(), def, WorkerToolResolutionRequest{
				Task: syntheticTask, TodoID: item.ID, Mode: WorkerToolResolutionNormal,
			})
			if err != nil {
				t.Fatalf("default resolver: %v", err)
			}
			for _, name := range []string{"bash", "submit_result"} {
				if !sliceHasString(resolved.Names, name) {
					t.Fatalf("resolver final names = %v, missing %q", resolved.Names, name)
				}
			}
			for _, name := range tc.denied {
				if sliceHasString(resolved.Names, name) {
					t.Fatalf("resolver final names = %v, denied tool %q still exposed", resolved.Names, name)
				}
			}
			for _, tool := range resolved.Tools {
				if tool == nil {
					t.Fatalf("resolver returned nil tool in final surface %v", resolved.Names)
				}
			}

			capture := &directToolNotesCaptureAgent{}
			c.workerAgentOverride = capture

			if _, err := c.RunDirectAgent(context.Background(), "worker", syntheticTask.Goal); err != nil {
				t.Fatalf("RunDirectAgent: %v", err)
			}
			if capture.prompt == "" {
				t.Fatal("RunDirectAgent did not deliver a prompt to the production agent consumer")
			}
			items := c.taskTracker.TodoList().Items()
			var directItem *TodoItem
			for _, item := range items {
				if item != nil && item.ExecutionReceipt != nil {
					directItem = item
					break
				}
			}
			if directItem == nil {
				t.Fatalf("direct task receipt = %#v, want a persisted receipt", items)
			}
			if scope := directItem.ExecutionReceipt.ArtifactScope; scope == nil || scope.TaskID != directItem.ID || scope.Attempt != 1 {
				t.Fatalf("direct task artifact scope receipt = %#v, want task %q attempt 1", directItem.ExecutionReceipt.ArtifactScope, directItem.ID)
			}

			// The allowlist observed by the actual consumer must be exactly the
			// final resolver Names used to build the direct agent surface.
			if !slices.Equal(capture.allowed, resolved.Names) {
				t.Fatalf("runtime allowlist = %v, resolver final names = %v", capture.allowed, resolved.Names)
			}

			const privilegedNote = "The bash tool REJECTS sudo commands. Run privileged/remote commands through the dedicated sudo tool(s) directly"
			const pollingNote = "When waiting for a state change (VM boot, service ready, async job completion), call `wait_for` once"
			if tc.wantNotes {
				for _, fragment := range []string{privilegedNote, pollingNote, "## Tool Notes"} {
					if !strings.Contains(capture.prompt, fragment) {
						t.Errorf("full resolver surface prompt missing note %q: %s", fragment, capture.prompt)
					}
				}
			} else {
				for _, fragment := range []string{privilegedNote, pollingNote, "## Tool Notes"} {
					if strings.Contains(capture.prompt, fragment) {
						t.Errorf("filtered resolver surface prompt mentions unavailable note %q: %s", fragment, capture.prompt)
					}
				}
			}
		})
	}
}

func TestRunDirectAgentRejectsProseWithoutSubmittedResult(t *testing.T) {
	c := newDirectTypedCoordinator(t, "", nil, nil)
	c.workerAgentOverride = &directToolNotesCaptureAgent{}

	result, err := c.RunDirectAgent(t.Context(), "worker", "perform direct work")
	if err != nil {
		t.Fatalf("RunDirectAgent returned top-level error: %v", err)
	}
	if result == nil || result.Error == nil {
		t.Fatalf("RunDirectAgent result = %#v, want protocol failure", result)
	}
	if !strings.Contains(result.Error.Error(), "missing submitted task result") {
		t.Fatalf("RunDirectAgent error = %v, want missing submitted task result", result.Error)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].Status == TaskDone {
		t.Fatalf("direct task items = %#v, want non-done terminal task", items)
	}
	if items[0].Status != TaskError {
		t.Fatalf("direct task status = %s, want error", items[0].Status)
	}
	if items[0].FailureEvent == nil || items[0].FailureEvent.FailureClass != FailureProtocol {
		t.Fatalf("direct task failure event = %#v, want protocol failure", items[0].FailureEvent)
	}
	if got := c.GetTaskResult(items[0].ID); got != nil {
		t.Fatalf("prose-only direct task stored typed result = %#v", got)
	}
}

// TestDirectAgentTypedSubmitReducesContextItems pins the half of HF-MEM5-005
// the fast path was failing: a direct worker that calls submit_result with
// a fully-typed payload must produce the corresponding typed ContextItem
// entries through reduceTaskResultToSharedMemory. Without submit_result in
// the direct tool slice, the reducer falls back to generic output and the
// downstream retrieval manifest has nothing to bind.
func TestDirectAgentTypedSubmitReducesContextItems(t *testing.T) {
	c := newDirectTypedCoordinator(t, "", nil, nil)
	repo, err := contextstore.OpenSQLite(filepath.Join(c.session.Workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c.contextRepo = repo

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "perform direct work"}})
	todoID := items[0].ID
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskInProgress, "running", ""); err != nil {
		t.Fatal(err)
	}

	// Build the same todo-bound submitResultTool the resolver appends.
	resolved := mustResolveTools(t, c, todoID)
	extras := []fantasy.AgentTool{&submitResultTool{coordinator: c, todoID: todoID}}
	all := append(append([]fantasy.AgentTool(nil), resolved...), extras...)
	if !containsTool(all, "submit_result") {
		t.Fatalf("submit_result missing from resolved direct-agent tool slice")
	}

	// materializeSubmittedArtifacts resolves declared artifact paths in the
	// team workspace, so the file must exist before submit_result validates.
	if err := os.MkdirAll(filepath.Join(c.projectDir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(c.projectDir, "out", "direct.txt")
	if err := os.WriteFile(artifactPath, []byte("produced artifact content"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"status":  "success",
		"summary": "direct typed result",
		"findings": []map[string]any{
			{"category": "observation", "summary": "alpha-finding", "detail": "detail-1"},
		},
		"decisions":      []string{"use the typed-result contract"},
		"open_questions": []string{"follow-up question"},
		"verification":   []map[string]any{{"command": "true", "exit_code": 0}},
		"artifacts":      []map[string]any{{"path": "out/direct.txt", "description": "produced artifact", "type": "text"}},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	tool := &submitResultTool{coordinator: c, todoID: todoID}
	ctx := occurrenceTestContext(c, todoID, 1)
	response, err := tool.Run(ctx, fantasy.ToolCall{Name: "submit_result", Input: string(payloadJSON)})
	if err != nil || response.IsError {
		t.Fatalf("submit_result Run response=%+v err=%v content=%q", response, err, response.Content)
	}
	stored := c.GetTaskResult(todoID)
	if stored == nil {
		t.Fatal("submit_result did not store TaskResult for direct todo")
	}
	def := workerAgentDef()
	c.reduceTaskResultToSharedMemory(context.Background(), TaskResultMemoryInput{
		TodoID: todoID, Agent: def, Result: stored, Output: "raw output", Verified: true, Attempt: 1,
	})

	got := mustQueryContextItems(t, c)
	kinds := make(map[contextstore.ContextKind]int)
	for _, it := range got {
		if it.Metadata["task_id"] == todoID {
			kinds[it.Kind]++
		}
	}
	wantKinds := map[contextstore.ContextKind]int{
		contextstore.ContextObservation:  1,
		contextstore.ContextDecision:     1,
		contextstore.ContextOpenQuestion: 1,
		contextstore.ContextVerification: 1,
		contextstore.ContextArtifact:     1,
	}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("direct typed submit produced kinds=%v, want %v", kinds, wantKinds)
	}
	for k, want := range wantKinds {
		if kinds[k] != want {
			t.Fatalf("direct typed submit kind %s count = %d, want %d (kinds=%v)", k, kinds[k], want, kinds)
		}
	}
}

func mustResolveNames(t *testing.T, c *Coordinator) []string {
	t.Helper()
	todoID := resolverTodoID(t, c, "resolver-names")
	names, err := c.ToolResolver().ResolveTaskTools(context.Background(), workerAgentDef(), WorkerToolResolutionRequest{
		Task: TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}}, TodoID: todoID, Mode: WorkerToolResolutionNormal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return names.Names
}

func resolveWithExtras(t *testing.T, c *Coordinator, todoID string) ResolvedWorkerTools {
	t.Helper()
	todoID = resolverTodoID(t, c, todoID)
	resolved, err := c.ToolResolver().ResolveTaskTools(context.Background(), workerAgentDef(), WorkerToolResolutionRequest{
		Task: TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}}, TodoID: todoID, Mode: WorkerToolResolutionNormal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func mustResolveTools(t *testing.T, c *Coordinator, todoID string) []fantasy.AgentTool {
	t.Helper()
	todoID = resolverTodoID(t, c, todoID)
	resolved, err := c.ToolResolver().ResolveTaskTools(context.Background(), workerAgentDef(), WorkerToolResolutionRequest{
		Task: TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}}, TodoID: todoID, Mode: WorkerToolResolutionNormal,
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Tools
}

func resolverTodoID(t *testing.T, c *Coordinator, want string) string {
	t.Helper()
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.ID == want {
			return want
		}
	}
	return c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: want}})[0].ID
}

func containsTool(tools []fantasy.AgentTool, want string) bool {
	for _, tool := range tools {
		if tool != nil && tool.Info().Name == want {
			return true
		}
	}
	return false
}

func sliceHasString(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func workerAgentDef() *agent.AgentDef {
	return &agent.AgentDef{Name: "worker", Role: "worker"}
}
