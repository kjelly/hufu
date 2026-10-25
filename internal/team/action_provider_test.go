package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/golangruntime"
)

func TestLoadTeamRegistersConfiguredActionProviderInSessionRegistry(t *testing.T) {
	dir := t.TempDir()
	teamYAML := `name: adapter-team
workflow:
  phases: [prepare, audit, execute, verify]
capabilities:
  required: [structured-actions]
delegation:
  bind-task-goal-contracts: true
action-providers:
  structured-actions:
    command: [/bin/sh, -c, 'cat >/dev/null; printf "{\"status\":\"ok\"}"']
tasks:
  - id: prepare
    agent: preparer
    phase: prepare
  - id: audit
    agent: auditor
    phase: audit
  - id: execute
    agent: executor
    phase: execute
    action:
      capability: structured-actions
      type: apply
      payload: '{}'
  - id: verify
    agent: verifier
    phase: verify
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(teamYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"preparer", "auditor", "executor", "verifier"} {
		content := "---\nname: " + name + "\nrole: worker\n---\nworker\n"
		if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := NewProviderRegistry()
	session, err := LoadTeam(dir, nil, nil, base)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if base.Has("structured-actions") {
		t.Fatal("team-specific provider mutated base registry")
	}
	provider, ok := session.ProviderRegistry.Get(" structured-actions ")
	if !ok {
		t.Fatal("configured action provider was not registered in session")
	}
	result, err := provider.Execute(context.Background(), Action{Capability: "structured-actions", Type: "apply", Payload: `{}`})
	if err != nil {
		t.Fatalf("configured provider Execute: %v", err)
	}
	resultMap, ok := result.(map[string]interface{})
	if !ok || resultMap["status"] != "ok" {
		t.Fatalf("configured provider result = %#v", result)
	}
}

func TestRegisterConfiguredGolangActionProviderValidatesTrustedStaticContract(t *testing.T) {
	teamDir := t.TempDir()
	source := filepath.Join(teamDir, "action")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	program := `package main
import ("context"; "io")
func Run(_ context.Context, _ io.Reader, _ io.Writer) error { return nil }
`
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewProviderRegistry()
	err := registerConfiguredActionProviders(registry, map[string]agent.ActionProviderConfig{
		"demo": {Runtime: "golang", Source: "./action", Mode: golangruntime.TrustedStaticMode},
	}, teamDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if name := registry.ProviderName("demo"); !strings.HasPrefix(name, "golang:sha256:") {
		t.Fatalf("provider name = %q", name)
	}

	for name, config := range map[string]agent.ActionProviderConfig{
		"missing mode": {Runtime: "golang", Source: "./action"},
		"mixed command": {
			Runtime: "golang", Source: "./action", Mode: golangruntime.TrustedStaticMode, Command: []string{"go"},
		},
		"unsupported runtime": {Runtime: "python", Source: "./action"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := registerConfiguredActionProviders(NewProviderRegistry(), map[string]agent.ActionProviderConfig{"demo": config}, teamDir, nil); err == nil {
				t.Fatal("configuration was accepted")
			}
		})
	}
}

func TestGolangActionProviderUsesWireContractAndActionEnvironment(t *testing.T) {
	provider := &golangActionProvider{
		capability: "demo",
		program:    golangruntime.Program{Source: "/team/action", Digest: "sha256:fixture"},
		execute: func(_ context.Context, _ golangruntime.Program, input []byte, env []string, _, _ int) (golangruntime.Result, error) {
			var action Action
			if err := json.Unmarshal(input, &action); err != nil {
				t.Fatal(err)
			}
			if action.Type != "apply" || !slices.Contains(env, "HUFU_REPOSITORY=/repo") {
				t.Fatalf("action=%#v env=%#v", action, env)
			}
			return golangruntime.Result{Stdout: []byte(`{"status":"ok"}`)}, nil
		},
	}
	ctx := WithActionEnvironment(t.Context(), ActionEnvironment{Repository: "/repo"})
	result, err := provider.Execute(ctx, Action{Capability: "demo", Type: "apply", Payload: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["status"] != "ok" {
		t.Fatalf("result = %#v", result)
	}
}

func TestCommandActionProviderHonorsContextDeadline(t *testing.T) {
	provider := &commandActionProvider{
		capability: "structured-actions",
		command:    []string{"/bin/sh", "-c", "cat >/dev/null; sleep 1"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := provider.Execute(ctx, Action{Capability: "structured-actions", Type: "apply"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 750*time.Millisecond {
		t.Fatalf("deadline did not bound process cleanup: %s", elapsed)
	}
}

func TestCommandResolverDeadlineBoundsDescendants(t *testing.T) {
	provider := &commandActionProvider{
		command: []string{"/bin/sh", "-c", "cat >/dev/null; sleep 1"},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := provider.ResolveRunInput(ctx, RunInputResolverRequest{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 750*time.Millisecond {
		t.Fatalf("resolver cleanup: elapsed=%s error=%v", time.Since(started), err)
	}
}

func TestCommandActionProviderInjectsEnvironment(t *testing.T) {
	provider := &commandActionProvider{
		capability: "structured-actions",
		command:    []string{"/bin/sh", "-c", `cat >/dev/null; printf '{"workspace":"%s","repo":"%s","team":"%s"}' "$HUFU_WORKSPACE" "$HUFU_REPOSITORY" "$HUFU_TEAM"`},
	}
	ctx := WithActionEnvironment(context.Background(), ActionEnvironment{
		Workspace:  "/test/workspace/my-team",
		Repository: "/test/repo",
		TeamName:   "my-team",
	})
	result, err := provider.Execute(ctx, Action{Capability: "structured-actions", Type: "apply"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	resMap, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not map: %#v", result)
	}
	if resMap["workspace"] != "/test/workspace/my-team" || resMap["repo"] != "/test/repo" || resMap["team"] != "my-team" {
		t.Fatalf("injected environment mismatch: %#v", resMap)
	}
}

func TestCommandActionProviderRunsStrictResolverEnvelope(t *testing.T) {
	provider := &commandActionProvider{
		capability: "resolve-demo",
		command:    []string{"/bin/sh", "-c", `cat >/dev/null; printf '{"status":"matched","value":3,"evidence":[{"source":"prompt","start":7,"end":8,"kind":"count"}],"resolver_version":"1"}'`},
	}
	response, err := provider.ResolveRunInput(t.Context(), RunInputResolverRequest{
		Type: "resolve_run_input", InputName: "count", Prompt: "last 3", SchemaHash: "sha256:fixture", ResolverID: "count-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "matched" || string(response.Value) != "3" || response.ResolverVersion != "1" {
		t.Fatalf("response = %#v", response)
	}

	provider.command = []string{"/bin/sh", "-c", `cat >/dev/null; printf '{"status":"matched","value":3,"resolver_version":"1","unknown":true}'`}
	if _, err := provider.ResolveRunInput(t.Context(), RunInputResolverRequest{}); err == nil {
		t.Fatal("resolver provider accepted an unknown response field")
	}
}

// newActionProviderCoordinator builds a coordinator whose workflow has reached
// the execute phase and whose "structured-actions" capability is served by
// provider. The returned session is the coordinator's own.
func newActionProviderCoordinator(t *testing.T, provider ActionProvider, runID string) (*Coordinator, *TeamSession) {
	t.Helper()
	session := workflowTestSession(t)
	session.Dir = t.TempDir()
	registry := NewProviderRegistry()
	registry.Register("structured-actions", provider)
	session.ProviderRegistry = registry
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	return &Coordinator{session: session, projectDir: session.Dir, taskTracker: NewTaskTracker(), phaseWorkflow: w, executionRunID: runID}, session
}

// newCommandActionCoordinator serves the structured-actions capability with a
// shell command provider.
func newCommandActionCoordinator(t *testing.T, command []string, runID string) (*Coordinator, *TeamSession) {
	t.Helper()
	return newActionProviderCoordinator(t, &commandActionProvider{capability: "structured-actions", command: command}, runID)
}

// structuredActionTask is the execute-phase task every runtime action test
// dispatches.
func structuredActionTask() TaskDef {
	return TaskDef{Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}}
}

func TestCoordinatorRuntimeActionPersistsProviderArtifactsAsTypedEvidence(t *testing.T) {
	provider := &recordingActionProvider{result: ActionResult{
		Outputs:   map[string]any{"count": 3},
		Artifacts: []ArtifactRef{{ID: "provider-forged", Path: "result.json", SHA256: strings.Repeat("0", 64)}},
	}}
	c, session := newActionProviderCoordinator(t, provider, "run-action-artifact")
	if err := os.WriteFile(filepath.Join(session.Workspace, "result.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := NewEventStore(session.Workspace, "run-action-artifact", "session-action-artifact")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	c.eventStore = events
	c.SetEventJournal(eventStoreJournal{store: events})
	task, item := createAdmittedTestTask(t, c, TaskDef{ID: "execute", Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}})
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatal(err)
	}
	got := c.taskTracker.TodoList().Items()[0]
	// The runtime recomputes artifact identity instead of trusting the
	// provider's claim, and keeps provider outputs typed.
	if got.TypedResult == nil || len(got.TypedResult.Artifacts) != 1 || got.TypedResult.Artifacts[0].ID == "provider-forged" {
		t.Fatalf("typed provider artifact = %#v", got.TypedResult)
	}
	if !equalJSONValues(got.TypedResult.RuntimeOutputs["count"], 3) || got.TypedResult.RuntimeOutputsHash == "" || got.TypedResult.Facts != nil {
		t.Fatalf("typed provider outputs = %#v (hash %q)", got.TypedResult.RuntimeOutputs, got.TypedResult.RuntimeOutputsHash)
	}
	stored, err := events.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var artifactEvent RunEvent
	for _, event := range stored {
		if event.Type == "artifact_created" {
			artifactEvent = event
			break
		}
	}
	if artifactEvent.Type == "" {
		t.Fatal("provider artifact_created event missing")
	}
	var payload struct {
		Artifact ArtifactRef `json:"artifact"`
	}
	if err := json.Unmarshal(artifactEvent.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Artifact.Provider != "fake-action-adapter" || payload.Artifact.RunID != "run-action-artifact" {
		t.Fatalf("artifact event provenance = %#v", payload.Artifact)
	}
}

func TestDecodeActionResultEnvelopeAndRejectsUndeclaredFields(t *testing.T) {
	result, err := decodeActionResult(map[string]any{
		"outputs": map[string]any{"count": float64(3)},
		"artifacts": []any{
			map[string]any{"path": "one.json"},
			map[string]any{"path": "two.json"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outputs["count"] != float64(3) || len(result.Artifacts) != 2 {
		t.Fatalf("decoded action result = %#v", result)
	}
	if _, err := decodeActionResult(map[string]any{"outputs": map[string]any{}, "unexpected": true}); err == nil {
		t.Fatal("undeclared action result field unexpectedly accepted")
	}
}

// actionRootWithFiles creates the action namespace root under the workspace
// and writes the named files into it.
func actionRootWithFiles(t *testing.T, workspace string, files map[string]string) string {
	t.Helper()
	actionRoot := filepath.Join(workspace, "runtime", "runs", "run-1", "actions", "action-1")
	if err := os.MkdirAll(actionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(actionRoot, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return actionRoot
}

func TestIngestActionProviderArtifactsRecomputesIdentityAndProvenance(t *testing.T) {
	c := newTestCoordinator(t)
	c.executionRunID = "run-1"
	actionRoot := actionRootWithFiles(t, c.session.Workspace, map[string]string{"one.json": `{"one":1}`, "two.json": `{"two":2}`})
	refs, err := c.ingestActionProviderArtifacts(context.Background(), actionRoot, TaskDef{Agent: "producer"}, "task-1", 3, []ArtifactRef{
		{ID: "forged", Path: "one.json", SHA256: strings.Repeat("0", 64)},
		{Path: "two.json", Description: "second"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].ID == "forged" || refs[0].SHA256 == strings.Repeat("0", 64) {
		t.Fatalf("runtime did not recompute artifact identity: %#v", refs)
	}
	for _, ref := range refs {
		if ref.RunID != "run-1" || ref.TaskID != "task-1" || ref.Agent != "producer" || ref.Attempt != 3 {
			t.Fatalf("artifact provenance = %#v", ref)
		}
	}
}

func TestIngestActionProviderArtifactsRejectsMissingAndEscapedPaths(t *testing.T) {
	c := newTestCoordinator(t)
	c.executionRunID = "run-1"
	actionRoot := actionRootWithFiles(t, c.session.Workspace, nil)
	insideSession := filepath.Join(c.session.Workspace, "stable.json")
	if err := os.WriteFile(insideSession, []byte("durable"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"missing.json", "../outside.json", insideSession} {
		t.Run(path, func(t *testing.T) {
			if _, err := c.ingestActionProviderArtifacts(context.Background(), actionRoot, TaskDef{Agent: "producer"}, "task-1", 1, []ArtifactRef{{Path: path}}); err == nil {
				t.Fatalf("path %q unexpectedly accepted", path)
			}
		})
	}
}

func TestRuntimeActionExecutionTelemetry(t *testing.T) {
	for _, test := range []struct {
		name     string
		command  []string
		wantErr  bool
		terminal string
	}{
		{name: "success", command: []string{"/bin/sh", "-c", `printf '{"outputs":{}}'`}, terminal: "done"},
		{name: "failure", command: []string{"/bin/sh", "-c", "exit 42"}, wantErr: true, terminal: "error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runID := "run-action-telemetry-" + test.name
			c, session := newCommandActionCoordinator(t, test.command, runID)
			store, err := NewEventStore(session.Workspace, runID, "session-action-telemetry")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			c.eventStore = store
			c.SetEventJournal(eventStoreJournal{store: store})
			if err := c.emitEvent("run_started", "coordinator", "", LifecycleEventPayload{Team: session.Config.Name}); err != nil {
				t.Fatal(err)
			}

			task := structuredActionTask()
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "parity", Phase: PhaseExecute, ContractID: "execute", Action: task.Action, Agent: task.Agent, Desc: task.Goal, Model: "configured-model"}})[0]
			_, actionErr := c.executeRuntimeAction(context.Background(), task, item.ID)
			if (actionErr != nil) != test.wantErr {
				t.Fatalf("executeRuntimeAction error=%v, wantErr=%v", actionErr, test.wantErr)
			}

			canonical, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			var statuses []string
			for _, row := range projectEvents(canonical) {
				if row.TaskID == "" {
					continue
				}
				// An action runs a provider command, not a model.
				if row.Model != "" || row.Team != session.Config.Name {
					t.Fatalf("action row = %+v, want no model and team %q", row, session.Config.Name)
				}
				statuses = append(statuses, row.Status)
			}
			if len(statuses) == 0 || statuses[0] != "in_progress" || statuses[len(statuses)-1] != test.terminal {
				t.Fatalf("action statuses = %v, want in_progress ... %s", statuses, test.terminal)
			}
		})
	}
}

func TestRuntimeActionCommandProviderGetsFreshNamespacesAndPreservesEvidence(t *testing.T) {
	command := []string{"/bin/sh", "-c", `set -eu
if [ ! -d "$HUFU_REPOSITORY" ] || [ "$HUFU_REPOSITORY" = "$HUFU_WORKSPACE" ]; then
  echo "provider did not receive distinct repository and action workspace roots" >&2
  exit 40
fi
if [ "$HUFU_REPOSITORY" != "$HUFU_EXPECTED_REPOSITORY" ]; then
  echo "provider did not receive the coordinator subject root" >&2
  exit 42
fi
if [ -n "$(find "$HUFU_WORKSPACE" -mindepth 1 -print -quit)" ]; then
  echo "provider workspace is not empty" >&2
  exit 41
fi
printf '%s' "$HUFU_ACTION_INVOCATION_ID" > "$HUFU_WORKSPACE/artifact.txt"
printf '{"outputs":{"action_id":"%s"},"artifacts":[{"path":"artifact.txt","kind":"provider-output"}]}' "$HUFU_ACTION_INVOCATION_ID"
	`}
	c, session := newCommandActionCoordinator(t, command, "run-command")
	t.Setenv("HUFU_EXPECTED_REPOSITORY", session.Dir)
	oldEvidence := filepath.Join(session.Workspace, "workset", "workset-manifest.json")
	if err := os.MkdirAll(filepath.Dir(oldEvidence), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldEvidence, []byte("old evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := structuredActionTask()
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{PlanTaskID: "action-1", Phase: PhaseExecute, ContractID: "execute-1", Action: task.Action, Agent: task.Agent, Desc: task.Goal},
		{PlanTaskID: "action-2", Phase: PhaseExecute, ContractID: "execute-2", Action: task.Action, Agent: task.Agent, Desc: task.Goal},
	})
	for _, item := range items {
		if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
			t.Fatalf("executeRuntimeAction(%s): %v", item.ID, err)
		}
	}
	roots, err := filepath.Glob(filepath.Join(session.Workspace, "runtime", "runs", "run-command", "actions", "structured-action-*"))
	if err != nil || len(roots) != 2 {
		t.Fatalf("action roots = %v, err=%v; want two unique roots", roots, err)
	}
	refs := make([]ArtifactRef, 0, 2)
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.TypedResult == nil || len(item.TypedResult.Artifacts) != 1 {
			t.Fatalf("typed result for %s = %#v", item.ID, item.TypedResult)
		}
		ref := item.TypedResult.Artifacts[0]
		if !strings.HasPrefix(ref.Path, "runtime/runs/run-command/actions/structured-action-") || ref.RunID != "run-command" || ref.TaskID != item.ID || ref.Attempt != 1 {
			t.Fatalf("artifact provenance for %s = %#v", item.ID, ref)
		}
		refs = append(refs, ref)
	}
	if refs[0].ID == refs[1].ID || refs[0].Path == refs[1].Path {
		t.Fatalf("artifact identities or paths were reused: %#v", refs)
	}
	if content, err := os.ReadFile(oldEvidence); err != nil || string(content) != "old evidence" {
		t.Fatalf("old evidence changed: content=%q err=%v", content, err)
	}
}

func TestRuntimeActionRetryKeepsFailedResidueAndAllocatesNewNamespace(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "provider-marker")
	command := []string{"/bin/sh", "-c", fmt.Sprintf(`set -eu
if [ ! -f %q ]; then
  printf partial > "$HUFU_WORKSPACE/partial.txt"
  touch %q
  exit 42
fi
printf success > "$HUFU_WORKSPACE/artifact.txt"
printf '{"artifacts":[{"path":"artifact.txt","kind":"provider-output"}]}'
`, marker, marker)}
	c, session := newCommandActionCoordinator(t, command, "run-retry")
	task := structuredActionTask()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "retry", Phase: PhaseExecute, ContractID: "execute", Action: task.Action, Agent: task.Agent, Desc: task.Goal}})[0]
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err == nil {
		t.Fatal("failed provider attempt unexpectedly succeeded")
	}
	actionRoots := func() []string {
		roots, err := filepath.Glob(filepath.Join(session.Workspace, "runtime", "runs", "run-retry", "actions", "structured-action-*"))
		if err != nil {
			t.Fatal(err)
		}
		return roots
	}
	firstRoots := actionRoots()
	if len(firstRoots) != 1 {
		t.Fatalf("failed-attempt roots = %v", firstRoots)
	}
	if _, err := os.Stat(filepath.Join(firstRoots[0], "partial.txt")); err != nil {
		t.Fatalf("failed-attempt residue missing: %v", err)
	}
	if err := c.CommitTaskResetForRetry(context.Background(), item.ID, "retry provider action"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
		t.Fatalf("retry provider attempt failed: %v", err)
	}
	if allRoots := actionRoots(); len(allRoots) != 2 || allRoots[0] == allRoots[1] {
		t.Fatalf("retry roots = %v; want two preserved distinct roots", allRoots)
	}
}
