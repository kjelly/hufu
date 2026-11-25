package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

func TestNormalizedCollectionAuthenticatesSourcesBeforeRepairingEvidenceKeys(t *testing.T) {
	schema := strings.Replace(collectionReferenceSchema, `"x-hufu-evidence-references":`, `"x-hufu-evidence-keys":{"runtime_owned_files_read":true,"pointers":["/references/*/paths/*"]},"x-hufu-evidence-references":`, 1)
	const wire = `{"status":"success","summary":"original","structured_payload":{}}`
	const corrected = `{"status":"success","summary":"corrected","structured_payload":{"claims":[]}}`
	for _, tc := range []struct {
		name, schema, output string
		want                 RepairFailureReason
	}{
		{"foreign source with unknown key", schema, `{"references":[{"producer":"foreign","paths":["ev_000000000000"]}]}`, RepairFailureSource},
		{"repairable key selection", observedFilesTestSchema(), `{"claims":[{"evidence":["ev_000000000000"]}]}`, RepairFailureInvalidSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compiled := compileFixtureJSONSchema(t, "normalized-evidence-choices", tc.schema)
			var normalizations atomic.Int32
			endpoint, calls := collectionEvidenceFeedbackServer(t, func(int32) string { return corrected })
			ref := compiled.ref(true)
			configure := func(spec *TodoSpec) { spec.ResultContract = &ref }
			install := func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
				c.session.Agents["worker"].Tools = "view"
				bindTestNormalizer(t, c, &TodoItem{ResultContract: &ref}, &mockNormalizerProvider{fn: func(_ context.Context, raw []byte) ([]byte, error) {
					normalizations.Add(1)
					if string(raw) == "{}" {
						return []byte(tc.output), nil
					}
					return raw, nil
				}})
			}
			t.Run("direct", func(t *testing.T) {
				c, item, ctx, receipt := resultCollectionTaskFixture(t, endpoint, false, configure, install)
				observeCollectionArtifact(t, c, item, ctx, &receipt, "authenticated observation")
				ctx = context.WithValue(ctx, artifactAccessScopeKey, receipt.ArtifactScope)
				ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: receipt.RunID, TaskID: item.ID, AgentName: item.Agent, Attempt: 1, ModelExecutionID: receipt.ModelExecutionID})
				before := normalizations.Load()
				response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: wire})
				if err != nil || !response.IsError || submissionFailureMetadata(response.Metadata) != tc.want || normalizations.Load() != before+1 {
					t.Fatalf("direct submission=%+v normalizations=%d err=%v", response, normalizations.Load()-before, err)
				}
				if tc.want == RepairFailureSource && !strings.Contains(response.Content, "foreign") {
					t.Fatalf("direct submission did not authenticate the producer beside the unknown key: %s", response.Content)
				}
			})
			for _, route := range []string{"live", "reopened"} {
				t.Run(route, func(t *testing.T) {
					c, item, ctx, receipt := resultCollectionTaskFixture(t, endpoint, true, configure, install)
					observeCollectionArtifact(t, c, item, ctx, &receipt, "authenticated observation")
					before := normalizations.Load()
					beforeCalls := calls.Load()
					cp, err := c.beginResultCollection(ctx, item.ID, receipt, "original work")
					if err == nil {
						cp, err = c.reserveResultCollection(ctx, cp, "original-collector", []byte(`{"tools":[]}`))
					}
					if err == nil {
						cp, err = c.recordCollectionResponse(ctx, cp, []byte(wire))
					}
					if err != nil {
						t.Fatal(err)
					}
					if route == "live" {
						err = c.collectWorkResult(ctx, c.session.Agents[item.Agent], cp)
						if tc.want == RepairFailureSource && localResultRejectionReason(err) != tc.want {
							t.Fatalf("live collection classification: %v", err)
						}
					} else {
						cp, err = c.prepareCollectionClaims(ctx, cp)
						if err != nil {
							t.Fatal(err)
						}
						c, err = resumeRejectedEvidenceCollection(t, c, item, cp, ctx, tc.want)
					}
					result := c.GetTaskResult(item.ID)
					turns, executions := calls.Load()-beforeCalls, normalizations.Load()-before
					if tc.want == RepairFailureSource {
						if err == nil || turns != 0 || result != nil || executions != 1 {
							t.Fatalf("foreign source received a correction: calls=%d normalizations=%d err=%v", turns, executions, err)
						}
					} else if err != nil || turns != 1 || result == nil || executions != 2 {
						t.Fatalf("selection did not receive exactly one correction: calls=%d normalizations=%d err=%v", turns, executions, err)
					}
				})
			}
		})
	}
}

func TestResultCollectionRecoveryAuthenticatesNormalizationAndReadableSources(t *testing.T) {
	const draft = `{"verdict":"approve"}`
	for _, tc := range []struct {
		name      string
		body      string
		prepare   bool
		want      ResultCollectionRecoveryDisposition
		checkCode string
	}{
		{"prepared with seal", `{"status":"success","summary":"reviewed","structured_payload":` + draft + `}`, true, RecoveryCommitLocal, ""},
		{"prepared missing seal", `{"status":"success","summary":"reviewed","structured_payload":` + draft + `}`, true, RecoveryBlocked, "normalization_stage"},
		{"truncated after receipt", `{"receipt_ids":["unbound"],"summary":`, false, RecoveryBlocked, "receipt_claims"},
		{"trailing data after receipt", `{"receipt_ids":["unbound"],"status":"success","summary":"reviewed"} trailing`, false, RecoveryBlocked, "receipt_claims"},
		{"framed syntax error after receipt", "```json\n" + `{"receipt_ids":["unbound"],"structured_payload":` + "\n```", false, RecoveryBlocked, "receipt_claims"},
		{"malformed without a receipt claim", `{"summary":`, false, RecoveryRepairClaims, ""},
		{"malformed envelope before normalization", `{"status":"success","summary":42,"structured_payload":` + draft + `}`, false, RecoveryRepairClaims, ""},
		{"runtime-owned files read before normalization", `{"status":"success","summary":"reviewed","structured_payload":{"claims":[]},"files_read":[{"path":"ev_000000000000"}]}`, false, RecoveryRepairClaims, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema, correctedDraft := reviewResultSchema, draft
			runtimeFiles := tc.name == "runtime-owned files read before normalization"
			if runtimeFiles {
				schema, correctedDraft = observedFilesTestSchema(), `{"claims":[]}`
			}
			compiled, err := CompileResultContractDocument("normalizer.json", []byte(schema))
			if err != nil {
				t.Fatal(err)
			}
			ref := compiled.ref(true)
			var calls int
			c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
				spec.ResultContract = &ref
			}, func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
				if runtimeFiles {
					c.session.Agents["worker"].Tools = "view"
				}
				bindTestNormalizer(t, c, &TodoItem{ResultContract: &ref}, &mockNormalizerProvider{fn: func(_ context.Context, raw []byte) ([]byte, error) {
					calls++
					return raw, nil
				}})
			})
			var observed ArtifactRef
			if runtimeFiles {
				observed = observeCollectionArtifact(t, c, item, ctx, &receipt, "authenticated source")
			}
			cp, err := c.beginResultCollection(ctx, item.ID, receipt, "completed original work")
			if err == nil {
				cp, err = c.reserveResultCollection(ctx, cp, "collector", []byte(`{"tools":[]}`))
			}
			if err == nil {
				cp, err = c.recordCollectionResponse(ctx, cp, []byte(tc.body))
			}
			if tc.prepare && err == nil {
				cp, err = c.prepareCollectionClaims(ctx, cp)
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "malformed envelope before normalization" || runtimeFiles {
				if _, err := c.prepareCollectionClaims(ctx, cp); localResultRejectionReason(err) != RepairFailureInvalidSchema || calls != 0 {
					t.Fatalf("live claims admitted malformed envelope: calls=%d, error=%v", calls, err)
				}
			}
			if tc.prepare && tc.want == RecoveryBlocked {
				key := normalizationDocument(ref, normalizationOccurrence(submitResultRuntimeIdentity{}, cp), sha256HexBytes([]byte(draft)))
				if err := os.Remove(filepath.Join(runtimeEffectRecordPath(c.artifactStoreRootPath(), key+"/output"), runtimeEffectBodyFile)); err != nil {
					t.Fatal(err)
				}
				if err := c.commitPreparedCollection(ctx, cp); localResultRejectionReason(err) != RepairFailureSource {
					t.Fatalf("commit accepted an unsealed prepared result: %v", err)
				}
			}
			c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureCommit, Checkpoint: cp, Cause: errors.New("storage interrupted")})
			c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Desc, item.ID, "storage interrupted", NeedsHuman, FailureProtocol, TaskBlocked, "original report")
			c = reopenRecoveryCoordinator(t, c)
			a, err := c.AssessResultCollectionRecovery(t.Context(), item.ID)
			if err != nil || a.Disposition != tc.want || a.AdditionalTurns != 0 || a.RemainingTurns != 1 {
				t.Fatalf("assessment=%+v, error=%v", a, err)
			}
			wantCalls := 0
			if tc.prepare {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("recovery ran the normalizer %d times, want %d", calls, wantCalls)
			}
			if tc.want != RecoveryBlocked {
				if err := validateRecoveryAssessment(a); err != nil {
					t.Fatalf("assessment is not eligible: %v, checks=%+v", err, a.Checks)
				}
				if tc.name == "malformed envelope before normalization" || runtimeFiles {
					c.executionRunID = "repair-normalizer-envelope"
					if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID)); err != nil {
						t.Fatal(err)
					}
					c = reopenRecoveryCoordinator(t, c)
					c.executionRunID = "repair-normalizer-envelope"
					granted := currentResultCollection(c.todoItemByID(item.ID))
					ctx, err := c.restoreCollectionContext(WithSessionResume(t.Context()), granted)
					if err != nil {
						t.Fatal(err)
					}
					reserved, err := c.reserveResultCollection(ctx, granted, "corrected-collector", []byte(`{"tools":[]}`))
					if err != nil || reserved.TurnsUsed != 2 || reserved.TurnLimit != 2 || calls != 0 {
						t.Fatalf("recovery did not reserve exactly the remaining correction: %+v, %v", reserved, err)
					}
					if _, err := c.recordCollectionResponse(ctx, reserved, []byte(`{"status":"success","summary":"reviewed","structured_payload":`+correctedDraft+`}`)); err != nil {
						t.Fatal(err)
					}
					c = reopenRecoveryCoordinator(t, c)
					c.executionRunID = "repair-normalizer-envelope"
					c.workerAgentOverride = scriptWorker(failTurn(errors.New("unexpected worker call")))
					_, err = c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(item.ID)), item.ID)
					settled := c.todoItemByID(item.ID)
					if err != nil || calls != 1 || settled.Status != TaskDone {
						t.Fatalf("corrected envelope did not settle exactly once: normalizer calls=%d, error=%v", calls, err)
					}
					if runtimeFiles && (len(settled.TypedResult.FilesRead) != 1 || settled.TypedResult.FilesRead[0].Path != observed.ID) {
						t.Fatalf("corrected envelope did not bind the original observation: %+v", settled.TypedResult.FilesRead)
					}
				}
				return
			}
			if !slices.ContainsFunc(a.Checks, func(check ResultCollectionRecoveryCheck) bool {
				return check.Code == tc.checkCode && !check.Passed
			}) {
				t.Fatalf("assessment did not reject at %s: %+v", tc.checkCode, a.Checks)
			}
			c.executionRunID = "explicit-result-recovery"
			err = c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID))
			if err == nil || len(collectionRepairGrants(c.todoItemByID(item.ID).ExecutionReceipt)) != 0 {
				t.Fatalf("recovery authorized an invalid result: %v", err)
			}
		})
	}
}
