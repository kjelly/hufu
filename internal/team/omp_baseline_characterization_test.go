package team

// HF-OMP-000 baseline characterization for the worker results, isolation,
// routes, and hub plan
// (docs/archive/implementation-plans/worker-results-isolation-routes-hub.md §8).
//
// These tests pin runtime behavior that later HF-OMP phases either build on
// or deliberately change. A test that documents behavior a later phase
// changes names that phase in its comment; that phase must update the test
// in the same commit that changes the behavior.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// ExecutionTopology/ModelTopology mean parallel fan-out, not an ordered
// fallback list: every leaf beyond the first runs concurrently. Execution
// routes must therefore store fallback candidates in a separate field (D3).
func TestOMPCharacterizeModelTopologyIsFanout(t *testing.T) {
	def := &agent.AgentDef{Name: "worker", ExtraModels: []string{" ollama/b ", "ollama/c"}}
	topology := initialTaskModelTopology(def, " ollama/a ")
	if want := []string{"ollama/a", "ollama/b", "ollama/c"}; !slices.Equal(topology, want) {
		t.Fatalf("initialTaskModelTopology = %q, want %q", topology, want)
	}

	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "fanout", ModelTopology: topology},
		{Agent: "worker", Desc: "singleton", ModelTopology: []string{"ollama/a"}},
	})
	tests := []struct {
		name   string
		todoID string
		want   bool
	}{
		{name: "multi-leaf topology fans out", todoID: items[0].ID, want: true},
		{name: "singleton topology does not fan out", todoID: items[1].ID, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.shouldExecuteWithExtraModels(def, tt.todoID); got != tt.want {
				t.Fatalf("shouldExecuteWithExtraModels = %v, want %v", got, tt.want)
			}
		})
	}
}

// An extra-model leaf gets a private control workspace but keeps the parent
// subject root, so state-changing leaves write the same project files
// concurrently. D13 leaves this behavior unchanged; this test records it.
func TestOMPCharacterizeExtraModelLeafSharesSubjectRoot(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view,write", nil, nil)
	leafWorkspace := t.TempDir()
	leaf := cloneCoordinator(c, cloneSession(c.session, leafWorkspace))
	if leaf.projectDir != c.projectDir {
		t.Fatalf("leaf projectDir = %q, want the shared parent subject root %q", leaf.projectDir, c.projectDir)
	}
	if leaf.session.Workspace != leafWorkspace || c.session.Workspace == leafWorkspace {
		t.Fatalf("leaf control workspace = %q (parent %q), want a private leaf workspace", leaf.session.Workspace, c.session.Workspace)
	}
}

// Without a bounded workset scope, every task with a side effect holds an
// exclusive whole-root claim and read-only tasks hold a read claim. This is
// why DAG-scheduled writers never run concurrently today.
func TestOMPCharacterizeWholeRootClaimsSerializeWriters(t *testing.T) {
	tests := []struct {
		effect SideEffectClass
		mode   ResourceClaimMode
	}{
		{effect: SideEffectNone, mode: ResourceRead},
		{effect: SideEffectWorkspaceWrite, mode: ResourceExclusive},
		{effect: SideEffectExternalWrite, mode: ResourceExclusive},
		{effect: SideEffectInfraMutation, mode: ResourceExclusive},
	}
	snapshots := make(map[SideEffectClass]*TaskResourceScopeSnapshot, len(tests))
	for _, tt := range tests {
		t.Run(string(tt.effect), func(t *testing.T) {
			snapshot, err := wholeRootTaskResourceScope(nil, tt.effect)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := NewWorkspacePathResourceClaim(".", tt.mode)
			if !slices.Contains(snapshot.Claims, want) {
				t.Fatalf("claims = %#v, want %#v", snapshot.Claims, want)
			}
			if snapshot.Version != 1 || snapshot.Digest != taskResourceScopeDigest(snapshot) {
				t.Fatalf("snapshot version/digest = %d/%q", snapshot.Version, snapshot.Digest)
			}
			snapshots[tt.effect] = snapshot
		})
	}
	writer, reader := snapshots[SideEffectWorkspaceWrite].Claims, snapshots[SideEffectNone].Claims
	if !claimsConflict(writer, writer) || !claimsConflict(writer, reader) || claimsConflict(reader, reader) {
		t.Fatalf("whole-root claim conflicts: writer/writer=%v writer/reader=%v reader/reader=%v",
			claimsConflict(writer, writer), claimsConflict(writer, reader), claimsConflict(reader, reader))
	}
}

// The canonical policy snapshot of a team that uses no result contract and
// no execution route. Later phases must keep this JSON byte-for-byte stable
// so ConfigurationHash (a sha256 of the same JSON) does not drift for teams
// that do not opt in. Path- and environment-dependent values are pinned or
// replaced by placeholders so the golden file is machine-independent.
// Set UPDATE_EXECUTION_POLICY_GOLDEN=1 to regenerate the golden file.
func TestOMPCharacterizePolicySnapshotGolden(t *testing.T) {
	t.Setenv("HOME", "/hufu-golden/home")
	t.Setenv("CODEX_HOME", "/hufu-golden/codex-home")
	t.Setenv("PATH", "/hufu-golden/bin")
	workspace := t.TempDir()
	c := newExecutionPolicySnapshotCoordinatorWithOptions(t, workspace, 4, 3, executionPolicySnapshotCoordinatorOptions{
		codexConfig: defaultExecutionPolicyCodexConfig(),
		subjectRoot: t.TempDir(),
	})
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := cloneExecutionPolicySnapshot(state.snapshot)
	for i := range snapshot.ExecutionWorlds {
		if snapshot.ExecutionWorlds[i].ProjectRootHash != "" {
			snapshot.ExecutionWorlds[i].ProjectRootHash = "<project-root-hash>"
		}
	}
	snapshot.ConfigurationHash = "<configuration-hash>"
	got, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	goldenPath := filepath.Join("testdata", "execution-policy", "no-contracts-no-routes.golden.json")
	if os.Getenv("UPDATE_EXECUTION_POLICY_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s (run with UPDATE_EXECUTION_POLICY_GOLDEN=1 to create it): %v", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Fatalf("policy snapshot for a team without contracts or routes drifted from %s; new fields must be omitted when unused\n--- got ---\n%s", goldenPath, got)
	}
}

// The Todo's frozen target overwrites any receipt-supplied Backend. A
// fallback attempt would therefore be recorded under the primary backend.
// Flipped by HF-OMP-001, which adds ExecutionReceipt.ExecutionTarget.
func TestOMPCharacterizeReceiptBackendComesFromTodoTarget(t *testing.T) {
	list := NewTaskTracker().TodoList()
	item := list.AddBatch([]TodoSpec{{
		Agent: "worker", Desc: "receipt backend",
		ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: "primary"},
	}})[0]
	if err := list.SetExecutionReceipt(item.ID, &ExecutionReceipt{RunID: "run-1", TaskID: item.ID, Attempt: 1, Backend: "openai"}); err != nil {
		t.Fatal(err)
	}
	got := list.Items()[0].ExecutionReceipt
	if got == nil || got.Backend != "ollama" {
		t.Fatalf("receipt backend = %#v, want the Todo's frozen backend ollama", got)
	}
}

// Lifecycle payloads carry the occurrence attempt (Retries+1), not the
// in-dispatch attempt counter that receipts use.
func TestOMPCharacterizeLifecycleAttemptIsOccurrenceAttempt(t *testing.T) {
	tests := []struct {
		retries int
		want    int
	}{
		{retries: 0, want: 1},
		{retries: 2, want: 3},
	}
	for _, tt := range tests {
		item := &TodoItem{ID: "1", Agent: "worker", Retries: tt.retries, DispatchID: "dispatch-7"}
		payload := taskTransitionPayloadWithCoordinator(item, nil)
		if got := payload["attempt"]; got != tt.want {
			t.Fatalf("retries=%d: payload attempt = %v, want %d", tt.retries, got, tt.want)
		}
	}
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "attempt"}})[0]
	if got := c.taskAttempt(item.ID); got != 1 {
		t.Fatalf("taskAttempt for a fresh occurrence = %d, want 1", got)
	}
}

// The DispatchID is a process-local counter, so it cannot be a durable
// identity input for world IDs or attempt keys.
func TestOMPCharacterizeDispatchIDIsProcessLocal(t *testing.T) {
	first, second := newTaskDispatchID(), newTaskDispatchID()
	if first == second || !strings.HasPrefix(first, "dispatch-") || !strings.HasPrefix(second, "dispatch-") {
		t.Fatalf("dispatch IDs = %q, %q, want distinct process-local counters", first, second)
	}
}

// Free-text promotion does not consult RequiresGroundedResult today, so a
// grounded task could complete from prose. Flipped by HF-OMP-001.
func TestOMPCharacterizeGroundedTaskFreeTextPromotion(t *testing.T) {
	task := TaskDef{Agent: "worker", Goal: "review", Execution: ExecutionContract{RequiresResult: true, RequiresGroundedResult: true}}
	promoted := promoteValidatedReadOnlyHandoff(task, "1", "worker", "## Review\nThe module looks correct.")
	if promoted == nil || promoted.Source != "promoted_free_text" {
		t.Fatalf("promoted = %#v, want the current ungated promotion", promoted)
	}
}

// Status tool-argument projections are not redacted today, and the TUI
// previews them. Flipped by HF-OMP-001.
func TestOMPCharacterizeStatusToolArgsAreNotRedacted(t *testing.T) {
	event := StatusEvent{}.withTool("bash", `{"command":"curl -H 'Authorization: Bearer sk-live-123456'"}`)
	if !strings.Contains(event.ToolArgs, "sk-live-123456") {
		t.Fatalf("ToolArgs = %q, want the current unredacted projection", event.ToolArgs)
	}
}

// ParseFreeTextResult labels its output parsed_free_text; production callers
// always overwrite the Source, so the final values are recovered_protocol or
// promoted_free_text.
func TestOMPCharacterizeFreeTextSourcesAreOverwritten(t *testing.T) {
	parsed := ParseFreeTextResult("Finished the review.")
	if parsed == nil || parsed.Source != "parsed_free_text" {
		t.Fatalf("ParseFreeTextResult source = %#v, want parsed_free_text", parsed)
	}
	promoted := promoteValidatedReadOnlyHandoff(TaskDef{Agent: "worker"}, "1", "worker", "## Review\nDone.")
	if promoted == nil || promoted.Source != "promoted_free_text" || isSubmittedResultSource(promoted.Source) {
		t.Fatalf("promoted source = %#v, want promoted_free_text outside the submitted sources", promoted)
	}
}
