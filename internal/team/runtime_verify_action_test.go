package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkpointActionProvider inspects the actual persisted occurrence written by
// ExecuteTasks, rather than a hand-built provider checkpoint.
type checkpointActionProvider struct {
	workspace string
	passed    bool
	calls     int
}

func (*checkpointActionProvider) Validate(Action) error { return nil }

func (p *checkpointActionProvider) Execute(ctx context.Context, action Action) (any, error) {
	p.calls++
	env := ActionEnvironmentFromContext(ctx)
	if env.TaskID == "" {
		return nil, fmt.Errorf("missing action environment")
	}
	data, err := os.ReadFile(filepath.Join(p.workspace, "session.json"))
	if err != nil {
		return nil, err
	}
	var saved SessionData
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	item := todoItemByID(saved.Tasks, env.TaskID)
	if item == nil || item.Status != TaskInProgress || item.RunInputSnapshotID == "" || item.RunInputSnapshotID != saved.ActiveRunInputSnapshotID {
		return nil, fmt.Errorf("action occurrence is not bound in checkpoint")
	}
	if action.Type == "inspect" && saved.WorkflowState != PhaseVerify {
		return nil, fmt.Errorf("inspection ran outside VERIFY")
	}
	return ActionResult{Outputs: map[string]any{"passed": p.passed}}, nil
}

func TestCoordinatorRunsBoundPrepareAndReadOnlyVerifyActions(t *testing.T) {
	for _, passed := range []bool{true, false} {
		t.Run(fmt.Sprintf("passed=%v", passed), func(t *testing.T) {
			session := workflowTestSession(t)
			session.Config.Workflow.Phases = []string{"prepare", "verify"}
			session.Config.Policies.AllowPhaseSkip = true
			session.Config.Verification.Required = true
			session.RunInputDefinitions = []RunInputDefinition{{Name: "scope", Schema: RunInputSchema{Type: "object"}, Required: true}}
			session.ContractTasks = []TaskDef{
				{ID: "produce", Agent: "preparer", Phase: PhasePrepare, SideEffect: SideEffectNone, Action: &Action{Capability: "structured-actions", Type: "prepare", Payload: `{"scope":{}}`, InputBindings: []ActionInputBinding{{Input: "scope", Target: "/scope"}}}},
				{ID: "inspect", Agent: "verifier", Phase: PhaseVerify, SideEffect: SideEffectNone, Action: &Action{Capability: "structured-actions", Type: "inspect", Payload: `{"scope":{}}`, InputBindings: []ActionInputBinding{{Input: "scope", Target: "/scope"}}}, VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{Pointer: "/runtime_outputs/passed", Op: "equals", Value: true}}}},
			}
			provider := &checkpointActionProvider{workspace: session.Workspace, passed: passed}
			registry := NewProviderRegistry()
			registry.Register("structured-actions", provider)
			session.ProviderRegistry = registry
			if err := validateRuntimeWorkflowTeam(session, registry); err != nil {
				t.Fatal(err)
			}
			if err := session.SetCompatibilityWorkspaceScope(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			c.SetSessionData(NewSession())
			workflow := c.phaseWorkflow
			closeRun := c.beginExecutionRun()
			defer closeRun()
			c.SetRunInputAssignments([]RunInputAssignment{{Name: "scope", RawValue: []byte(`{"revision":"frozen"}`), Source: RunInputSourceCLI}})
			if err := c.resolveRunInputsForInvocation(t.Context(), "inspect frozen evidence"); err != nil {
				t.Fatal(err)
			}
			if err := workflow.Start(); err != nil {
				t.Fatal(err)
			}
			if output, err := c.ExecuteTasks(t.Context(), []TaskDef{{Agent: "preparer", ContractID: "produce", Goal: "prepare inputs"}}); err != nil || strings.Contains(output, "ERROR") {
				t.Fatalf("prepare: %s, %v", output, err)
			}
			output, err := c.ExecuteTasks(t.Context(), []TaskDef{{Agent: "verifier", ContractID: "inspect", Goal: "inspect accepted evidence"}})
			if passed && err != nil {
				t.Fatalf("dispatch: %s, %v", output, err)
			}
			items := c.taskTracker.TodoList().Items()
			if len(items) != 2 || provider.calls != 2 {
				t.Fatalf("items=%d provider calls=%d", len(items), provider.calls)
			}
			inspect := items[1]
			if passed {
				if inspect.Status != TaskDone || !isVerifySuccess(inspect.VerifyResult) || inspect.ExecutionReceipt == nil || inspect.ExecutionReceipt.RunInputSnapshotID != c.RunInputSnapshot().ID || workflow.State() != PhaseDone {
					t.Fatalf("inspection=%+v phase=%s", inspect, workflow.State())
				}
			} else if inspect.Status != TaskError || inspect.ExecutionReceipt != nil || err == nil || !strings.Contains(inspect.Detail, "structured action verification failed") {
				t.Fatalf("failed inspection=%+v output=%s", inspect, output)
			}
		})
	}
}

func TestVerifyActionRejectsMutationBeforeProviderExecution(t *testing.T) {
	for _, sideEffect := range []SideEffectClass{SideEffectWorkspaceWrite, SideEffectExternalWrite, ""} {
		t.Run(string(sideEffect), func(t *testing.T) {
			w := workflowAt(t, PhaseVerify)
			provider := &recordingActionProvider{result: ActionResult{}}
			w.registry = NewProviderRegistry()
			w.registry.Register("structured-actions", provider)
			if _, err := w.executeActionValueForTask(t.Context(), Action{Capability: "structured-actions", Type: "inspect"}, string(sideEffect)); err == nil || provider.executed != 0 {
				t.Fatalf("mutation dispatch err=%v calls=%d", err, provider.executed)
			}
		})
	}
}

func TestVerifyActionConfigurationRequiresExplicitNoSideEffects(t *testing.T) {
	for _, sideEffect := range []SideEffectClass{SideEffectNone, SideEffectWorkspaceWrite, SideEffectExternalWrite, ""} {
		t.Run(string(sideEffect), func(t *testing.T) {
			session := workflowTestSession(t)
			session.ContractTasks[3].SideEffect = sideEffect
			session.ContractTasks[3].Action = &Action{Capability: "structured-actions", Type: "inspect"}
			err := validateRuntimeWorkflowTeam(session, workflowTestRegistry())
			if (err == nil) != (sideEffect == SideEffectNone) {
				t.Fatalf("side effect=%q validation=%v", sideEffect, err)
			}
		})
	}
}
