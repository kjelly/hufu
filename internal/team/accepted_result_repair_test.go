package team

import (
	"context"
	"encoding/json/jsontext"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestContractRecoveryAcceptedRepairRequiresVerifiedSources(t *testing.T) {
	for _, scenario := range []string{"corrupt_source", "corrupt_accepted", "valid_result", "without_runtime_reads", "schema_requires_semantic_change"} {
		t.Run(scenario, func(t *testing.T) {
			c, source, id := contractCollectionFixture(t, researchCollectionResponse(t, 2), SideEffectNone, contractCollectionFixtureOptions{Accepted: true, ArtifactKeys: true, ValidAccepted: scenario == "valid_result"})
			item := todoItemByID(source.Session.Tasks, id)
			if scenario == "without_runtime_reads" || scenario == "schema_requires_semantic_change" {
				compiled := c.session.ResultContracts[item.ResultContract.ID]
				raw := strings.Replace(string(compiled.CanonicalSchema), `"runtime_owned_files_read":true`, `"runtime_owned_files_read":false`, 1)
				if scenario == "schema_requires_semantic_change" {
					raw = strings.Replace(string(compiled.CanonicalSchema), `"maxItems":12`, `"maxItems":1`, 1)
				}
				next, err := CompileResultContractDocument(item.ResultContract.ID, []byte(raw))
				if err != nil {
					t.Fatal(err)
				}
				c.session.ResultContracts[next.ID] = next
				ref := next.ref(true)
				c.session.ContractTasks[0].ResultContract = &ref
			} else if strings.HasPrefix(scenario, "corrupt_") {
				artifact := item.ExecutionReceipt.ArtifactScope.AuthorizedRefs[0].ID
				if scenario == "corrupt_accepted" {
					artifact = currentResultCollection(item).Accepted.ArtifactID
				}
				if err := os.WriteFile(filepath.Join(c.session.Workspace, logsDir, "artifacts", "data", artifact), []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.AssessContractRecovery(contractCollectionContext(t, id), source); err == nil {
				t.Fatal("unverified or unnecessary accepted repair admitted")
			}
		})
	}
}

func TestContractRecoveryAcceptedGrantWithInvocationContext(t *testing.T) {
	for _, scenario := range []string{"plain", "coordinator", "provider_bound"} {
		t.Run(scenario, func(t *testing.T) {
			c, source, id := contractCollectionFixture(t, researchCollectionResponse(t, 2), SideEffectNone, contractCollectionFixtureOptions{Accepted: true, ArtifactKeys: true, Dependents: true})
			ctx := contractCollectionContext(t, id)
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			branch, err := c.ApplyContractRecovery(ctx, source, "invocation-repair", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			child, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			c = reopenContractCollection(t, c, child)
			ctx = WithResultOnlyRepair(WithSessionResume(t.Context()), id)
			if scenario != "plain" {
				ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: c.executionRunID, AgentName: "coordinator", AgentRole: "coordinator", ModelExecutionID: "coordinator-call"})
			}
			if scenario == "provider_bound" {
				ctx = withTestProtocolRepairInvocationContext(ctx)
			}
			ctx = acceptedReferenceTestContext(t, c, ctx, id)
			if err := c.authorizeOperatorResultRepair(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Model-free native work/collection fixtures use the same durable transition,
// observation, receipt and admission boundaries as the selected source.
func acceptedRepairDependents(t *testing.T, c *Coordinator, source string, definition TaskDef, snapshot *RunInputSnapshot, artifact ArtifactRef) {
	t.Helper()
	parent := source
	for _, name := range []string{"inventory", "consumer", "unrelated"} {
		deps := []string{parent}
		if name == "unrelated" {
			deps = nil
		}
		spec := TodoSpec{Agent: "worker", Desc: name, Model: "ollama/test", ExecutionTarget: c.todoItemByID(source).ExecutionTarget, ContractID: definition.ID, ContractHash: c.todoItemByID(source).ContractHash, Phase: definition.Phase, Execution: definition.Execution, VerifySpec: definition.VerifySpec, ResultContract: definition.ResultContract, SideEffect: definition.SideEffect, Recovery: definition.Recovery, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, DependsOn: deps}
		ids := c.taskTracker.TodoList().ReserveIDs(1)
		projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.admitTaskOccurrence(t.Context(), projection, ids[0], 1); err != nil {
			t.Fatal(err)
		}
		items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
		if err != nil {
			t.Fatal(err)
		}
		item := items[0]
		completeAcceptedRepairDependent(t, c, item, definition, c.todoItemByID(source).ExecutionReceipt, artifact)
		parent = item.ID
	}
}

func completeAcceptedRepairDependent(t *testing.T, c *Coordinator, item *TodoItem, definition TaskDef, base *ExecutionReceipt, artifact ArtifactRef) {
	t.Helper()
	name := item.Desc
	if err := c.CommitTaskTransition(t.Context(), item.ID, TaskPending, TaskInProgress, "working", "", nil); err != nil {
		t.Fatal(err)
	}
	ctx := occurrenceTestContext(c, item.ID, 1)
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, base.RunID, 1, item.Agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	if err := transcript.RecordAssistantOutput(name); err != nil {
		t.Fatal(err)
	}
	sealed, err := sealVerbatimTaskTranscript(transcript)
	if err != nil {
		t.Fatal(err)
	}
	receipt := cloneExecutionReceipt(base)
	receipt.TaskID, receipt.ModelExecutionID, receipt.TranscriptRef = item.ID, "work-"+name, sealed.ID
	receipt.ResultCollection, receipt.RepairProvenance, receipt.SubmittedResult = nil, nil, nil
	receipt.ResultPayloadSHA256, receipt.ResultContractID, receipt.ResultContractSHA256, receipt.ResultValidation = "", "", "", ""
	ctx, response := observeContractCollectionKeys(t, c, item, ctx, &receipt, researchCollectionResponse(t, 2), artifact)
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, name)
	if err == nil {
		cp, err = c.reserveResultCollection(ctx, cp, "collector-"+name, []byte(`{"tools":[]}`))
	}
	if err == nil {
		cp, err = c.recordCollectionResponse(ctx, cp, []byte(response))
	}
	if err == nil {
		cp, err = c.prepareCollectionClaims(ctx, cp)
	}
	if err == nil {
		err = c.commitPreparedCollection(ctx, cp)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.finishProtocolRepair(ctx, c.todoItemByID(item.ID), definition, item.Agent, "ollama/test", c.GetTaskResult(item.ID), name); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptedResultRepairFreezesSemanticClaims(t *testing.T) {
	baseline := &TaskResult{Status: "success", Summary: "original", Details: "complete report", Confidence: 1, Evidence: []EvidenceRef{{Type: "task_transcript", TaskID: "original", RunID: "run", Value: "original-transcript", SystemHMAC: "previous-process"}}, Facts: map[string]any{"count": jsontext.Value(`9007199254740993`)}, Findings: []Finding{{Summary: "original issue", Severity: "error"}}, StructuredPayload: &ResultPayload{Value: jsontext.Value(`{"findings":[{"summary":"original issue","severity":"error","evidence":["bad","good"]}]}`)}}
	pointers := []string{"/findings/0/evidence/0"}
	want, err := acceptedResultSemanticHash(baseline, pointers)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"reference", "runtime_signature", "evidence_identity", "summary", "severity", "drop", "reorder", "good_reference", "outer_finding", "status", "details", "large_integer"} {
		t.Run(change, func(t *testing.T) {
			candidate := cloneTaskResult(baseline)
			body := string(candidate.StructuredPayload.Value)
			switch change {
			case "runtime_signature":
				candidate.Evidence[0].SystemHMAC = "current-process"
			case "evidence_identity":
				candidate.Evidence[0].Value = "unrelated-transcript"
			case "large_integer":
				candidate.Facts["count"] = jsontext.Value(`9007199254740992`)
			case "reference":
				body = strings.Replace(body, `"bad"`, `"correct"`, 1)
			case "summary":
				candidate.Summary = "changed"
			case "severity":
				body = strings.Replace(body, `"error"`, `"info"`, 1)
			case "drop":
				body = `{"findings":[]}`
			case "reorder":
				body = strings.Replace(body, `["bad","good"]`, `["good","bad"]`, 1)
			case "good_reference":
				body = strings.Replace(body, `"good"`, `"other"`, 1)
			case "outer_finding":
				candidate.Findings[0].Severity = "info"
			case "status":
				candidate.Status = "blocked"
			case "details":
				candidate.Details = "changed"
			}
			candidate.StructuredPayload.Value = []byte(body)
			got, err := acceptedResultSemanticHash(candidate, pointers)
			if (err == nil && got == want) != (change == "reference" || change == "runtime_signature") {
				t.Fatalf("unexpected immutable claims result: %v", err)
			}
		})
	}
}

func TestContractRecoveryAcceptedRepairRejectsChangedProof(t *testing.T) {
	c, source, id := contractCollectionFixture(t, researchCollectionResponse(t, 2), SideEffectNone, contractCollectionFixtureOptions{Accepted: true, ArtifactKeys: true})
	ctx := contractCollectionContext(t, id)
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	original := todoItemByID(source.Session.Tasks, id)
	for _, change := range []string{"result", "claims", "pointer", "schema", "missing_payload", "no_selection", "stale_plan"} {
		t.Run(change, func(t *testing.T) {
			if change == "no_selection" {
				// A plain preview cannot turn a done task into a result-only repair.
				plain, err := c.AssessContractRecovery(WithMetadataOnlyContractRecovery(t.Context()), source)
				if err == nil && plain.Repaired != 0 {
					t.Fatal("accepted result reopened without explicit selection")
				}
				return
			}
			if change == "stale_plan" {
				if _, err := c.ApplyContractRecovery(ctx, source, "must-not-publish", strings.Repeat("0", 64)); err == nil {
					t.Fatal("stale plan published")
				}
				return
			}
			patch := plan.Tasks[0]
			patch.Collection = new(*patch.Collection)
			patch.Collection.AcceptedResult = new(*patch.Collection.AcceptedResult)
			proof := patch.Collection.AcceptedResult
			candidate := cloneTodoItem(original)
			switch change {
			case "missing_payload":
				candidate.TypedResult.StructuredPayload = nil
			case "result":
				proof.ResultSHA256 = strings.Repeat("0", 64)
			case "claims":
				proof.SemanticSHA256 = strings.Repeat("0", 64)
			case "pointer":
				proof.Pointers = []string{"/summary"}
			case "schema":
				proof.Schema = []byte(`{"type":"object"}`)
			}
			if err := validateContractResultRepairSource(candidate, patch, plan); err == nil {
				t.Fatal("forged accepted repair proof authorized")
			}
		})
	}
}

func TestContractRecoveryRepairsAcceptedEvidenceWithoutWorkerReplay(t *testing.T) {
	for _, scenario := range []string{"correct", "exhausted", "prior_grant", "verbatim", "assumption_checks", "invalid_selection", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			runCtx, cancel := context.WithCancel(WithSessionResume(t.Context()))
			t.Cleanup(cancel)
			response := strings.Replace(researchCollectionResponse(t, 2), `"status":"success"`, `"facts":{"large":9007199254740993,"count":6,"paths":["one","two"]},"status":"success"`, 1)
			if scenario == "assumption_checks" {
				response = strings.Replace(response, `"status":"success"`, `"assumption_checks":[{"assumption_id":"source_revision","status":"supported","note":"verified in original work"}],"status":"success"`, 1)
			}
			c, source, id := contractCollectionFixture(t, response, SideEffectNone, contractCollectionFixtureOptions{ArtifactKeys: true, Accepted: true, Exhausted: scenario == "exhausted", LongOutput: scenario == "correct", Verbatim: scenario == "verbatim", PriorGrant: scenario == "prior_grant", Dependents: scenario == "correct"})
			original := cloneTodoItem(todoItemByID(source.Session.Tasks, id))
			old := currentResultCollection(original)
			reference := original.ExecutionReceipt.ArtifactScope.AuthorizedRefs[0]
			verifyHandoff := VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{
				Pointer: "/structured_payload/value/findings/0", Op: "equals", Value: reference.ID,
			}}}
			if scenario == "correct" {
				if result, err := executeTaskResultAssertVerification(c.projectDir, verifyHandoff, original.TypedResult); original.Status != TaskDone || err == nil || result.ExitCode == 0 {
					t.Fatal("legacy result must be accepted locally and rejected by the consumer")
				}
			}
			ctx := contractCollectionContext(t, id)
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			wantRetries := 0
			if scenario == "correct" {
				wantRetries = 2
			}
			if plan.Repaired != 1 || plan.Retried != wantRetries || plan.Tasks[0].Collection.AcceptedResult == nil || !reflect.DeepEqual(plan.Tasks[0].Collection.AcceptedResult.Pointers, []string{"/findings/0"}) {
				t.Fatalf("unexpected accepted repair plan: %+v", plan)
			}
			before := mustReadEvents(t, c.eventStore)
			branch, err := c.ApplyContractRecovery(ctx, source, "accepted-reference-repair", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			parent, err := ReadResumeSource(c.session.Workspace, "main", "run-test")
			if err != nil || compareSingleTaskProjection(parent.Session.Tasks[0], original) != nil {
				t.Fatalf("parent changed: %v", err)
			}
			events := mustReadEvents(t, c.eventStore)
			if !reflect.DeepEqual(before, events[:len(before)]) {
				t.Fatal("historical events changed")
			}
			child, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			cp := currentResultCollection(child.Session.Tasks[0])
			if scenario == "correct" {
				if child.Session.Tasks[1].Status != TaskPending || child.Session.Tasks[1].TypedResult != nil || child.Session.Tasks[2].Status != TaskPending || child.Session.Tasks[3].Status != TaskDone {
					t.Fatal("accepted repair failed downstream invalidation or lost unrelated work")
				}
				if plan.Retained != 1 || !reflect.DeepEqual(plan.ReopenedVerification, []string{"1", "2", "3"}) {
					t.Fatalf("verification not reopened: %+v", plan)
				}
			}
			if cp.Work != old.Work || cp.TurnsUsed != old.TurnsUsed || cp.TurnLimit != old.TurnLimit || cp.Response != nil || cp.Accepted != nil {
				t.Fatal("publication altered original work/budget or reused accepted claims")
			}
			restarted := reopenContractCollection(t, c, child)
			if scenario == "correct" {
				if len(original.Output) <= 2000 {
					t.Fatal("fixture does not cover long accepted output")
				}
				if err := restarted.emitTaskEventsFromCheckpoint(restarted.taskTracker.TodoList().Items()); err != nil {
					t.Fatal(err)
				}
				resumed, err := ReadResumeSource(restarted.session.Workspace, branch, "")
				if err != nil {
					t.Fatal(err)
				}
				restarted = reopenContractCollection(t, restarted, resumed)
				if restarted.sessionData.RecoveryRequired || restarted.todoItemByID(id).Output != original.Output {
					t.Fatal("restart before grant lost complete work or failed canonical parity")
				}
			}
			if scenario == "assumption_checks" {
				assertAcceptedRepairContextClaims(t, restarted, id, original.TypedResult)
			}
			if err := restarted.validateResumeSources(WithSessionResume(t.Context())); err != nil {
				t.Fatal(err)
			}
			selectionCtx := acceptedReferenceTestContext(t, restarted, t.Context(), id)
			a, err := restarted.AssessResultCollectionRecovery(selectionCtx, id)
			if err != nil || a.Disposition != RecoverySelectReferences {
				t.Fatalf("assessment=%+v error=%v", a, err)
			}
			missingContext := cloneRecoveryAssessment(a)
			for i, check := range missingContext.Checks {
				if check.Code == "claims_context" {
					missingContext.Checks = append(missingContext.Checks[:i], missingContext.Checks[i+1:]...)
					break
				}
			}
			missingContext.Fingerprint, _ = recoveryAssessmentFingerprint(missingContext)
			if validateRecoveryAssessment(missingContext) == nil {
				t.Fatal("recovery grant admitted without context proof")
			}
			if scenario == "invalid_selection" {
				bad := WithAcceptedReferenceSelections(t.Context(), []AcceptedReferenceSelection{{Pointer: "/findings/0", Reference: "ev_unobserved"}})
				if err := restarted.authorizeOperatorResultRepair(WithResultOnlyRepair(bad, id)); err == nil {
					t.Fatal("unobserved reference granted")
				}
				if currentResultCollection(restarted.todoItemByID(id)).TurnsUsed != old.TurnsUsed {
					t.Fatal("invalid selection consumed turns")
				}
				return
			}
			if err := restarted.authorizeOperatorResultRepair(WithResultOnlyRepair(selectionCtx, id)); err != nil {
				t.Fatal(err)
			}
			granted, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			restarted = reopenContractCollection(t, restarted, granted)
			cp = currentResultCollection(restarted.todoItemByID(id))
			if cp.TurnsUsed != old.TurnsUsed || cp.TurnLimit != old.TurnLimit {
				t.Fatal("local projection changed model budget")
			}
			if scenario == "cancelled" {
				cancel()
			}
			worker := scriptWorker(textTurn("must not execute old work"))
			restarted.workerAgentOverride = worker
			_, runErr := restarted.executeTask(runCtx, taskDefFromTodoItem(restarted.todoItemByID(id)), id)
			item := restarted.todoItemByID(id)
			if worker.Calls() != 0 {
				t.Fatal("accepted repair replayed worker tools")
			}
			if scenario == "cancelled" {
				if runErr == nil || item.TypedResult != nil || currentResultCollection(item).Work != old.Work {
					t.Fatal("cancelled repair lost work or published a result")
				}
				saved, err := ReadResumeSource(c.session.Workspace, branch, "")
				if err != nil {
					t.Fatalf("cancelled checkpoint not replayable: %v", err)
				}
				restarted = reopenContractCollection(t, restarted, saved)
				restarted.executionRunID = "child-cancelled-reference-repair"
				restarted.runInputSelectionRunID = restarted.executionRunID
				nextCtx := acceptedReferenceTestContext(t, restarted, t.Context(), id)
				if err := restarted.authorizeOperatorResultRepair(WithResultOnlyRepair(nextCtx, id)); err != nil {
					t.Fatal(err)
				}
				restarted.workerAgentOverride = worker
				_, runErr = restarted.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(restarted.todoItemByID(id)), id)
				item = restarted.todoItemByID(id)
				if worker.Calls() != 0 {
					t.Fatal("restart replayed worker")
				}

			}
			if runErr != nil || item.Status != TaskDone || item.TypedResult == nil {
				t.Fatalf("repair failed: status=%s err=%v", item.Status, runErr)
			}
			digest, err := acceptedResultSemanticHash(item.TypedResult, plan.Tasks[0].Collection.AcceptedResult.Pointers)
			if err != nil || digest != plan.Tasks[0].Collection.AcceptedResult.SemanticSHA256 {
				t.Fatal("accepted repair changed semantic claims")
			}
			if currentResultCollection(item).Work != old.Work {
				t.Fatal("work seal changed")
			}
			if scenario == "verbatim" {
				if item.TypedResult.RawOutputRef == nil || item.TypedResult.RawOutputRef.ID != original.TypedResult.RawOutputRef.ID {
					t.Fatal("verbatim transcript identity changed")
				}
				last := item.TypedResult.Evidence[len(item.TypedResult.Evidence)-1]
				if !VerifyEvidenceSignature(last, "", id, old.SourceRunID) {
					t.Fatal("verbatim runtime evidence was not signed by the current runtime")
				}
			}
			if err := restarted.Close(); err != nil {
				t.Fatal(err)
			}
			replayed, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil || replayed.Session.Tasks[0].Status != TaskDone || !sameCanonicalEvidence(replayed.Session.Tasks[0].TypedResult, item.TypedResult) {
				t.Fatalf("accepted repair failed replay: %v", err)
			}
			if scenario == "correct" {
				finishAcceptedRepairHandoff(t, restarted, replayed, reference, verifyHandoff)
			}
		})
	}
}

func finishAcceptedRepairHandoff(t *testing.T, c *Coordinator, replayed *ResumeSource, reference ArtifactRef, verifyHandoff VerificationSpec) {
	t.Helper()
	c = reopenContractCollection(t, c, replayed)
	if err := c.SetAcceptanceSpec(AcceptanceSpec{RequireNoUnresolvedTasks: true}); err != nil {
		t.Fatal(err)
	}
	if result, err := c.runTeamAcceptance(t.Context()); err == nil || result.IsPassed() {
		t.Fatal("repaired result skipped workflow acceptance")
	}
	source := replayed.Session.Tasks[0]
	base := cloneExecutionReceipt(source.ExecutionReceipt)
	base.RunID = c.executionRunID
	for _, pending := range replayed.Session.Tasks[1:3] {
		deps := c.dependencyResultsForBinding(pending.ID)
		if len(deps) != 1 {
			t.Fatalf("task %s received %d accepted dependencies", pending.ID, len(deps))
		}
		if result, err := executeTaskResultAssertVerification(c.projectDir, verifyHandoff, &deps[0]); err != nil || result.ExitCode != 0 {
			t.Fatalf("task %s rejected its recovered dependency: %v", pending.ID, err)
		}
		completeAcceptedRepairDependent(t, c, pending, taskDefFromTodoItem(pending), &base, reference)
	}
	if result, err := c.runTeamAcceptance(t.Context()); err != nil || !result.IsPassed() {
		t.Fatalf("final acceptance after replay and two handoffs: %+v %v", result, err)
	}
	if got := c.todoItemByID(replayed.Session.Tasks[3].ID); !sameCanonicalEvidence(got, replayed.Session.Tasks[3]) {
		t.Fatal("unrelated completed occurrence changed")
	}
}

func assertAcceptedRepairContextClaims(t *testing.T, c *Coordinator, id string, original *TaskResult) {
	t.Helper()
	for _, scenario := range []string{"unchanged", "note", "status", "identity", "evidence", "added", "removed", "decision", "memory"} {
		t.Run(scenario, func(t *testing.T) {
			candidate := cloneTaskResult(original)
			switch scenario {
			case "note":
				candidate.AssumptionChecks[0].Note = "new judgment"
			case "status":
				candidate.AssumptionChecks[0].Status = AssumptionContradicted
			case "identity":
				candidate.AssumptionChecks[0].AssumptionID = "unrelated"
			case "evidence":
				candidate.AssumptionChecks[0].Evidence = []string{"unrelated"}
			case "added":
				candidate.AssumptionChecks = append(candidate.AssumptionChecks, AssumptionCheck{AssumptionID: "new", Status: AssumptionSupported})
			case "removed":
				candidate.AssumptionChecks = nil
			case "decision":
				if err := c.armDiscipline(t.Context(), id, TaskDef{}, disciplinePolicy(CommitGatePolicy{}, StopPolicy{}, ReplanPolicy{}), &DecisionRecord{ID: "governing"}); err != nil {
					t.Fatal(err)
				}
				defer c.disarmDiscipline(id)
			case "memory":
				candidate.MemoryUses = []MemoryUseRef{{ContextItemID: "new"}}
			}
			before := mustReadEvents(t, c.eventStore)
			if err := c.validateRecoveryContextClaims(t.Context(), id, candidate); (err == nil) != (scenario == "unchanged") {
				t.Fatalf("context admission = %v", err)
			}
			if !reflect.DeepEqual(before, mustReadEvents(t, c.eventStore)) {
				t.Fatal("context preview wrote events")
			}
		})
	}
}

func acceptedReferenceTestContext(t *testing.T, c *Coordinator, ctx context.Context, id string) context.Context {
	t.Helper()
	a, err := c.AssessResultCollectionRecovery(ctx, id)
	if err != nil || a.Disposition != RecoveryBlocked || len(a.ReferenceChoices) == 0 {
		t.Fatalf("expected exact reference menu: %+v %v", a, err)
	}
	proof := acceptedRepairProof(a.Checkpoint)
	selections := make([]AcceptedReferenceSelection, 0, len(proof.Pointers))
	for _, pointer := range proof.Pointers {
		selections = append(selections, AcceptedReferenceSelection{Pointer: pointer, Reference: a.ReferenceChoices[0].Key})
	}
	return WithAcceptedReferenceSelections(ctx, selections)
}
