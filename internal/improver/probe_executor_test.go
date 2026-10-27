package improver

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/scripted"
	"github.com/kjelly/hufu/internal/team"
)

func testProbeExecutor(t *testing.T, edit func(*Prepared)) (*ProbeExecutor, []StageSpec, *ObjectStore) {
	t.Helper()
	prepared, err := Prepare("testdata/extraction/experiment.yaml", testBuild)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(&prepared)
	}
	root := t.TempDir()
	objects, err := OpenObjects(root, prepared.Config.ExperimentID, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Close() })
	settings := RuntimeSettings{ProviderURL: scripted.Endpoint}
	plan, bindings, err := FreezePlan(prepared, "parent-workspace", "parent-branch", objects, settings)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPrepared(objects, plan, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.Inputs.Inventory(), loaded.Inputs.Inventory()) {
		t.Fatal("frozen input closure changed")
	}
	children := filepath.Join(root, "children")
	if err := os.Mkdir(children, 0o700); err != nil {
		t.Fatal(err)
	}
	executor, err := NewProbeExecutor(loaded, plan, objects, children, settings)
	if err != nil {
		t.Fatal(err)
	}
	stages, _, err := plannedStages(plan)
	if err != nil {
		t.Fatal(err)
	}
	return executor, stages, objects
}

func probeTestRequest(executor *ProbeExecutor, stage StageSpec) StageRequest {
	return StageRequest{
		Lease: Lease{StageID: stage.ID, Generation: 1, Owner: "test-owner", ChildWorkspaceID: "child-" + stage.ID},
		Spec:  stage, Deadline: time.Now().UTC().Add(time.Minute), Limits: executor.plan.Limits,
	}
}

func TestProbeExecutorCanonicalPairAndReconcile(t *testing.T) {
	executor, stages, _ := testProbeExecutor(t, nil)
	for i, stage := range stages {
		request := probeTestRequest(executor, stage)
		bindings := 0
		result, err := executor.Execute(t.Context(), request, func(id paired.RuntimeIdentity) error {
			bindings++
			if id.InvocationID == "" || id.RunID == "" || id.BranchID == "" {
				t.Fatal("missing actual child identity")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := "passed"
		if i == 0 {
			want = "failed"
		}
		if result.Outcome != want || !result.UsageKnown || result.Tokens != 120 || !result.Stopped || bindings != 1 {
			t.Fatalf("stage %s: got %+v, want %s with 120 known tokens and one binding", stage.ID, result, want)
		}
		for range 2 {
			recovered, err := executor.Reconcile(t.Context(), request)
			if err != nil || recovered.State != "completed" || recovered.Settlement != result {
				t.Fatalf("reconcile changed settlement: %+v, %v", recovered, err)
			}
		}
		// An admitted child path is never reused for another provider attempt.
		if _, err := executor.Execute(t.Context(), request, func(paired.RuntimeIdentity) error {
			t.Fatal("duplicate dispatch reached binding")
			return nil
		}); err == nil {
			t.Fatal("duplicate execution accepted")
		}
		foreign := request
		foreign.Owner = "another-owner"
		if _, err := executor.ReadSettlement(t.Context(), foreign, result.Evidence); err == nil {
			t.Fatal("foreign lease accepted completed evidence")
		}
		if err := os.RemoveAll(filepath.Join(executor.children, request.ChildWorkspaceID)); err != nil {
			t.Fatal(err)
		}
		offline, err := executor.ReadSettlement(t.Context(), request, result.Evidence)
		if err != nil || offline != result {
			t.Fatalf("self-contained evidence failed: %+v %v", offline, err)
		}
	}
}

func TestProbeExecutorBindingFailureCannotCompleteOrRedispatch(t *testing.T) {
	executor, stages, _ := testProbeExecutor(t, nil)
	request := probeTestRequest(executor, stages[0])
	refusal := errors.New("parent commit rejected")
	if result, err := executor.Execute(t.Context(), request, func(paired.RuntimeIdentity) error { return refusal }); err == nil || result.Stopped {
		t.Fatalf("failed binding completed: %+v %v", result, err)
	}
	recovered, err := executor.Reconcile(t.Context(), request)
	if err == nil || recovered.State != "unknown" {
		t.Fatalf("absent drain marker inferred completion: %+v %v", recovered, err)
	}
}

func TestProbeExecutorAlreadyCancelledNeverCreatesChild(t *testing.T) {
	executor, stages, _ := testProbeExecutor(t, nil)
	request := probeTestRequest(executor, stages[0])
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := executor.Execute(ctx, request, func(paired.RuntimeIdentity) error {
		t.Fatal("cancelled stage reached binding")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
	if _, err := os.Stat(filepath.Join(executor.children, request.ChildWorkspaceID)); !os.IsNotExist(err) {
		t.Fatal("cancelled stage created child")
	}
}

func updateProbeProgram(t *testing.T, prepared *Prepared) {
	t.Helper()
	raw, err := json.Marshal(prepared.Program)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Inputs.files["fixture_program.json"] = raw
	if err := prepared.finishPreview(); err != nil {
		t.Fatal(err)
	}
}

func TestProbeExecutorExplicitZeroAndUnknownUsage(t *testing.T) {
	for _, known := range []bool{false, true} {
		name := "unknown"
		if known {
			name = "explicit_zero"
		}
		t.Run(name, func(t *testing.T) {
			executor, stages, _ := testProbeExecutor(t, func(prepared *Prepared) {
				for i := range prepared.Program.Scripts {
					for j := range prepared.Program.Scripts[i].Steps {
						var usage *scripted.Usage
						if known {
							usage = &scripted.Usage{}
						}
						prepared.Program.Scripts[i].Steps[j].Usage = usage
					}
				}
				updateProbeProgram(t, prepared)
			})
			result, err := executor.Execute(t.Context(), probeTestRequest(executor, stages[1]), func(paired.RuntimeIdentity) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			want := "unknown"
			if known {
				want = "passed"
			}
			if result.Outcome != want || result.UsageKnown != known || result.Tokens != 0 {
				t.Fatalf("usage presence changed: %+v", result)
			}
		})
	}
}

func TestProbeExecutorForbiddenToolCallsHaveNoEffect(t *testing.T) {
	for _, tool := range []string{"bash", "view", "agent", "memory_query", "mcp_exec"} {
		t.Run(tool, func(t *testing.T) {
			sentinel := filepath.Join(t.TempDir(), "must-not-exist")
			executor, stages, objects := testProbeExecutor(t, func(prepared *Prepared) {
				arguments, err := json.Marshal(map[string]string{"command": "touch " + sentinel, "file_path": sentinel})
				if err != nil {
					t.Fatal(err)
				}
				for i := range prepared.Program.Scripts {
					script := &prepared.Program.Scripts[i]
					script.Steps = append([]scripted.Step{{
						ToolCall: &scripted.ToolCall{Name: tool, Arguments: string(arguments)},
						Usage:    &scripted.Usage{InputTokens: 100, OutputTokens: 20},
					}}, script.Steps...)
				}
				updateProbeProgram(t, prepared)
			})
			request := probeTestRequest(executor, stages[1])
			result, err := executor.Execute(t.Context(), request, func(paired.RuntimeIdentity) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
				t.Fatal("forbidden tool caused a filesystem effect")
			}
			var stage StageEvidence
			if err := readObjectJSON(objects, result.Evidence, &stage); err != nil {
				t.Fatal(err)
			}
			observation, err := inspectChildEvidence(t.Context(), objects, stage.Child)
			if err != nil {
				t.Fatal(err)
			}
			for _, task := range observation.session.Tasks {
				for _, receipt := range task.ExecutionReceipts {
					if receipt.ToolSequence == nil {
						continue
					}
					// The canonical transcript remains available for a denied
					// attempt; none may be counted as a successful tool call.
					for _, invocation := range receipt.ToolInvocations {
						if invocation.LogicalTool == tool && invocation.Outcome == "success" {
							t.Fatal("forbidden tool was recorded as successful")
						}
					}
				}
			}
		})
	}
}

func TestProbeClassificationRequiresCanonicalVerification(t *testing.T) {
	executor, stages, objects := testProbeExecutor(t, nil)
	request := probeTestRequest(executor, stages[1])
	result, err := executor.Execute(t.Context(), request, func(paired.RuntimeIdentity) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var evidence StageEvidence
	if err := readObjectJSON(objects, result.Evidence, &evidence); err != nil {
		t.Fatal(err)
	}
	item, input, err := executor.checkRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*childObservation, *team.TodoItem)
		want string
	}{
		{"verified", func(*childObservation, *team.TodoItem) {}, "passed"},
		{"collected_verified", func(_ *childObservation, task *team.TodoItem) { task.TypedResult.Source = "collected" }, "passed"},
		{"missing_verifier", func(_ *childObservation, task *team.TodoItem) {
			task.VerifyResult = nil
		}, "unknown"},
		{"verifier_timeout", func(_ *childObservation, task *team.TodoItem) {
			task.VerifyResult.TimedOut = true
			task.VerifyResult.ExitCode = 1
			task.FailureEvent = &team.FailureEventPayload{FailureClass: team.FailureVerify}
		}, "unknown"},
		{"overturned_verifier", func(_ *childObservation, task *team.TodoItem) {
			task.VerifyResult.Overturned = true
		}, "unknown"},
		{"assertion_failure", func(_ *childObservation, task *team.TodoItem) {
			task.Status = team.TaskError
			task.VerifyResult.ExitCode = 1
			task.FailureEvent = &team.FailureEventPayload{FailureClass: team.FailureVerify}
		}, "failed"},
		{"schema_rejected", func(_ *childObservation, task *team.TodoItem) {
			task.Status = team.TaskError
			task.TypedResult = nil
			task.VerifyResult = nil
			task.ExecutionReceipt.ResultValidation = team.ResultValidationInvalid
			task.ExecutionReceipt.ResultValidationFailures = 1
		}, "failed"},
		{"schema_rejected_unknown_usage", func(observation *childObservation, task *team.TodoItem) {
			observation.usageKnown = false
			task.TypedResult = nil
			task.ExecutionReceipt.ResultValidation = team.ResultValidationInvalid
			task.ExecutionReceipt.ResultValidationFailures = 1
		}, "unknown"},
		{"provider_failure", func(observation *childObservation, _ *team.TodoItem) {
			observation.providerFailed = true
		}, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Re-import the immutable bytes for every case. The classifier must
			// not turn a partial or failed canonical observation into a pass.
			observation, err := inspectChildEvidence(t.Context(), objects, evidence.Child)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(&observation, observation.session.Tasks[0])
			got, err := classifyProbe(observation, executor.factory.config.Worker, input, item.Verify, evidence.Child.Overlay.ResultSchemaHash)
			if err != nil || got != tc.want {
				t.Fatalf("outcome = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
