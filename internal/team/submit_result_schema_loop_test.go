package team

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

type schemaPreflightStreamModel struct {
	rejectedResultStreamModel
}

func (*schemaPreflightStreamModel) Model() string { return "test" }

func TestSubmitResultSchemaPreflightUsesStableFailureIdentity(t *testing.T) {
	info := submitResultToolInfo(taskResultSubmissionContract{})
	baseline, ok := submitResultFailureFingerprint(submitResultToolName, "invalid submit_result arguments: unknown field")
	if !ok {
		t.Fatal("decoder rejection has no failure identity")
	}
	for _, path := range []string{"$.structured_payload.question", "$.structured_payload.findings[10].sources[0].title"} {
		message := buildToolSchemaValidationPrompt(submitResultToolName, &toolArgumentSchemaError{
			Path: path, Expected: "required property", Actual: "missing",
		}, info)
		fingerprint, ok := submitResultFailureFingerprint(submitResultToolName, message)
		if !ok || fingerprint != baseline {
			t.Fatalf("schema rejection did not share the decoder failure identity: %q", message)
		}
		if _, ok := submitResultFailureFingerprint("view", message); ok {
			t.Fatal("another tool counted as a submit_result rejection")
		}
		otherMessage := buildToolSchemaValidationPrompt("view", &toolArgumentSchemaError{Path: path}, info)
		if _, ok := submitResultFailureFingerprint(submitResultToolName, otherMessage); ok {
			t.Fatal("another tool's diagnostic counted as a submit_result rejection")
		}
	}
}

// Exercise the actual submit_result preflight and Fantasy's local-tool executor,
// which ignores OnToolResult errors. Vary the failing JSON path at every step;
// exact-input loop detection cannot make this regression pass accidentally.
func TestSubmitResultSchemaPreflightStopsLoopWithoutWorkerReplay(t *testing.T) {
	for _, name := range []string{"result_only_repair", "valid_correction", "repair_exhausted"} {
		t.Run(name, func(t *testing.T) {
			c := newBudgetCoordinator(t)
			c.session.Workspace = t.TempDir()
			c.executionRunID = "run-schema-preflight-loop"
			c.session.Agents = map[string]*agent.AgentDef{
				"reviewer": {Name: "reviewer", Role: "worker", MaxRetries: 2, SideEffect: string(SideEffectExternalWrite), Generation: agent.GenerationParams{Model: "test"}},
			}
			compiled, ref := compiledReviewContract(t, true)
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "submit a review", ResultContract: &ref}})[0]
			const valid = `{"verdict":"approve","max_tokens":7,"findings":[{"summary":"ok"}]}`
			invalid := []string{
				`{"max_tokens":7}`,
				`{"verdict":"approve","max_tokens":"seven"}`,
				`{"verdict":"approve","findings":[{"summary":3}]}`,
			}
			model := &schemaPreflightStreamModel{rejectedResultStreamModel: rejectedResultStreamModel{
				toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls,
				input: func(attempt int) string {
					payload := invalid[(attempt-1)%len(invalid)]
					if name == "valid_correction" && attempt == maxRepeatedSubmitResultFailures {
						payload = valid
					}
					return fmt.Sprintf(`{"status":"success","summary":"attempt %d","structured_payload":%s}`, attempt, payload)
				},
			}}
			workerTools := c.gatePolicyTools([]fantasy.AgentTool{&submitResultTool{coordinator: c, todoID: item.ID}})
			c.workerAgentOverride = fantasy.NewAgent(model, fantasy.WithTools(workerTools...), fantasy.WithMaxRetries(0), fantasy.WithStopConditions(fantasy.StepCountIs(maxRepeatedSubmitResultFailures+1)))
			repairCalls := 0
			var prompts []string
			c.repairAgentOverride = &scriptedRepairAgent{
				calls: &repairCalls, prompts: &prompts,
				steps: func(int) []fantasy.StepResult {
					if name == "repair_exhausted" {
						return invalidSchemaRepairSteps()
					}
					return nil
				},
				onCall: func(int) {
					if name == "repair_exhausted" {
						return
					}
					payload, err := validateStructuredResultPayload(compiled, ref, []byte(valid))
					if err != nil {
						t.Fatal(err)
					}
					c.storeSubmittedTaskResult(item.ID, &TaskResult{
						TaskID: item.ID, Agent: "reviewer", Status: TaskResultStatusSuccess,
						Summary: "repaired review", Source: "submitted", StructuredPayload: payload,
					})
				},
			}
			_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), TaskDef{
				Agent: "reviewer", Goal: "submit a review", Recovery: RecoveryManual,
				SideEffect: SideEffectExternalWrite, ResultContract: &ref, Execution: ExecutionContract{RequiresResult: true},
			}, item.ID)
			if name == "repair_exhausted" {
				if err == nil {
					t.Fatal("exhausted repair unexpectedly completed the task")
				}
			} else if err != nil {
				t.Fatalf("executeTask: %v", err)
			}
			wantRepairs := 1
			if name == "valid_correction" {
				wantRepairs = 0
			} else if name == "repair_exhausted" {
				wantRepairs = 2
			}
			if model.calls != maxRepeatedSubmitResultFailures || repairCalls != wantRepairs {
				t.Fatalf("provider/repair calls = %d/%d, want %d/%d", model.calls, repairCalls, maxRepeatedSubmitResultFailures, wantRepairs)
			}
			got := c.todoItemByID(item.ID)
			if got.ExecutionReceipt == nil || got.ExecutionReceipt.StepBudget.Used != maxRepeatedSubmitResultFailures {
				t.Fatalf("completion/receipt lost original execution: %#v", got)
			}
			if name == "repair_exhausted" {
				if got.Status != TaskBlocked || got.FailureEvent == nil || got.FailureEvent.FailureClass != FailureProtocol || got.FailureEvent.RetryDisposition != ReconcileOnly {
					t.Fatalf("exhausted repair lost its protocol/reconciliation boundary: %#v", got)
				}
			} else if got.Status != TaskDone {
				t.Fatalf("accepted result did not complete the task: %s", got.Status)
			}
			if wantRepairs > 0 {
				provenance := got.ExecutionReceipt.RepairProvenance
				if provenance == nil || provenance.Success != (name != "repair_exhausted") || provenance.RepairAttempts != wantRepairs {
					t.Fatalf("schema-only repair lost its provenance: %#v", provenance)
				}
				if len(prompts) != wantRepairs || !strings.Contains(prompts[0], "$.structured_payload.findings[0].summary") {
					t.Fatalf("repair lost the actual preflight diagnostic: %q", prompts)
				}
			}
		})
	}
}
