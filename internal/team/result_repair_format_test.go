package team

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestResultOnlyRepairRequiresProviderToolCall(t *testing.T) {
	for _, repair := range []bool{false, true} {
		name := "normal_worker"
		if repair {
			name = "result_only_repair"
		}
		t.Run(name, func(t *testing.T) {
			c := newBudgetCoordinator(t)
			model := &rejectedResultStreamModel{
				toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls,
				input: func(int) string {
					return `{"status":"success","summary":"existing evidence","structured_payload":{}}`
				},
			}
			toolCalls := 0
			tool := fantasy.NewAgentTool(submitResultToolName, "Report existing evidence", func(context.Context, resultStreamSubmission, fantasy.ToolCall) (fantasy.ToolResponse, error) {
				toolCalls++
				return fantasy.NewTextResponse("accepted"), nil
			})
			ag := fantasy.NewAgent(model, fantasy.WithTools(tool))
			ctx := context.WithValue(withTestAuxiliaryInvocationContext(t.Context()), protocolRepairExecutionKey{}, repair)
			if _, _, err := c.runAgentWithStatusAndHistory(ctx, ag, "worker", "report", nil, &taskTiming{}, fantasy.StepCountIs(1)); err != nil {
				t.Fatal(err)
			}
			want := fantasy.ToolChoiceAuto
			if repair {
				want = fantasy.ToolChoiceRequired
			}
			if model.calls != 1 || toolCalls != 1 || len(model.choices) != 1 || model.choices[0] != want {
				t.Fatalf("provider/tool/choices = %d/%d/%v, want 1/1/[%s]", model.calls, toolCalls, model.choices, want)
			}
		})
	}
}

func TestResultRepairUsesBoundSubmissionFormat(t *testing.T) {
	for _, path := range []string{"missing_result", "schema_retry", "step_budget", "resume", "legacy"} {
		t.Run(path, func(t *testing.T) {
			c, item := newEvidenceRepairCoordinator(t, "repair-format-"+path)
			compiled, ref := compiledReviewContract(t, true)
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			task := TaskDef{
				Agent: "reviewer", Goal: "review unit-0013", SideEffect: SideEffectNone,
				Execution: ExecutionContract{RequiresResult: true},
			}
			if path != "legacy" {
				task.ResultContract = &ref
				c.session.AgentResultContracts = map[string]ResultContractRef{"reviewer": ref}
			}
			c.taskTracker = NewTaskTracker()
			item = c.taskTracker.TodoList().AddBatch([]TodoSpec{{
				Agent: task.Agent, Desc: task.Goal, Goal: task.Goal,
				Execution: task.Execution, SideEffect: task.SideEffect, ResultContract: task.ResultContract,
			}})[0]
			workerCalls, repairCalls := 0, 0
			var prompts []string
			c.workerAgentOverride = &rejectedOnceWorkerAgent{calls: &workerCalls}
			if path == "step_budget" {
				c.session.Agents["reviewer"].MaxSteps = 1
				c.workerAgentOverride = &exhaustedWorkerAgent{calls: &workerCalls}
			}
			c.repairAgentOverride = &scriptedRepairAgent{
				calls: &repairCalls, prompts: &prompts,
				steps: func(call int) []fantasy.StepResult {
					if path == "schema_retry" && call == 1 {
						return invalidSchemaRepairSteps()
					}
					return nil
				},
				onCall: func(call int) {
					if path == "schema_retry" && call == 1 {
						return
					}
					result := &TaskResult{
						TaskID: item.ID, Agent: "reviewer", Status: TaskResultStatusSuccess,
						Summary: "Review completed from existing evidence", Source: "submitted",
					}
					if path != "legacy" {
						var err error
						result.StructuredPayload, err = validateStructuredResultPayload(compiled, ref,
							[]byte(`{"verdict":"approve","max_tokens":7,"findings":[{"summary":"No issues"}]}`))
						if err != nil {
							t.Fatal(err)
						}
					}
					c.storeSubmittedTaskResult(item.ID, result)
				},
			}
			ctx := withTestProtocolRepairInvocationContext(t.Context())
			if path == "resume" {
				// Restore a durable protocol checkpoint, with no worker replay.
				if err := c.CommitTaskTransition(ctx, item.ID, TaskPending, TaskProtocolIncomplete,
					"missing result", "Review found no issues in unit-0013", nil); err != nil {
					t.Fatal(err)
				}
				if _, err := c.ResumeInterruptedTasks(ctx); err != nil {
					t.Fatal(err)
				}
			} else if _, err := c.executeTask(ctx, task, item.ID); err != nil {
				t.Fatal(err)
			}
			wantWorkerCalls, wantRepairCalls := 1, 1
			if path == "resume" {
				wantWorkerCalls = 0
			}
			if path == "schema_retry" {
				wantRepairCalls = 2
			}
			if workerCalls != wantWorkerCalls || repairCalls != wantRepairCalls || len(prompts) != wantRepairCalls {
				t.Fatalf("worker/repair/prompts = %d/%d/%d, want %d/%d/%d", workerCalls, repairCalls, len(prompts), wantWorkerCalls, wantRepairCalls, wantRepairCalls)
			}
			for _, prompt := range prompts {
				if path == "legacy" {
					if !strings.Contains(prompt, "report in outer `details`") {
						t.Fatalf("legacy deliverable instructions missing: %s", prompt)
					}
					continue
				}
				for _, want := range []string{"schema-defined deliverable in `structured_payload`", "every nested object and array item", "schema-valid `structured_payload` is required"} {
					if !strings.Contains(prompt, want) {
						t.Fatalf("%s prompt lacks %q: %s", path, want, prompt)
					}
				}
				for _, conflict := range []string{"report body in `details`", "deliverable in `details`", "For `open_questions`, use strings or objects"} {
					if strings.Contains(prompt, conflict) {
						t.Fatalf("%s prompt contains conflicting instruction %q", path, conflict)
					}
				}
			}
			got := c.todoItemByID(item.ID)
			if got.Status != TaskDone || got.ExecutionReceipt == nil || got.ExecutionReceipt.RepairProvenance == nil || !got.ExecutionReceipt.RepairProvenance.Success {
				t.Fatalf("repair did not complete with successful repair provenance: status=%s receipt=%#v", got.Status, got.ExecutionReceipt)
			}
			if path != "legacy" && (got.TypedResult == nil || got.TypedResult.StructuredPayload == nil || got.TypedResult.StructuredPayload.Contract.SchemaSHA256 != ref.SchemaSHA256) {
				t.Fatal("repair lost the bound structured result")
			}
		})
	}
}

func TestFinalizationFormatUsesDurableContract(t *testing.T) {
	c, _ := newEvidenceRepairCoordinator(t, "durable-result-format")
	_, ref := compiledReviewContract(t, false)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", ResultContract: &ref}})[0]
	// The loaded agent's current default must not replace the occurrence's
	// admitted optional contract with a different, required one.
	c.session.AgentResultContracts = map[string]ResultContractRef{"reviewer": {ID: "different", RequireStructured: true}}
	prompt := c.taskFinalizationBinding(item.ID)
	if !strings.Contains(prompt, "schema-defined deliverable in `structured_payload`") {
		t.Fatalf("durable contract ignored: %s", prompt)
	}
	if strings.Contains(prompt, "schema-valid `structured_payload` is required") {
		t.Fatalf("optional contract made mandatory: %s", prompt)
	}
}
