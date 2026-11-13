package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/golangruntime"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func actionRecoveryProvider(digest string, fail bool, calls *int, origins *map[string]string, captured ...*string) *golangActionProvider {
	return &golangActionProvider{capability: "structured-actions", program: golangruntime.Program{Source: "/fixture/action", Digest: runInputHash([]byte(digest))}, execute: func(_ context.Context, _ golangruntime.Program, input []byte, env []string, _, _ int) (golangruntime.Result, error) {
		*calls++
		if len(captured) > 0 {
			*captured[0] = string(input)
		}
		values := map[string]string{}
		for _, value := range env {
			key, value, found := strings.Cut(value, "=")
			if found {
				values[key] = value
			}
		}
		if origins != nil {
			if err := json.Unmarshal([]byte(values["HUFU_RECOVERED_TASK_RUNS"]), origins); err != nil {
				return golangruntime.Result{}, err
			}
		}
		if fail && values["HUFU_TASK_ID"] == "8" {
			err := exec.Command("/bin/sh", "-c", "exit 1").Run()
			return golangruntime.Result{Stderr: []byte("old provider defect")}, err
		}
		if err := os.WriteFile(filepath.Join(values["HUFU_WORKSPACE"], "evidence.txt"), []byte("immutable evidence"), 0600); err != nil {
			return golangruntime.Result{}, err
		}
		return golangruntime.Result{Stdout: []byte(`{"outputs":{"verified":true},"artifacts":[{"path":"evidence.txt","kind":"evidence"}]}`)}, nil
	}}
}

func nativeActionRecoveryFixture(t *testing.T, payload ...string) (*Coordinator, *versionFixture, *ResumeSource, *int) {
	t.Helper()
	f := &versionFixture{t: t, control: t.TempDir(), subject: t.TempDir()}
	f.version = WorkspaceVersionContext{Mode: versionstore.ModeOff, WorkspaceID: "action-workspace", ProjectID: "action-project", TeamName: "generic-action", SubjectRoot: f.subject, ControlRoot: f.control, StateDir: t.TempDir()}
	var err error
	f.events, err = NewEventStore(f.control, "source-run", "generic-action")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.closeStores)
	f.events.SetBranchID("main")
	f.tree, err = LoadSessionTree(f.control)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveSessionTree(f.control, f.tree); err != nil {
		t.Fatal(err)
	}
	c := newTestCoordinator(t)
	c.session.Workspace = f.control
	c.session.Config = workflowTestSession(t).Config
	c.session.Config.Name = "generic-action"
	c.session.Config.DefaultLLMBackend = "ollama"
	c.session.WorkspaceVersion = f.version
	if err = c.session.SetCompatibilityWorkspaceScope(f.subject); err != nil {
		t.Fatal(err)
	}
	c.projectDir = f.subject
	c.eventStore = f.events
	c.executionRunID = "source-run"
	c.SetEventJournal(eventStoreJournal{store: f.events})
	c.session.Agents["worker"].Generation.Model = "ollama/test"
	c.SetExecutionProfile(BuiltinProfiles()[ProfileFreshSession])
	c.session.LaunchOptions = &LaunchOptions{ExecutionProfile: string(ProfileFreshSession)}
	c.session.Config.ExecutionProfile = string(ProfileFreshSession)
	c.session.RunInputDefinitions = []RunInputDefinition{{Name: "scope", Schema: RunInputSchema{Type: "object"}, Required: true, Default: []byte(`{"count":2}`), Resolver: &RunInputResolverSpec{ID: "scope-v1", Capability: "structured-actions", Type: "scope", Source: "invocation_prompt", SideEffect: "none", Timeout: 5}}}
	c.session.Config.ActionProviders = map[string]agent.ActionProviderConfig{"structured-actions": {Runtime: "golang", Source: "./action", Mode: golangruntime.TrustedStaticMode}}
	c.session.ProviderRegistry = NewProviderRegistry()
	calls := new(int)
	c.session.ProviderRegistry.Register("structured-actions", actionRecoveryProvider("original", true, calls, nil))
	flow := workflowTestSession(t)
	flow.Workspace = f.control
	if err = flow.SetCompatibilityWorkspaceScope(f.subject); err != nil {
		t.Fatal(err)
	}
	c.phaseWorkflow = executePhaseWorkflow(t, flow)
	c.phaseWorkflow.registry = c.session.ProviderRegistry
	for i := range 8 {
		c.session.ContractTasks = append(c.session.ContractTasks, TaskDef{ID: fmt.Sprintf("native-%d", i), Agent: "worker", Goal: "read frozen evidence", Phase: PhaseExecute, SideEffect: SideEffectNone, Recovery: RecoveryRetry, Verify: "true", Action: &Action{Capability: "structured-actions", Type: "read", Payload: `{"scope":{}}`, InputBindings: []ActionInputBinding{{Input: "scope", Target: "/scope"}}}})
	}
	if len(payload) > 0 {
		for i := range c.session.ContractTasks {
			c.session.ContractTasks[i].Action.Payload = payload[0]
		}
	}
	manager, err := agent.NewProviderManager("http://127.0.0.1:1/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = manager
	if err = c.prepareRecoveryValidation(); err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, f.events, RunEvent{Type: string(EventExecutionPolicySnapshot), Payload: mustProjectionJSON(t, c.ExecutionPolicySnapshot())})
	snapshot, err := ResolveRunInputSnapshot(c.session.RunInputDefinitions, nil, "source-run", "source-invocation", "generic-action")
	if err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, f.events, RunEvent{Type: string(EventRunInputsResolved), RunID: "source-run", Payload: mustProjectionJSON(t, snapshot)})
	c.SetSessionData(&SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID})
	definitions := append([]TaskDef(nil), c.session.ContractTasks...)
	for i := range definitions {
		definition := &definitions[i]
		definition.ContractID = definition.ID
		definition.ContractRevision = effectiveTaskContractRevision
		definition.ContractHash, err = effectiveContractHash(definition.ID, definition.Agent, definition.Execution, definition.OutputMode, definition.SideEffect, definition.Recovery, definition.MaxRetries, definition.Action, definition.FanOut, definition.Optional, *definition)
		if err != nil {
			t.Fatal(err)
		}
	}
	definitions, err = c.materializeTaskActions(definitions)
	if err != nil {
		t.Fatal(err)
	}
	for i, definition := range definitions {
		task, item := createAdmittedTestTask(t, c, definition)
		_, err := c.executeRuntimeAction(t.Context(), task, item.ID)
		if (err != nil) != (i == 7) {
			t.Fatalf("native %d: %v", i, err)
		}
	}
	appendBranchEvent(t, f.events, RunEvent{Type: string(EventWorkflowStateChanged), RunID: "source-run", Payload: []byte(`{"state":"FAILED"}`)})
	if err = SaveSession(f.control, ReduceToSessionData(mustReadEvents(t, f.events))); err != nil {
		t.Fatal(err)
	}
	source, err := ReadResumeSource(f.control, "main", "source-run")
	if err != nil {
		t.Fatal(err)
	}
	c.session.ProviderRegistry.Register("structured-actions", actionRecoveryProvider("fixed", false, new(int), nil))
	return c, f, source, calls
}

func TestContractRecoveryActionRetainsCompletedTasksAndResumesChild(t *testing.T) {
	testNativeActionRecoveryChild(t, false)
}

func testNativeActionRecoveryChild(t *testing.T, rebuild bool) {
	t.Helper()
	fixture := nativeActionRecoveryFixture
	if rebuild {
		fixture = func(t *testing.T, _ ...string) (*Coordinator, *versionFixture, *ResumeSource, *int) {
			return legacyNativeActionRecoveryFixture(t)
		}
	}
	c, f, source, oldCalls := fixture(t)
	fixedCalls := new(int)
	var origins map[string]string
	var captured string
	c.session.ProviderRegistry.Register("structured-actions", actionRecoveryProvider("fixed", false, fixedCalls, &origins, &captured))
	ctx := WithReadOnlyActionRepair(WithMetadataOnlyContractRecovery(t.Context()), "8")
	if rebuild {
		if _, err := c.AssessContractRecovery(ctx, source); err == nil {
			t.Fatal("historical reconstruction did not require explicit selection")
		}
		ctx = WithActionPayloadReconstruction(ctx)
	}
	original := mustReadEvents(t, f.events)
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if rebuild {
		for _, patch := range plan.Tasks {
			if patch.PayloadReconstruction == nil || !strings.Contains(source.Session.Tasks[0].Action.Payload, "[REDACTED]") {
				t.Fatal("missing immutable reconstruction proof")
			}
		}
	}
	if plan.Retained != 7 || plan.Retried != 1 || plan.ActionRepair == nil || *oldCalls != 8 || *fixedCalls != 0 {
		t.Fatalf("plan=%#v calls=%d", plan, *oldCalls)
	}
	if !reflect.DeepEqual(original, mustReadEvents(t, f.events)) {
		t.Fatal("preview mutated history")
	}
	if _, err = c.ApplyContractRecovery(ctx, source, "bad-plan", strings.Repeat("0", 64)); err == nil {
		t.Fatal("stale plan accepted")
	}
	c.session.ProviderRegistry.Register("structured-actions", actionRecoveryProvider("changed-again", false, new(int), nil))
	if _, err = c.ApplyContractRecovery(ctx, source, "changed-program", plan.SHA256); err == nil {
		t.Fatal("program changed after preview was accepted")
	}
	c.session.ProviderRegistry.Register("structured-actions", actionRecoveryProvider("fixed", false, fixedCalls, &origins, &captured))
	branch, err := c.ApplyContractRecovery(ctx, source, "fixed-action", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parent.Session.Tasks, source.Session.Tasks) {
		t.Fatal("parent work changed")
	}
	child, err := ReadResumeSource(f.control, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range child.Session.Tasks {
		if task.ID == "8" {
			if task.Status != TaskPending {
				t.Fatal("failed action not reopened")
			}
		} else if task.Status != TaskDone {
			t.Fatal("completed work replayed")
		}
	}
	fresh := newTestCoordinator(t)
	fresh.executionRunID = ""
	copySession := *c.session
	fresh.session = &copySession
	fresh.projectDir = f.subject
	fresh.providerManager = c.providerManager
	fresh.SetExecutionProfile(BuiltinProfiles()[ProfileFreshSession])
	if err = fresh.AuthorizeSessionRestore(child); err != nil {
		t.Fatal(err)
	}
	fresh.SetSessionData(child.Session)
	if len(fresh.taskTracker.TodoList().Items()) != 8 {
		t.Fatal("fresh-session child lost task state")
	}
	if err = fresh.prepareRecoveryValidation(); err != nil {
		t.Fatal(err)
	}
	assessment, err := fresh.AssessResumeSource(t.Context(), child)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Accepted != 7 || assessment.Resumable != 1 || assessment.TerminalUnresolved != 0 {
		t.Fatalf("assessment=%#v", assessment)
	}
	if *fixedCalls != 0 {
		t.Fatal("preview/publication executed provider")
	}
	fresh.session.ProviderRegistry.Register("structured-actions", actionRecoveryProvider("changed-after-publication", false, new(int), nil))
	if err := fresh.validateResumeRecoveryMode(t.Context(), child); err == nil {
		t.Fatal("child accepted unapproved provider source")
	}
	fresh.session.ProviderRegistry.Register("structured-actions", actionRecoveryProvider("fixed", false, fixedCalls, &origins, &captured))

	fresh.executionRunID = "child-execution"
	fresh.eventStore, err = NewEventStore(f.control, fresh.executionRunID, "generic-action")
	if err != nil {
		t.Fatal(err)
	}
	fresh.eventStore.SetBranchID(branch)
	fresh.SetEventJournal(eventStoreJournal{store: fresh.eventStore})
	flow := workflowTestSession(t)
	flow.Workspace = f.control
	if err = flow.SetCompatibilityWorkspaceScope(f.subject); err != nil {
		t.Fatal(err)
	}
	fresh.phaseWorkflow = executePhaseWorkflow(t, flow)
	fresh.phaseWorkflow.registry = fresh.session.ProviderRegistry
	admission, found, err := loadDecisionAdmission(t.Context(), eventStoreJournal{store: fresh.eventStore}, "8", 1)
	if err != nil || !found {
		t.Fatalf("original admission: %v", err)
	}
	for _, name := range []string{"goal", "payload", "snapshot", "contract"} {
		item := cloneTodoItem(fresh.todoItemByID("8"))
		switch name {
		case "goal":
			item.Goal = "unapproved goal"
		case "payload":
			item.Action.Payload = `{}`
		case "snapshot":
			item.RunInputSnapshotHash = runInputHash([]byte("other"))
		case "contract":
			item.ContractHash = runInputHash([]byte("other"))
		}
		matches, err := actionRepairDecisionAdmissionMatches(t.Context(), eventStoreJournal{store: fresh.eventStore}, item, admission)
		if err != nil || matches {
			t.Fatalf("altered %s admitted: matches=%v err=%v", name, matches, err)
		}
	}
	resumeCtx := WithSessionResume(t.Context())
	if err = fresh.resolveRunInputsForInvocation(resumeCtx, "resume"); err != nil {
		t.Fatal(err)
	}
	if err = fresh.validateResumeSources(resumeCtx); err != nil {
		t.Fatal(err)
	}
	retained := fresh.todoItemByID("1")
	if err = fresh.validateRecoveredArtifactOrigin(retained.TypedResult.Artifacts[0], retained.ID, retained.TypedResult); err != nil {
		t.Fatal(err)
	}
	resumed, err := fresh.ResumeInterruptedTasks(resumeCtx)
	if err != nil {
		t.Fatal(err)
	}
	if resumed != 1 || *fixedCalls != 1 || *oldCalls != 8 || origins["1"] != "source-run" {
		t.Fatalf("resumed=%d calls=%d origins=%v", resumed, *fixedCalls, origins)
	}
	if rebuild {
		var request Action
		if err := json.Unmarshal([]byte(captured), &request); err != nil {
			t.Fatal(err)
		}
		if request.Payload != fresh.todoItemByID("8").Action.Payload || !strings.Contains(request.Payload, `"max_analysis_input_tokens":12000`) || strings.Contains(request.Payload, "[REDACTED]") {
			t.Fatalf("provider did not receive exact reconstructed payload: %s", request.Payload)
		}
	}

	for _, task := range fresh.taskTracker.TodoList().Items() {
		if task.Status != TaskDone {
			t.Fatalf("unfinished task %s: %s", task.ID, task.Status)
		}
	}
	parent, err = ReadResumeSource(f.control, "main", "")
	if err != nil || !reflect.DeepEqual(parent.Session.Tasks, source.Session.Tasks) {
		t.Fatalf("execution changed parent: %v", err)
	}
}

func TestContractRecoveryActionRefusesChangedAuthorityOrLostEvidence(t *testing.T) {
	for _, name := range []string{"unchanged_program", "unknown_family", "model", "static_payload", "schema", "stored_payload", "source_head", "cancelled", "missing_failure", "side_effect"} {
		t.Run(name, func(t *testing.T) {
			c, f, source, calls := nativeActionRecoveryFixture(t)
			ctx := WithReadOnlyActionRepair(WithMetadataOnlyContractRecovery(t.Context()), "8")
			switch name {
			case "unchanged_program":
				c.session.ProviderRegistry.Register("structured-actions", actionRecoveryProvider("original", false, new(int), nil))
			case "unknown_family":
				c.session.Config.ActionProviders["unobserved"] = agent.ActionProviderConfig{Runtime: "golang", Source: "./other", Mode: golangruntime.TrustedStaticMode}
				c.session.ProviderRegistry.Register("unobserved", actionRecoveryProvider("unrelated", false, new(int), nil))
			case "model":
				c.session.Agents["worker"].Generation.Model = "ollama/other"
			case "static_payload":
				c.session.ContractTasks[0].Action.Payload = `{"scope":{},"limit":99}`
			case "schema":
				c.session.RunInputDefinitions[0].Schema.Type = "string"
			case "stored_payload":
				source.Session.Tasks[0].Action.Payload = `{"scope":"[REDACTED]"}`
			case "source_head":
				appendBranchEvent(t, f.events, RunEvent{Type: "observation", Payload: []byte(`{}`)})
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "missing_failure":
				source.Session.Tasks[7].FailureEvent = nil
			case "side_effect":
				source.Session.Tasks[7].SideEffect = SideEffectExternalWrite
			}
			before := mustReadEvents(t, f.events)
			plan, err := c.AssessContractRecovery(ctx, source)
			if name == "source_head" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = c.ApplyContractRecovery(ctx, source, "stale-source", plan.SHA256)
			}
			if err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if *calls != 8 || !reflect.DeepEqual(before, mustReadEvents(t, f.events)) {
				t.Fatal("refusal executed work or changed history")
			}
		})
	}
}

func TestContractRecoveryActionReplayRejectsAlteredProof(t *testing.T) {
	c, f, source, _ := nativeActionRecoveryFixture(t)
	ctx := WithReadOnlyActionRepair(WithMetadataOnlyContractRecovery(t.Context()), "8")
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	before := mustReadEvents(t, f.events)
	for _, name := range []string{"failure_hash", "original_program", "accepted_program", "bindings", "unrelated_retry", "input_hash", "schema"} {
		t.Run(name, func(t *testing.T) {
			var changed ContractRecoveryPlan
			if err := json.Unmarshal(mustProjectionJSON(t, plan), &changed); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "failure_hash":
				changed.ActionRepair.FailureEventHash = strings.Repeat("a", 64)
			case "original_program":
				changed.ActionRepair.OriginalProvider = "golang:" + runInputHash([]byte("other"))
			case "accepted_program":
				changed.ActionRepair.AcceptedProvider = changed.ActionRepair.OriginalProvider
			case "bindings":
				changed.ActionRepair.ProviderBindings["structured-actions"] = "golang:" + runInputHash([]byte("other"))
			case "unrelated_retry":
				changed.Tasks[0].Action = "retry"
				changed.Retained--
				changed.Retried++
			case "input_hash":
				changed.InputPolicyUpgrade.SnapshotSHA256 = strings.Repeat("a", 64)
			case "schema":
				changed.Policy.RunInputSchemaHash = runInputHash([]byte("other"))
				changed.Policy.ConfigurationHash, _ = executionPolicyConfigurationHash(changed.Policy)
			}
			changed.SHA256, err = recoveryPlanHash(changed)
			if err != nil {
				t.Fatal(err)
			}
			event := RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventContractRecovery), BranchID: "child", Payload: mustProjectionJSON(t, changed)}
			if _, err := validateContractRecoveryLineage(event, before, source.Session.ExecutionPolicySnapshot); err == nil {
				t.Fatal("altered proof accepted on replay")
			}
		})
	}
}

func TestContractRecoveryActionUnpublishedChildKeepsOriginalPolicy(t *testing.T) {
	c, f, source, calls := nativeActionRecoveryFixture(t)
	// Simulate stopping between the existing metadata fork transaction and
	// recovery publication. No repair proof has reached the child yet.
	branch, err := ForkSessionBranch(t.Context(), f.control, f.tree, f.events, "unpublished-action", "main")
	if err != nil {
		t.Fatal(err)
	}
	child, err := ReadResumeSource(f.control, branch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if child.actionProviders != nil || child.Session.ExecutionPolicySnapshot.ConfigurationHash != source.Session.ExecutionPolicySnapshot.ConfigurationHash || !sameCanonicalEvidence(child.Session.Tasks, source.Session.Tasks) {
		t.Fatal("unpublished child gained repair authority")
	}
	ctx := WithReadOnlyActionRepair(WithMetadataOnlyContractRecovery(t.Context()), "8")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = c.ApplyContractRecovery(cancelled, child, "cancelled-repair", strings.Repeat("a", 64)); err == nil {
		t.Fatal("cancelled publication accepted")
	}
	parent, err := ReadResumeSource(f.control, "main", "")
	if err != nil || !sameCanonicalEvidence(parent.Session.Tasks, source.Session.Tasks) || *calls != 8 {
		t.Fatalf("parent lost work: %v", err)
	}
	for _, event := range mustReadEvents(t, f.events) {
		if event.Type == string(EventContractRecovery) {
			t.Fatal("interruption or cancellation committed a recovery grant")
		}
	}
}

func TestContractRecoveryActionSessionRestoreRequiresExactRequest(t *testing.T) {
	c, _, source, _ := nativeActionRecoveryFixture(t)
	fresh := newTestCoordinator(t)
	fresh.executionRunID = ""
	copySession := *c.session
	fresh.session = &copySession
	fresh.SetExecutionProfile(BuiltinProfiles()[ProfileFreshSession])
	fresh.SetSessionData(source.Session)
	if len(fresh.taskTracker.TodoList().Items()) != 0 {
		t.Fatal("new request restored historical tasks")
	}
	c.executionRunID = ""
	for _, name := range []string{"changed_projection", "other_request", "wrong_profile"} {
		t.Run(name, func(t *testing.T) {
			copySource := *source
			var session SessionData
			if err := json.Unmarshal(mustProjectionJSON(t, source.Session), &session); err != nil {
				t.Fatal(err)
			}
			copySource.Session = &session
			switch name {
			case "changed_projection":
				session.Tasks[0].Goal = "different"
			case "other_request":
				session.Tasks[0].RunInputSnapshotID = "another-request"
				copySource.sessionSHA256, _ = collectionDigest(&session)
			case "wrong_profile":
				session.ExecutionPolicySnapshot.Launch.Options.ExecutionProfile = "other-profile"
				copySource.sessionSHA256, _ = collectionDigest(&session)
			}
			if err := c.AuthorizeSessionRestore(&copySource); err == nil {
				t.Fatal("unbound restore accepted")
			}
		})
	}
	if err := c.AuthorizeSessionRestore(source); err != nil {
		t.Fatal(err)
	}
	c.SetFreshSession(true)
	if c.restoresSessionTasks() {
		t.Fatal("fresh request retained explicit restore authorization")
	}
}

func TestContractRecoveryActionRefusesRedactedHistoricalPayload(t *testing.T) {
	c, f, source, _ := legacyNativeActionRecoveryFixture(t)
	if !strings.Contains(source.Session.Tasks[0].Action.Payload, "[REDACTED]") {
		t.Fatal("fixture did not reproduce historical redaction")
	}
	before := mustReadEvents(t, f.events)
	ctx := WithReadOnlyActionRepair(WithMetadataOnlyContractRecovery(t.Context()), "8")
	if _, err := c.AssessContractRecovery(ctx, source); err == nil || !strings.Contains(err.Error(), "stored frozen action payload") {
		t.Fatalf("expected lost payload refusal: %v", err)
	}
	if !sameCanonicalEvidence(before, mustReadEvents(t, f.events)) {
		t.Fatal("refusal modified historical evidence")
	}
}
