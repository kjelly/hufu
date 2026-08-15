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
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
	contextstore "github.com/kjelly/hufu/internal/context"
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
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		agentCache:   map[string]fantasy.Agent{},
		agentPool:    &mockAgentPool{resolveDef: def, resolveKey: "worker"},
		reportStatus: func(StatusEvent) {},
		projectDir:   workspace,
		sessionTime:  time.Now(),
		coreTools:    workerInvariantCoreTools(t),
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
	response, err := tool.Run(context.Background(), fantasy.ToolCall{Name: "submit_result", Input: string(payloadJSON)})
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
	names, err := c.ToolResolver().ResolveTaskTools(context.Background(), workerAgentDef(), TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}}, []fantasy.AgentTool{})
	if err != nil {
		t.Fatal(err)
	}
	return names.Names
}

func resolveWithExtras(t *testing.T, c *Coordinator, todoID string) ResolvedWorkerTools {
	t.Helper()
	resolved, err := c.ToolResolver().ResolveTaskTools(context.Background(), workerAgentDef(), TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}}, []fantasy.AgentTool{&submitResultTool{coordinator: c, todoID: todoID}})
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func mustResolveTools(t *testing.T, c *Coordinator, todoID string) []fantasy.AgentTool {
	t.Helper()
	resolved, err := c.ToolResolver().ResolveTaskTools(context.Background(), workerAgentDef(), TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}}, []fantasy.AgentTool{})
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Tools
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
