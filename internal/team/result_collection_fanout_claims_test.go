package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestResultCollectionFanoutAuthenticatesNormalizerBeforeCollectingSibling(t *testing.T) {
	for _, scenario := range []struct {
		name, diagnostic  string
		reason            RepairFailureReason
		deleteStage       bool
		stopBeforePrepare bool
		projectionFault   bool
		changePendingBody bool
	}{
		{name: "interrupted before durable output", diagnostic: "normalizer invocation has no durable output", reason: RepairFailureAction},
		{name: "deleted published stage", diagnostic: "projection differs from its sealed completion", reason: RepairFailureSource, deleteStage: true},
		{name: "substituted received normalization", diagnostic: "projection differs from its sealed completion", reason: RepairFailureSource, stopBeforePrepare: true},
		{name: "substituted prepared normalization", diagnostic: "projection differs from its sealed completion", reason: RepairFailureSource},
		{name: "interrupted projection with durable output", projectionFault: true},
		{name: "substituted pending normalization", diagnostic: "body differs from its sealed digest", reason: RepairFailureSource, projectionFault: true, changePendingBody: true},
		{name: "received before first normalization"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			compiled := compileFixtureJSONSchema(t, "fanout-normalizer", reviewResultSchema)
			ref := compiled.ref(true)
			var calls atomic.Int32
			c, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
				spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
				spec.ResultContract = &ref
			}, func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
				ref.Normalizer, ref.NormalizerSHA256 = registerTestNormalizer(t, c, &mockNormalizerProvider{fn: func(context.Context, []byte) ([]byte, error) {
					calls.Add(1)
					if scenario.reason == RepairFailureAction {
						return nil, errors.New("fixture interrupted normalizer")
					}
					return []byte(`{"findings":[],"verdict":"approve"}`), nil
				}})
			})
			firstLeaf, firstCtx, first, err := beginFanoutWorkForRecovery(t, c, item, receipt, 0)
			if err != nil {
				t.Fatal(err)
			}
			leaf, ctx, affected, err := beginFanoutWorkForRecovery(t, c, item, receipt, 1)
			if err != nil {
				t.Fatal(err)
			}
			const draft = `{"findings": [], "verdict": "  APPROVE  "}`
			const response = `{"status":"success","summary":"sealed work","structured_payload":` + draft + `}`
			affected = recordFanoutResponse(t, c, leaf, ctx, affected, ResultCollectionReceived, response)
			workspace := leaf.session.Workspace
			occurrence := normalizationOccurrence(submitResultRuntimeIdentity{}, affected)
			key := normalizationDocument(ref, occurrence, sha256HexBytes([]byte(draft)))
			if scenario.projectionFault {
				execJournal(t, workspace, `CREATE TRIGGER refuse_normalizer_seal BEFORE UPDATE ON runtime_documents WHEN NEW.name LIKE 'result-normalization/%' BEGIN SELECT RAISE(ABORT, 'fixture normalizer seal fault'); END`)
				if _, err := leaf.prepareCollectionClaims(ctx, affected); localResultRejectionReason(err) != RepairFailureCommit || calls.Load() != 1 {
					t.Fatalf("expected original normalization projection fault, calls=%d err=%v", calls.Load(), err)
				}
				execJournal(t, workspace, "DROP TRIGGER refuse_normalizer_seal")
				body, found, err := workspaceDocs(workspace).getDocument(key)
				if err != nil || !found || string(body) != "null" {
					t.Fatalf("expected durable pending normalization, got %s, %v", body, err)
				}
				if scenario.changePendingBody {
					body := substitutedNormalizationBody(t, ref, occurrence, draft)
					path := filepath.Join(runtimeEffectRecordPath(c.artifactStoreRootPath(), key+"/output"), runtimeEffectBodyFile)
					if err := os.WriteFile(path, body, 0o600); err != nil {
						t.Fatal(err)
					}
					assertFanoutPreflightFailure(t, c, item, scenario.reason, scenario.diagnostic, first, affected)
					if calls.Load() != 1 {
						t.Fatalf("preflight invoked normalizer %d times, want one original invocation", calls.Load())
					}
					return
				}
			}
			if scenario.reason == "" {
				recordFanoutResponse(t, c, firstLeaf, firstCtx, first, ResultCollectionReceived, response)
				c = reopenRecoveryCoordinator(t, c)
				c.executionRunID = "resume-before-first-normalization"
				resume := WithSessionResume(t.Context())
				rejectFanoutProviderBoundary(t, c, resume)
				count, err := c.ResumeInterruptedTasks(resume)
				if count != 1 || err != nil || todoByID(t, c, item.ID).Status != TaskDone || calls.Load() != 2 {
					t.Fatalf("collected drafts should normalize once per slot: tasks=%d calls=%d err=%v", count, calls.Load(), err)
				}
				return
			}
			if scenario.reason == RepairFailureAction {
				if _, err := leaf.prepareCollectionClaims(ctx, affected); localResultRejectionReason(err) != RepairFailureAction {
					t.Fatalf("expected original normalizer failure, got %v", err)
				}
			} else {
				if scenario.stopBeforePrepare {
					execJournal(t, c.session.Workspace, `CREATE TRIGGER refuse_prepared BEFORE INSERT ON events WHEN NEW.type='task_result_collection_checkpointed' AND json_extract(NEW.payload, '$.execution_receipt.result_collection.state')='prepared' BEGIN SELECT RAISE(ABORT, 'fixture prepared publication fault'); END`)
				}
				next, err := leaf.prepareCollectionClaims(ctx, affected)
				if scenario.stopBeforePrepare {
					execJournal(t, c.session.Workspace, "DROP TRIGGER refuse_prepared")
					if err == nil || localResultRejectionReason(err) != RepairFailureCommit ||
						currentFanoutCollection(todoByID(t, leaf, item.ID), 1).State != ResultCollectionReceived {
						t.Fatalf("expected durable output before Prepared publication: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				} else {
					affected = next
				}
				if scenario.deleteStage {
					execJournal(t, workspace, `DELETE FROM runtime_documents WHERE name=?`, key)
				} else {
					execJournal(t, workspace, `UPDATE runtime_documents SET body=? WHERE name=?`, substitutedNormalizationBody(t, ref, occurrence, draft), key)
				}
			}
			assertFanoutPreflightFailure(t, c, item, scenario.reason, scenario.diagnostic, first, affected)
			if calls.Load() != 1 {
				t.Fatalf("preflight invoked normalizer %d times, want one original invocation", calls.Load())
			}
		})
	}
}

func substitutedNormalizationBody(t *testing.T, ref ResultContractRef, occurrence, draft string) []byte {
	t.Helper()
	substitute := []byte(`{"findings":[{"summary":"unsealed substitution"}],"verdict":"approve"}`)
	body, err := json.Marshal(resultNormalization{OccurrenceSHA256: occurrence, ProviderSHA256: ref.NormalizerSHA256,
		OutputSHA256: sha256HexBytes(substitute), Raw: []byte(draft), Output: substitute})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestResultCollectionFanoutAuthenticatesReadableSourcesBeforeSchemaRepair(t *testing.T) {
	const duplicate = `{"status":"success","summary":"reviewed","structured_payload":{"verdict":"approve","verdict":"reject"},"receipt_ids":["forged"]}`
	const unknownArtifactMember = `{"status":"success","summary":"reviewed","artifacts":[{"path":"missing-readable-artifact.txt","unknown":true}],"structured_payload":{"verdict":"approve"}}`
	for _, tc := range []struct {
		wire, detail string
		assumptions  bool
		bound        bool
		envelope     bool
	}{
		{wire: duplicate, detail: "receipt"},
		{wire: "```json\n" + duplicate + "\n```", detail: "receipt"},
		{wire: unknownArtifactMember, detail: "missing-readable-artifact.txt"},
		{wire: `{"result":` + unknownArtifactMember + `,"extra":true}`, detail: "missing-readable-artifact.txt", envelope: true},
		{wire: `{"status":"success","summary":"reviewed","assumption_checks":[{"assumption_id":"forged","assumption_id":"A1","status":"unknown"}],"structured_payload":{"verdict":"approve"}}`, detail: `assumption "forged"`, assumptions: true},
		{wire: `{"status":"success","summary":"reviewed","assumption_checks":[{"assumption_id":"A1","status":"unknown","evidence":[false,"forged-digest"]}],"structured_payload":{"verdict":"approve"}}`, detail: `evidence "forged-digest"`, assumptions: true},
		{wire: `{"status":"success","summary":"reviewed","memory_uses":[{"retrieval_id":"forged-retrieval","unknown":true}],"structured_payload":{"verdict":"approve"}}`, detail: "retrieval_id is runtime-owned", bound: true},
	} {
		compiled, ref := compiledReviewContract(t, true)
		c, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
			spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
			spec.ResultContract = &ref
		}, func(c *Coordinator) {
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			ref.Normalizer, ref.NormalizerSHA256 = registerTestNormalizer(t, c, &mockNormalizerProvider{fn: func(context.Context, []byte) ([]byte, error) {
				t.Fatal("forged source reached normalizer")
				return nil, nil
			}})
		})
		_, _, pending, err := beginFanoutWorkForRecovery(t, c, item, receipt, 0)
		if err != nil {
			t.Fatal(err)
		}
		leaf, ctx, cp, err := beginFanoutWorkForRecovery(t, c, item, receipt, 1)
		if err != nil {
			t.Fatal(err)
		}
		if tc.assumptions {
			armNormalizationAssumption(t, c, item, true)
			armNormalizationAssumption(t, leaf, item, false)
		}
		request := `{"tools":[]}`
		if tc.bound {
			request = `{"tools":[],"bound_source_fields":true}`
		}
		cp, err = leaf.reserveResultCollection(ctx, cp, "local-attachment", []byte(request))
		if err != nil {
			t.Fatal(err)
		}
		var issue *agent.StructuredResponseIssue
		if tc.envelope {
			issue = &agent.StructuredResponseIssue{Code: "additional_property", Expected: "object", Actual: "object"}
		}
		cp, err = leaf.recordCollectionResponseUsage(ctx, cp, []byte(tc.wire), ExecutionUsage{}, issue)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leaf.prepareCollectionClaims(ctx, cp); err == nil || localResultRejectionReason(err) != RepairFailureSource {
			t.Fatalf("readable forged source was hidden by malformed claims: %v", err)
		}
		assertFanoutPreflightRejects(t, c, item, tc.detail, pending, cp)
	}
}

func TestResultCollectionFanoutValidatesSubmittedReadKeysBeforeActions(t *testing.T) {
	for _, reads := range []string{`["ev_000000000000"]`, `[17,{"path":"ev_000000000000"}]`, `[{"Path":"ev\u005f000000000000"}]`, `[{"path":"ev_000000000000","path":"ordinary-file.txt"}]`} {
		t.Run(reads, func(t *testing.T) {
			compiled := compileFixtureJSONSchema(t, "fanout-read-keys", artifactKeysTestSchema)
			ref := compiled.ref(true)
			calls := 0
			c, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
				spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
				spec.ResultContract = &ref
			}, func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
				ref.Normalizer, ref.NormalizerSHA256 = registerTestNormalizer(t, c, &mockNormalizerProvider{fn: func(context.Context, []byte) ([]byte, error) {
					calls++
					return []byte(`{"claims":[]}`), nil
				}})
			})
			_, _, pending, err := beginFanoutWorkForRecovery(t, c, item, receipt, 0)
			if err != nil {
				t.Fatal(err)
			}
			leaf, ctx, cp, err := beginFanoutWorkForRecovery(t, c, item, receipt, 1)
			if err != nil {
				t.Fatal(err)
			}
			cp = recordFanoutResponse(t, c, leaf, ctx, cp, ResultCollectionReceived, `{"status":"success","summary":"reviewed","structured_payload":{"claims":[]},"files_read":`+reads+`}`)
			if _, err := leaf.prepareCollectionClaims(ctx, cp); localResultRejectionReason(err) != RepairFailureSource || !strings.Contains(err.Error(), "evidence_key_unknown_or_out_of_scope") || calls != 0 {
				t.Fatalf("source admission did not stop normalization: calls=%d err=%v", calls, err)
			}
			assertFanoutPreflightRejects(t, c, item, "evidence_key_unknown_or_out_of_scope", pending, cp)
			if calls != 0 {
				t.Fatalf("fanout preflight invoked the normalizer %d times", calls)
			}
		})
	}
}
