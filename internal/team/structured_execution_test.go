package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

type structuredTestTool struct {
	name string
	run  func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error)
}

func (t *structuredTestTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: t.name, Parameters: map[string]any{"type": "object"}}
}

func (t *structuredTestTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}

func (t *structuredTestTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *structuredTestTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return t.run(ctx, call)
}

func TestExecutionContractStructuredStepsParseAndValidate(t *testing.T) {
	const contractJSON = `{
  "agent":"worker",
  "goal":"produce, validate, and execute an artifact",
  "execution":{
    "kind":"process",
    "steps":[
      {"id":"produce","tool":"writer","effect":"produce","input":{"format":"text"},"outputs":[{"name":"draft","kind":"artifact"}]},
      {"id":"validate","tool":"validator","effect":"validate","depends_on":["produce"],"consumes":["draft"],"on_failure":"repairable","max_repairs":1},
      {"id":"mutate","tool":"executor","effect":"mutate","depends_on":["validate"],"consumes":["draft"]},
      {"id":"verify","tool":"verifier","effect":"verify","depends_on":["mutate"]}
    ]
  }
}`
	var task TaskDef
	if err := json.Unmarshal([]byte(contractJSON), &task); err != nil {
		t.Fatalf("unmarshal structured execution contract: %v", err)
	}
	if len(task.Execution.Steps) != 4 || task.Execution.Steps[0].Input["format"] != "text" {
		t.Fatalf("structured steps parsed incorrectly: %#v", task.Execution.Steps)
	}
	if err := ValidateExecutionContract(task); err != nil {
		t.Fatalf("valid structured execution contract rejected: %v", err)
	}
}

func TestRunStructuredExecutionResolvesTypedUpstreamReferences(t *testing.T) {
	contract := ExecutionContract{Steps: []ExecutionStep{
		{
			ID: "discover", Tool: "probe", Effect: ExecutionEffectRead,
			Outputs: []ExecutionStepOutput{
				{Name: "role_index", Kind: ExecutionOutputFact, Schema: "integer"},
				{Name: "discovery_receipt", Kind: ExecutionOutputReceipt},
			},
		},
		{
			ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, DependsOn: []string{"discover"},
			References: []ExecutionStepReference{
				{Target: "index", StepID: "discover", Output: "role_index", Kind: ExecutionOutputFact, Schema: "integer"},
				{Target: "evidence", StepID: "discover", Output: "discovery_receipt", Kind: ExecutionOutputReceipt},
			},
			Outputs: []ExecutionStepOutput{{Name: "draft", Kind: ExecutionOutputArtifact}},
		},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"draft"}},
	}}
	runner := StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		switch request.Step.ID {
		case "discover":
			return ExecutionStepResult{Stdout: "index=0", Facts: map[string]any{"role_index": 0}}, nil
		case "produce":
			if request.ResolvedInput["index"] != 0 {
				return ExecutionStepResult{}, errors.New("typed fact reference was not resolved")
			}
			receipt, ok := request.ResolvedInput["evidence"].(ExecutionStepReceipt)
			if !ok || receipt.StepID != "discover" || receipt.ExitCode != 0 {
				return ExecutionStepResult{}, errors.New("successful receipt reference was not resolved")
			}
			return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": {ID: "draft", SHA256: "digest"}}}, nil
		case "validate":
			return ExecutionStepResult{}, nil
		default:
			return ExecutionStepResult{}, errors.New("unexpected step")
		}
	})
	result, err := RunStructuredExecution(context.Background(), StructuredExecutionRequest{TaskID: "typed-refs", Attempt: 1, Contract: contract}, runner)
	if err != nil {
		t.Fatalf("RunStructuredExecution() error = %v", err)
	}
	if result.Facts["role_index"].SHA256 == "" || len(result.Receipts[0].ProducedFacts) != 1 {
		t.Fatalf("fact evidence was not preserved: result=%#v receipt=%#v", result.Facts, result.Receipts[0])
	}
	if result.Receipts[0].StdoutRef.SHA256 == "" || result.Receipts[0].StdoutRef.Bytes != int64(len("index=0")) {
		t.Fatalf("stdout artifact reference missing: %#v", result.Receipts[0].StdoutRef)
	}
}

func TestRunStructuredExecutionInjectsOpaqueArtifactRefAndReceiptsProvenance(t *testing.T) {
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "transcript", Kind: ExecutionOutputArtifact}}},
		{ID: "audit", Tool: "view", Effect: ExecutionEffectRead, DependsOn: []string{"produce"}, References: []ExecutionStepReference{{Target: "artifact_ref", StepID: "produce", Output: "transcript", Kind: ExecutionOutputArtifact}}},
	}}
	const artifactID = "sha256-exact-runtime-reference"
	const digest = "exact-digest"
	runner := StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		switch request.Step.ID {
		case "produce":
			return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"transcript": {ID: artifactID, SHA256: digest}}}, nil
		case "audit":
			if got, ok := request.ResolvedInput["artifact_ref"].(string); !ok || got != artifactID {
				return ExecutionStepResult{}, errors.New("artifact reference was flattened to a path or changed")
			}
			return ExecutionStepResult{}, nil
		default:
			return ExecutionStepResult{}, errors.New("unexpected step")
		}
	})
	result, err := RunStructuredExecution(context.Background(), StructuredExecutionRequest{TaskID: "opaque-artifact", Attempt: 1, Contract: contract}, runner)
	if err != nil {
		t.Fatalf("RunStructuredExecution() error = %v", err)
	}
	refs := result.Receipts[1].ResolvedRefs
	if len(refs) != 1 || refs[0].RefID != artifactID || refs[0].SHA256 != digest || refs[0].ProducerStep != "produce" || refs[0].Target != "artifact_ref" {
		t.Fatalf("resolved reference provenance = %#v", refs)
	}
}

func TestValidateExecutionContractStructuredStepsRejectUnsafeShapes(t *testing.T) {
	validSteps := []ExecutionStep{
		{ID: "produce", Tool: "writer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "draft"}}},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"draft"}, OnFailure: StepFailureRepairable, MaxRepairs: 1},
		{ID: "mutate", Tool: "executor", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"draft"}},
	}
	cases := []struct {
		name string
		edit func([]ExecutionStep) []ExecutionStep
		want string
	}{
		{
			name: "repairable validator needs a bound",
			edit: func(steps []ExecutionStep) []ExecutionStep {
				steps[1].MaxRepairs = 0
				return steps
			},
			want: "max_repairs",
		},
		{
			name: "mutation requires validated artifact",
			edit: func(steps []ExecutionStep) []ExecutionStep {
				steps[2].DependsOn = []string{"produce"}
				return steps
			},
			want: "successful validation",
		},
		{
			name: "producer output cannot be duplicated",
			edit: func(steps []ExecutionStep) []ExecutionStep {
				steps = append(steps, ExecutionStep{ID: "other", Tool: "writer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "draft"}}})
				return steps
			},
			want: "already produced",
		},
		{
			name: "reference schema must match upstream output",
			edit: func(steps []ExecutionStep) []ExecutionStep {
				steps[0].Outputs = append(steps[0].Outputs, ExecutionStepOutput{Name: "fact", Kind: ExecutionOutputFact, Schema: "string"})
				steps[2].References = []ExecutionStepReference{{Target: "fact", StepID: "produce", Output: "fact", Kind: ExecutionOutputFact, Schema: "integer"}}
				return steps
			},
			want: "does not match output schema",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			steps := append([]ExecutionStep(nil), validSteps...)
			steps = tc.edit(steps)
			err := ValidateExecutionContract(TaskDef{Agent: "worker", Goal: "unsafe", Execution: ExecutionContract{Steps: steps}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateExecutionContract() error = %v, want %q", err, tc.want)
			}
		})
	}

	mixed := TaskDef{Agent: "worker", Goal: "mixed", Execution: ExecutionContract{
		Steps:          validSteps,
		RequiresResult: true,
		ToolSequence:   []string{"bash", "submit_result"},
	}}
	if err := ValidateExecutionContract(mixed); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("mixed structured and legacy contract error = %v", err)
	}
}

func TestRunStructuredExecutionRepairsValidatorBeforeFreezeAndMutation(t *testing.T) {
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "draft"}}},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"draft"}, OnFailure: StepFailureRepairable, MaxRepairs: 1},
		{ID: "mutate", Tool: "mutator", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"draft"}},
		{ID: "verify", Tool: "verifier", Effect: ExecutionEffectVerify, DependsOn: []string{"mutate"}},
	}}
	registry := NewExecutionStepReceiptRegistry()
	var calls []string
	runner := StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		calls = append(calls, request.Step.ID+":"+string(rune('0'+request.RepairAttempt)))
		switch request.Step.ID {
		case "produce":
			digest := "bad-digest"
			if request.RepairAttempt == 1 {
				digest = "good-digest"
			}
			return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": {ID: "draft-" + digest, SHA256: digest}}}, nil
		case "validate":
			if request.Artifacts["draft"].SHA256 != "good-digest" {
				return ExecutionStepResult{ExitCode: 1, Stderr: "draft rejected"}, nil
			}
			return ExecutionStepResult{}, nil
		case "mutate":
			if request.Frozen["draft"].SHA256 != "good-digest" {
				return ExecutionStepResult{}, errors.New("mutation was not passed the frozen digest")
			}
			return ExecutionStepResult{}, nil
		case "verify":
			return ExecutionStepResult{}, nil
		default:
			return ExecutionStepResult{}, errors.New("unexpected step")
		}
	})

	result, err := RunStructuredExecution(context.Background(), StructuredExecutionRequest{
		TaskID: "task-1", Attempt: 1, Contract: contract, Registry: registry,
	}, runner)
	if err != nil {
		t.Fatalf("RunStructuredExecution() error = %v", err)
	}
	if result.State != StructuredExecutionVerified {
		t.Fatalf("final state = %q, want %q", result.State, StructuredExecutionVerified)
	}
	wantStates := []StructuredExecutionState{
		StructuredExecutionDraft,
		StructuredExecutionValidating,
		StructuredExecutionRepairableFailed,
		StructuredExecutionValidating,
		StructuredExecutionValidated,
		StructuredExecutionFrozen,
		StructuredExecutionExecuting,
		StructuredExecutionVerified,
	}
	if !reflect.DeepEqual(result.StateHistory, wantStates) {
		t.Fatalf("state history = %#v, want %#v", result.StateHistory, wantStates)
	}
	if got, want := calls, []string{"produce:0", "validate:0", "produce:1", "validate:1", "mutate:0", "verify:0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("step calls = %#v, want %#v", got, want)
	}
	if result.FrozenArtifacts["draft"].SHA256 != "good-digest" {
		t.Fatalf("frozen draft = %#v, want repaired digest", result.FrozenArtifacts["draft"])
	}
	if len(result.Receipts) != 6 || result.Receipts[1].ValidatorVerdict != "fail" || result.Receipts[3].ValidatorVerdict != "pass" {
		t.Fatalf("validator receipts = %#v", result.Receipts)
	}
	claims := make([]string, 0, len(result.Receipts))
	for _, receipt := range result.Receipts {
		claims = append(claims, receipt.ID)
	}
	if err := registry.ValidateClaims("task-1", 1, claims); err != nil {
		t.Fatalf("valid receipt claims rejected: %v", err)
	}
	if err := registry.ValidateSuccessfulContract("task-1", 1, contract, claims); err != nil {
		t.Fatalf("complete successful receipt chain rejected: %v", err)
	}
	if err := registry.ValidateClaims("task-1", 1, append(claims, "receipt-not-run")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unexecuted receipt claim error = %v", err)
	}
}

func TestCoordinatorStructuredLifecycleAcceptsOnlyReceiptCompleteSuccess(t *testing.T) {
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "draft", Kind: ExecutionOutputArtifact}}},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"draft"}, OnFailure: StepFailureRepairable, MaxRepairs: 1},
		{ID: "mutate", Tool: "mutator", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"draft"}},
		{ID: "verify", Tool: "verifier", Effect: ExecutionEffectVerify, DependsOn: []string{"mutate"}},
	}}
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, executionRunID: "run-structured-lifecycle", taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "structured lifecycle", Execution: contract}})[0]
	runner := StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		switch request.Step.ID {
		case "produce":
			digest := "invalid"
			if request.RepairAttempt == 1 {
				digest = "valid"
			}
			ref, putErr := store.Put(context.Background(), PutArtifactRequest{
				Content: []byte(digest), Path: "draft-" + digest, Kind: string(ExecutionOutputArtifact),
				RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, Agent: item.Agent,
			})
			if putErr != nil {
				return ExecutionStepResult{}, putErr
			}
			return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": ref.ArtifactRef}}, nil
		case "validate":
			if request.Artifacts["draft"].Path != "draft-valid" {
				return ExecutionStepResult{ExitCode: 1, Stderr: "invalid draft"}, nil
			}
			return ExecutionStepResult{}, nil
		case "mutate", "verify":
			return ExecutionStepResult{}, nil
		default:
			return ExecutionStepResult{}, errors.New("unexpected step")
		}
	})
	result, err := c.RunStructuredTask(context.Background(), item.ID, 1, runner)
	if err != nil {
		t.Fatalf("RunStructuredTask() error = %v", err)
	}
	ids := make([]string, len(result.Receipts))
	for i, receipt := range result.Receipts {
		ids[i] = receipt.ID
	}
	payload, err := json.Marshal(map[string]any{"status": TaskResultStatusSuccess, "summary": "done", "receipt_ids": ids})
	if err != nil {
		t.Fatalf("marshal submit_result: %v", err)
	}
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{Name: "submit_result", Input: string(payload)})
	if err != nil || response.IsError {
		t.Fatalf("receipt-complete submit_result rejected: response=%#v err=%v", response, err)
	}
	stored := c.GetTaskResult(item.ID)
	if stored == nil || len(stored.ReceiptIDs) != len(result.Receipts) {
		t.Fatalf("stored result does not preserve complete receipts: %#v", stored)
	}
}

func TestExecuteTaskRoutesStructuredContractThroughCoordinatorRunner(t *testing.T) {
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "draft", Kind: ExecutionOutputArtifact}}},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"draft"}},
		{ID: "mutate", Tool: "mutator", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"draft"}},
		{ID: "verify", Tool: "verifier", Effect: ExecutionEffectVerify, DependsOn: []string{"mutate"}},
	}}
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "structured-dispatch"}},
		executionRunID: "run-structured-dispatch",
		taskTracker:    NewTaskTracker(),
		reportStatus:   func(StatusEvent) {},
		taskResults:    make(map[string]*TaskResult),
		taskAttempts:   make(map[string]int),
		stepReceipts:   NewExecutionStepReceiptRegistry(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "structured dispatch", Execution: contract}})[0]
	c.SetStructuredStepRunner(StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		if request.Step.Effect == ExecutionEffectProduce {
			ref, putErr := store.Put(context.Background(), PutArtifactRequest{
				Content: []byte("approved"), Path: "draft.txt", Kind: string(ExecutionOutputArtifact),
				RunID: c.executionRunID, TaskID: request.TaskID, Attempt: request.Attempt, Agent: item.Agent,
			})
			if putErr != nil {
				return ExecutionStepResult{}, putErr
			}
			return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": ref.ArtifactRef}}, nil
		}
		return ExecutionStepResult{}, nil
	}))
	output, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "structured dispatch", Execution: contract}, item.ID)
	if err != nil {
		t.Fatalf("executeTask() error = %v", err)
	}
	if !strings.Contains(output, "structured execution completed") || item.Status != TaskDone {
		t.Fatalf("structured dispatch output/status = %q/%s", output, item.Status)
	}
	if result := c.GetTaskResult(item.ID); result == nil || result.Source != "runtime" || len(result.ReceiptIDs) != 4 {
		t.Fatalf("runtime receipt-backed result = %#v", result)
	}
}

func TestCoordinatorDeclaredToolRunnerExecutesTypedInputAndRehashesArtifact(t *testing.T) {
	projectDir := t.TempDir()
	var calls []string
	writer := &artifactAwareStructuredTestTool{structuredTestTool: &structuredTestTool{name: "writer", run: func(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		calls = append(calls, call.Name)
		var input struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(call.Input), &input); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		if err := os.WriteFile(filepath.Join(projectDir, input.Path), []byte(input.Content), 0o600); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		return fantasy.NewTextResponse("written"), nil
	}}}
	plainTool := func(name string) fantasy.AgentTool {
		return &artifactAwareStructuredTestTool{structuredTestTool: &structuredTestTool{name: name, run: func(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			calls = append(calls, call.Name)
			return fantasy.NewTextResponse("ok"), nil
		}}}
	}
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: "writer", Input: map[string]any{"path": "draft.txt", "content": "valid"}, Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "draft", Kind: ExecutionOutputArtifact, Path: "draft.txt"}}},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"draft"}},
		{ID: "mutate", Tool: "mutator", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"draft"}},
		{ID: "verify", Tool: "verifier", Effect: ExecutionEffectVerify, DependsOn: []string{"mutate"}},
	}}
	agentDef := &agent.AgentDef{Name: "worker", Role: "worker", Tools: "writer,validator,mutator,verifier"}
	c := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "declared-runner"}, Agents: map[string]*agent.AgentDef{"worker": agentDef}},
		projectDir:   projectDir,
		coreTools:    []fantasy.AgentTool{writer, plainTool("validator"), plainTool("mutator"), plainTool("verifier")},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "declared runner", Execution: contract}})[0]
	result, err := c.RunStructuredTask(context.Background(), item.ID, 1, &coordinatorDeclaredToolRunner{c: c})
	if err != nil {
		t.Fatalf("RunStructuredTask() error = %v", err)
	}
	if result.State != StructuredExecutionVerified || !reflect.DeepEqual(calls, []string{"writer", "validator", "mutator", "verifier"}) {
		t.Fatalf("declared runner state/calls = %s/%#v", result.State, calls)
	}
	if result.FrozenArtifacts["draft"].SHA256 == "" || result.Receipts[2].ConsumedDigests["draft"] != result.FrozenArtifacts["draft"].SHA256 {
		t.Fatalf("declared runner digest evidence = frozen %#v mutate %#v", result.FrozenArtifacts, result.Receipts[2])
	}
	ref, ok := result.Artifacts["draft"]
	if !ok || ref.RunID == "" || ref.TaskID != item.ID || ref.Attempt != 1 || ref.Agent != item.Agent || ref.Bytes != ref.ByteSize {
		t.Fatalf("declared publisher occurrence = %#v, want current run/task/attempt/agent and exact sizes", ref)
	}
}

func TestCoordinatorResolvesSuccessfulCrossTaskTypedOutput(t *testing.T) {
	upstreamContract := ExecutionContract{Steps: []ExecutionStep{{
		ID: "discover", Tool: "probe", Effect: ExecutionEffectRead,
		Outputs: []ExecutionStepOutput{{Name: "role_index", Kind: ExecutionOutputFact, Schema: "integer", Scope: "task"}},
	}}}
	c := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "cross-task-refs"}},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	upstream := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "discover-task", Agent: "worker", Desc: "discover", Execution: upstreamContract}})[0]
	var consumed any
	runner := StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		switch request.Step.ID {
		case "discover":
			return ExecutionStepResult{Facts: map[string]any{"role_index": 0}}, nil
		case "consume":
			consumed = request.ResolvedInput["index"]
			return ExecutionStepResult{}, nil
		default:
			return ExecutionStepResult{}, errors.New("unexpected step")
		}
	})
	c.SetStructuredStepRunner(runner)
	if _, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "discover", Execution: upstreamContract}, upstream.ID); err != nil {
		t.Fatalf("execute upstream structured task: %v", err)
	}
	downstreamContract := ExecutionContract{Steps: []ExecutionStep{{
		ID: "consume", Tool: "consumer", Effect: ExecutionEffectRead,
		References: []ExecutionStepReference{{Target: "index", TaskID: "discover-task", Output: "role_index", Kind: ExecutionOutputFact, Schema: "integer", Scope: "task"}},
	}}}
	downstream := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "consume", Execution: downstreamContract}})[0]
	downstream.DependsOn = []string{upstream.ID}
	if _, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "consume", Execution: downstreamContract}, downstream.ID); err != nil {
		t.Fatalf("execute downstream structured task: %v", err)
	}
	if consumed != 0 {
		t.Fatalf("cross-task typed fact = %#v, want 0", consumed)
	}

	badContract := downstreamContract
	badContract.Steps = append([]ExecutionStep(nil), downstreamContract.Steps...)
	badContract.Steps[0].References = append([]ExecutionStepReference(nil), downstreamContract.Steps[0].References...)
	badContract.Steps[0].References[0].Scope = "secret"
	bad := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "bad scope", Execution: badContract}})[0]
	bad.DependsOn = []string{upstream.ID}
	if _, err := c.RunStructuredTask(context.Background(), bad.ID, 1, runner); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("cross-task scope mismatch error = %v", err)
	}
}

func TestRunStructuredExecutionDoesNotMutateAfterRepairBudgetIsExhausted(t *testing.T) {
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "draft"}}},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"draft"}, OnFailure: StepFailureRepairable, MaxRepairs: 1},
		{ID: "mutate", Tool: "mutator", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"draft"}},
	}}
	var calls []string
	runner := StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		calls = append(calls, request.Step.ID)
		if request.Step.ID == "produce" {
			return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": {ID: "draft", SHA256: "still-invalid"}}}, nil
		}
		if request.Step.ID == "validate" {
			return ExecutionStepResult{ExitCode: 1, Stderr: "invalid"}, nil
		}
		return ExecutionStepResult{}, errors.New("mutation must not run")
	})

	result, err := RunStructuredExecution(context.Background(), StructuredExecutionRequest{TaskID: "task-2", Attempt: 1, Contract: contract}, runner)
	if err == nil {
		t.Fatal("RunStructuredExecution() error = nil, want exhausted validation failure")
	}
	var executionErr *StructuredExecutionError
	if !errors.As(err, &executionErr) || executionErr.StepID != "validate" || executionErr.Class != "validation" || executionErr.ExitCode != 1 {
		t.Fatalf("causal validation error = %#v, want validator receipt and exit code", err)
	}
	if result == nil || result.State != StructuredExecutionFailed {
		t.Fatalf("result = %#v, want failed structured execution", result)
	}
	if strings.Contains(strings.Join(calls, ","), "mutate") {
		t.Fatalf("mutation ran after failed validation: %#v", calls)
	}
}

func TestExecutionStepReceiptRegistryRejectsOtherAttempt(t *testing.T) {
	registry := NewExecutionStepReceiptRegistry()
	receipt := ExecutionStepReceipt{ID: "receipt-1", TaskID: "task", Attempt: 2, StepID: "validate", Tool: "validator", InputSHA256: "digest", ExitCode: 0}
	if err := registry.Record(receipt); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if err := registry.ValidateClaims("task", 1, []string{"receipt-1"}); err == nil || !strings.Contains(err.Error(), "attempt 2") {
		t.Fatalf("other-attempt claim error = %v", err)
	}
}

func TestFailureEventPreservesFirstCausalStepReceipt(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), projectDir: "/workspace"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "validate then report"}})[0]
	item.LastOperation = "submit_result"
	c.setCurrentTaskAttempt(item.ID, 1)
	registry := c.executionStepReceiptRegistry()
	validatorStarted := time.Now().Add(-time.Second)
	if err := registry.Record(ExecutionStepReceipt{
		ID: "validator-receipt", TaskID: item.ID, Attempt: 1, StepID: "validate", Tool: "validator",
		InputSHA256: "one", StartedAt: validatorStarted, ExitCode: 1, Stderr: "invalid artifact", ValidatorVerdict: "fail",
	}); err != nil {
		t.Fatalf("record validator receipt: %v", err)
	}
	if err := registry.Record(ExecutionStepReceipt{
		ID: "submit-receipt", TaskID: item.ID, Attempt: 1, StepID: "submit_result", Tool: "submit_result",
		InputSHA256: "two", StartedAt: time.Now(), ExitCode: 1, Stderr: "result rejected",
	}); err != nil {
		t.Fatalf("record submit receipt: %v", err)
	}

	event := c.failureEventForItem(item, FailureExecution, RetryNone, "task failed", FailureFingerprint{Digest: "fingerprint"}, item.ID)
	if event.Command != "validator" || event.ExitCode == nil || *event.ExitCode != 1 {
		t.Fatalf("failure event command/exit = %#v, want causal validator receipt", event)
	}
	if event.FailedStepID != "validate" || event.ReceiptID != "validator-receipt" || event.FailureType != "validation" || event.Phase != "validation" {
		t.Fatalf("failure event causal fields = %#v", event)
	}
	if strings.Contains(RenderFailureText(event), "command: submit_result") {
		t.Fatalf("failure rendering was overwritten by submit_result: %s", RenderFailureText(event))
	}
}

func TestReceiptRegistryIgnoresRepairedValidatorWhenFindingCausalFailure(t *testing.T) {
	registry := NewExecutionStepReceiptRegistry()
	started := time.Now().UTC()
	for _, receipt := range []ExecutionStepReceipt{
		{ID: "validator-fail", TaskID: "task", Attempt: 1, StepID: "validate", Tool: "validator", StartedAt: started, ExitCode: 1, ValidatorVerdict: "fail", PolicyVerdict: "allowed"},
		{ID: "validator-pass", TaskID: "task", Attempt: 1, StepID: "validate", Tool: "validator", StartedAt: started.Add(time.Millisecond), ExitCode: 0, ValidatorVerdict: "pass", PolicyVerdict: "allowed"},
		{ID: "mutation-fail", TaskID: "task", Attempt: 1, StepID: "mutate", Tool: "mutator", StartedAt: started.Add(2 * time.Millisecond), ExitCode: 9, PolicyVerdict: "allowed"},
	} {
		if err := registry.Record(receipt); err != nil {
			t.Fatalf("Record(%s): %v", receipt.ID, err)
		}
	}
	got, ok := registry.FirstFailure("task", 1)
	if !ok || got.ID != "mutation-fail" {
		t.Fatalf("FirstFailure() = %#v/%v, want unresolved mutation failure", got, ok)
	}
}

func TestRunStructuredExecutionDoesNotRepairOrReplayAfterMutationStarts(t *testing.T) {
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "draft"}}},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"draft"}, OnFailure: StepFailureRepairable, MaxRepairs: 2},
		{ID: "mutate", Tool: "mutator", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"draft"}},
		{ID: "verify", Tool: "verifier", Effect: ExecutionEffectVerify, DependsOn: []string{"mutate"}},
	}}
	counts := make(map[string]int)
	runner := StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		counts[request.Step.ID]++
		switch request.Step.Effect {
		case ExecutionEffectProduce:
			return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": {ID: "frozen", SHA256: "frozen-digest"}}}, nil
		case ExecutionEffectValidate:
			return ExecutionStepResult{}, nil
		case ExecutionEffectMutate:
			return ExecutionStepResult{ExitCode: 9, Stderr: "mutation failed"}, nil
		default:
			return ExecutionStepResult{}, nil
		}
	})

	result, err := RunStructuredExecution(context.Background(), StructuredExecutionRequest{TaskID: "mutation-task", Attempt: 1, Contract: contract}, runner)
	var executionErr *StructuredExecutionError
	if !errors.As(err, &executionErr) || executionErr.StepID != "mutate" || executionErr.ExitCode != 9 {
		t.Fatalf("mutation error = %#v, want causal mutation receipt", err)
	}
	if result == nil || result.State != StructuredExecutionFailed {
		t.Fatalf("result = %#v, want failed state", result)
	}
	if counts["produce"] != 1 || counts["validate"] != 1 || counts["mutate"] != 1 || counts["verify"] != 0 {
		t.Fatalf("post-mutation execution was repaired or replayed: %#v", counts)
	}
}

type changingArtifactRunner struct {
	digest        string
	mutationCalls int
}

func (r *changingArtifactRunner) RunStructuredStep(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
	switch request.Step.Effect {
	case ExecutionEffectProduce:
		r.digest = "approved"
		return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"draft": {ID: "draft", Path: "draft", SHA256: r.digest}}}, nil
	case ExecutionEffectValidate:
		// Simulate an out-of-band change after validation but before mutation.
		r.digest = "changed-after-validation"
		return ExecutionStepResult{}, nil
	case ExecutionEffectMutate:
		r.mutationCalls++
		return ExecutionStepResult{}, nil
	default:
		return ExecutionStepResult{}, nil
	}
}

func (r *changingArtifactRunner) InspectStructuredArtifact(_ context.Context, artifact ArtifactRef) (ArtifactRef, error) {
	artifact.SHA256 = r.digest
	return artifact, nil
}

func TestRunStructuredExecutionRejectsArtifactChangedAfterValidation(t *testing.T) {
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: "producer", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "draft"}}},
		{ID: "validate", Tool: "validator", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"draft"}},
		{ID: "mutate", Tool: "mutator", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"draft"}},
	}}
	runner := &changingArtifactRunner{}
	result, err := RunStructuredExecution(context.Background(), StructuredExecutionRequest{TaskID: "toctou", Attempt: 1, Contract: contract}, runner)
	if err == nil || !strings.Contains(err.Error(), "digest changed after validation") {
		t.Fatalf("RunStructuredExecution() error = %v, want changed digest rejection", err)
	}
	if result == nil || result.State != StructuredExecutionFailed || runner.mutationCalls != 0 {
		t.Fatalf("TOCTOU result/state mutation calls = %#v/%d", result, runner.mutationCalls)
	}
	var executionErr *StructuredExecutionError
	if !errors.As(err, &executionErr) || executionErr.Class != "policy" || executionErr.ReceiptID == "" || len(result.Receipts) != 3 || result.Receipts[2].PolicyVerdict != "denied" {
		t.Fatalf("TOCTOU causal policy receipt = err %#v receipts %#v", err, result.Receipts)
	}
}

func TestRunStructuredExecutionIsProviderNameNeutral(t *testing.T) {
	providerPrefix := "fake-provider-7f3a9"
	contract := ExecutionContract{Steps: []ExecutionStep{
		{ID: "produce", Tool: providerPrefix + "-produce", Effect: ExecutionEffectProduce, Outputs: []ExecutionStepOutput{{Name: "artifact"}}},
		{ID: "validate", Tool: providerPrefix + "-validate", Effect: ExecutionEffectValidate, DependsOn: []string{"produce"}, Consumes: []string{"artifact"}},
		{ID: "mutate", Tool: providerPrefix + "-mutate", Effect: ExecutionEffectMutate, DependsOn: []string{"validate"}, Consumes: []string{"artifact"}},
		{ID: "verify", Tool: providerPrefix + "-verify", Effect: ExecutionEffectVerify, DependsOn: []string{"mutate"}},
	}}
	runner := StructuredStepRunnerFunc(func(_ context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
		if !strings.HasPrefix(request.Step.Tool, providerPrefix) {
			return ExecutionStepResult{}, errors.New("runtime changed the provider tool name")
		}
		if request.Step.Effect == ExecutionEffectProduce {
			return ExecutionStepResult{Artifacts: map[string]ArtifactRef{"artifact": {ID: "artifact", SHA256: "digest"}}}, nil
		}
		return ExecutionStepResult{}, nil
	})

	result, err := RunStructuredExecution(context.Background(), StructuredExecutionRequest{TaskID: "neutral-task", Attempt: 1, Contract: contract}, runner)
	if err != nil || result.State != StructuredExecutionVerified {
		t.Fatalf("provider-neutral execution failed: result=%#v err=%v", result, err)
	}
	for _, receipt := range result.Receipts {
		if !strings.HasPrefix(receipt.Tool, providerPrefix) {
			t.Fatalf("provider tool name was interpreted or rewritten: %#v", receipt)
		}
	}
}
