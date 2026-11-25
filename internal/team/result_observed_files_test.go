package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/tools"
)

func observedFilesTestSchema() string {
	return strings.Replace(artifactKeysTestSchema, `"pointers":`, `"runtime_owned_files_read":true,"pointers":`, 1)
}

func TestResultCollectionAuthenticatesObservedSourcesBeforeCollector(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "temporary_io"
		if corrupt {
			name = "corrupt"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			var proposal atomic.Pointer[string]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
					return
				}
				calls.Add(1)
				writeNativeRepairStream(t, w, map[string]any{"content": *proposal.Load()}, "stop", "test")
			}))
			t.Cleanup(server.Close)
			c, item, cp, ref := beginCollectionWithObservedArtifact(t, server.URL+"/v1")
			proposal.Store(new(fmt.Sprintf(`{"status":"success","summary":"original evidence verified","structured_payload":{"claims":[{"evidence":[%q]}]}}`, ref.ID)))
			path := normalizedArtifactData(c, ref.ID)
			var restore func()
			want := RepairFailureCommit
			if corrupt {
				want = RepairFailureSource
				if err := os.WriteFile(path, []byte("replaced source bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				restore = faultNormalizedEvidencePath(t, path, true)
			}
			c = reopenRecoveryCoordinator(t, c)
			forbidden := scriptWorker(failTurn(errors.New("unexpected work or legacy repair")))
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			c.executionRunID = "read-original-observed-source"
			ctx := WithSessionResume(t.Context())
			count, err := c.ResumeInterruptedTasks(ctx)
			after := todoByID(t, c, item.ID)
			pinned := currentResultCollection(after)
			if count != 1 || localResultRejectionReason(err) != want || calls.Load() != 0 || forbidden.Calls() != 0 ||
				pinned == nil || pinned.Work != cp.Work || pinned.TurnsUsed != cp.TurnsUsed {
				t.Fatalf("count=%d status=%s checkpoint=%+v collector=%d work=%d err=%v", count, after.Status, pinned, calls.Load(), forbidden.Calls(), err)
			}
			if corrupt {
				if after.Status != TaskBlocked || pinned.FailureReason != RepairFailureSource {
					t.Fatalf("corrupt source was not blocked: %+v", pinned)
				}
				return
			}
			if after.Status != TaskInProgress || pinned.State != ResultCollectionPending {
				t.Fatalf("temporary fault replaced pending checkpoint: %+v", pinned)
			}
			restore()
			c = reopenRecoveryCoordinator(t, c)
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			c.executionRunID = "resume-observed-source"
			count, err = c.ResumeInterruptedTasks(ctx)
			after = todoByID(t, c, item.ID)
			pinned = currentResultCollection(after)
			if err != nil || count != 1 || after.Status != TaskDone || calls.Load() != 1 || forbidden.Calls() != 0 ||
				pinned == nil || pinned.Work != cp.Work || pinned.TurnsUsed != 1 || after.TypedResult == nil ||
				!slices.Equal(after.TypedResult.FilesRead, []FileRef{{Path: ref.ID}}) {
				t.Fatalf("recovered count=%d status=%s checkpoint=%+v collector=%d work=%d err=%v", count, after.Status, pinned, calls.Load(), forbidden.Calls(), err)
			}
		})
	}
}

func beginCollectionWithObservedArtifact(t *testing.T, endpoint string) (*Coordinator, *TodoItem, *ResultCollectionCheckpoint, ArtifactRef) {
	t.Helper()
	compiled, err := CompileResultContractDocument("keys.json", []byte(observedFilesTestSchema()))
	if err != nil {
		t.Fatal(err)
	}
	c, item, ctx, receipt := resultCollectionTaskFixture(t, endpoint, true, func(spec *TodoSpec) {
		spec.ResultContract = new(compiled.ref(true))
	}, func(c *Coordinator) {
		c.session.Agents["worker"].Tools = "view"
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	})
	source := observeCollectionArtifact(t, c, item, ctx, &receipt, "authorized observed source")
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "complete work that read the original source")
	if err != nil {
		t.Fatal(err)
	}
	return c, item, cp, source
}

func observeCollectionArtifact(t *testing.T, c *Coordinator, item *TodoItem, ctx context.Context, receipt *ExecutionReceipt, content string) ArtifactRef {
	t.Helper()
	store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Put(ctx, PutArtifactRequest{Kind: "test", Path: "source.txt", Content: []byte(content), TaskID: "producer"})
	if err != nil {
		t.Fatal(err)
	}
	receipt.ArtifactScope = &ArtifactAccessScope{RunID: receipt.RunID, TaskID: item.ID, Attempt: 1, AuthorizedRefs: []ArtifactRef{source.ArtifactRef}}
	observeScopedCollectionArtifact(t, c, item, ctx, receipt, source.ID)
	return source.ArtifactRef
}

func observeScopedCollectionArtifact(t *testing.T, c *Coordinator, item *TodoItem, ctx context.Context, receipt *ExecutionReceipt, sourceID string) {
	t.Helper()
	ctx = context.WithValue(ctx, artifactAccessScopeKey, receipt.ArtifactScope)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: receipt.RunID, TaskID: item.ID, AgentName: item.Agent, Attempt: 1, ModelExecutionID: receipt.ModelExecutionID})
	ctx = tools.SetToolsAllowed(ctx, []string{"view"})
	view := tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))
	call := fantasy.ToolCall{ID: "observed-source", Name: "view", Input: fmt.Sprintf(`{"artifact_ref":%q,"byte_limit":1024}`, sourceID)}
	response, err := view.Run(ctx, call)
	if err != nil || response.IsError {
		t.Fatalf("view: %v %s", err, response.Content)
	}
	if err := c.captureDecisionArtifactRead(ctx, view, call, &response); err != nil {
		t.Fatal(err)
	}
}

func TestResultCollectionFanoutAuthenticatesAllObservedSourcesBeforeProvider(t *testing.T) {
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		http.Error(w, "source preflight must precede model discovery and collection", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	compiled, err := CompileResultContractDocument("keys.json", []byte(observedFilesTestSchema()))
	if err != nil {
		t.Fatal(err)
	}
	c, item, _, receipt := resultCollectionTaskFixture(t, server.URL+"/v1", true, func(spec *TodoSpec) {
		spec.ResultContract = new(compiled.ref(true))
		spec.ExecutionTopology = []execution.ExecutionTarget{spec.ExecutionTarget, spec.ExecutionTarget}
	}, func(c *Coordinator) {
		c.session.Agents["worker"].Tools = "view"
		c.session.Agents["worker"].ExtraModels = []string{"ollama/test"}
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	})
	var checkpoints [2]*ResultCollectionCheckpoint
	for i := range checkpoints {
		var source ArtifactRef
		_, _, checkpoints[i], err = beginFanoutWorkForRecovery(t, c, item, receipt, i, func(leaf *Coordinator, ctx context.Context, receipt *ExecutionReceipt) {
			source = observeCollectionArtifact(t, leaf, item, ctx, receipt, fmt.Sprintf("slot %d original source", i))
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if err := os.WriteFile(normalizedArtifactData(c, source.ID), []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	c = reopenRecoveryCoordinator(t, c)
	c.executionRunID = "cold-two-local-slots"
	var boundaries atomic.Int32
	ctx, cancel := context.WithCancelCause(WithSessionResume(t.Context()))
	defer cancel(nil)
	c.setInvocationWatchdog(c.newInvocationWatchdog(ctx, cancel))
	defer c.setInvocationWatchdog(nil)
	c.providerBoundaryStarted = false
	c.providerBoundaryStart = func(context.Context, string) error {
		boundaries.Add(1)
		return errors.New("unverified sibling crossed the provider boundary")
	}
	count, err := c.ResumeInterruptedTasks(ctx)
	after := todoByID(t, c, item.ID)
	if count != 1 || localResultRejectionReason(err) != RepairFailureSource || after.Status != TaskBlocked || boundaries.Load() != 0 || providerCalls.Load() != 0 {
		t.Fatalf("count=%d status=%s boundaries=%d calls=%d err=%v", count, after.Status, boundaries.Load(), providerCalls.Load(), err)
	}
	if !strings.Contains(err.Error(), "observed artifact source integrity") {
		t.Fatalf("recovery did not reach the damaged observation: %v", err)
	}
	for i, original := range checkpoints {
		if actual := currentFanoutCollection(after, i); !sameCanonicalEvidence(actual, original) {
			t.Fatalf("slot %d changed during failed preflight: %+v", i, actual)
		}
	}
}

func TestResultCollectionFanoutChecksUnsettledDecisionObservationBeforeProvider(t *testing.T) {
	c, item, _, receipt, decisionCalls := configuredDecisionCollectionFixture(t, "http://127.0.0.1:1/v1", 1, 0, func(spec *TodoSpec) {
		spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
	}, nil)
	_, _, first, err := beginFanoutWorkForRecovery(t, c, item, receipt, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, second, err := beginFanoutWorkForRecovery(t, c, item, receipt, 1, func(leaf *Coordinator, ctx context.Context, receipt *ExecutionReceipt) {
		observeScopedCollectionArtifact(t, leaf, item, ctx, receipt, receipt.ArtifactScope.AuthorizedRefs[0].ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var output string
	for _, event := range events {
		if event.Type != string(EventDecisionArtifactObserved) || event.TaskID != item.ID {
			continue
		}
		var obs decisionArtifactObservation
		if err := json.Unmarshal(event.Payload, &obs); err != nil {
			t.Fatal(err)
		}
		if obs.ModelExecutionID == second.ModelExecutionID {
			output = obs.Output.ID
		}
	}
	if output == "" {
		t.Fatal("fixture did not record the authentic decision-only observation")
	}
	damageFanoutCAS(t, normalizedArtifactData(c, output), false)
	assertFanoutPreflightRejects(t, c, item, "observation_integrity_invalid", first, second)
	if decisionCalls.Load() != 0 {
		t.Fatalf("unsettled decision dispatched: %d", decisionCalls.Load())
	}
}

func TestResultCollectionFanoutChecksAcceptedObservedSourcesAfterParentWriteFault(t *testing.T) {
	for _, scenario := range []string{"corrupt", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			c, item, checkpoints, cited := acceptedObservedFanoutFixture(t)
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			execJournal(t, c.session.Workspace, `CREATE TRIGGER refuse_cited_parent BEFORE INSERT ON events WHEN NEW.type='task_completed' BEGIN SELECT RAISE(ABORT, 'fixture parent completion write failure'); END`)
			count, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			if count != 1 || localResultRejectionReason(err) != RepairFailureCommit {
				t.Fatalf("expected aggregate publication fault: count=%d err=%v", count, err)
			}
			execJournal(t, c.session.Workspace, "DROP TRIGGER refuse_cited_parent")
			damageFanoutCAS(t, normalizedArtifactData(c, cited.ID), scenario == "missing")
			assertFanoutPreflightRejects(t, c, item, "observed artifact source integrity", checkpoints...)
		})
	}
}

func acceptedObservedFanoutFixture(t *testing.T) (*Coordinator, *TodoItem, []*ResultCollectionCheckpoint, ArtifactRef) {
	t.Helper()
	compiled, err := CompileResultContractDocument("keys.json", []byte(observedFilesTestSchema()))
	if err != nil {
		t.Fatal(err)
	}
	c, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
		spec.ResultContract = new(compiled.ref(true))
		spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
	}, func(c *Coordinator) {
		c.session.Agents["worker"].Tools = "view"
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	})
	var checkpoints []*ResultCollectionCheckpoint
	var source ArtifactRef
	for index := range 2 {
		leaf, ctx, cp, err := beginFanoutWorkForRecovery(t, c, item, receipt, index, func(leaf *Coordinator, ctx context.Context, receipt *ExecutionReceipt) {
			source = observeCollectionArtifact(t, leaf, item, ctx, receipt, fmt.Sprintf("accepted original source in slot %d", index))
		})
		if err != nil {
			t.Fatal(err)
		}
		commitNormalizedCollectionFixture(t, leaf, ctx, cp, fmt.Sprintf(`{"claims":[{"evidence":[%q]}]}`, source.ID))
		current := todoByID(t, leaf, item.ID)
		cp = currentResultCollection(current)
		result, err := leaf.acceptedCollectionResult(t.Context(), current, cp)
		if err != nil || result == nil || len(result.FilesRead) != 1 || result.FilesRead[0].Path != source.ID {
			t.Fatalf("committed result lacks its optional observed source: %+v err=%v", result, err)
		}
		checkpoints = append(checkpoints, cp)
	}
	return reopenRecoveryCoordinator(t, c), item, checkpoints, source
}

func TestRuntimeOwnedFilesReadAdmission(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"full_id", "short_key", "empty", "unread", "other_model", "corrupt", "override", "null_override", "empty_override", "typo_full_id", "typo_key", "typo_full_id_receipt", "typo_key_receipt"} {
		t.Run(scenario, func(t *testing.T) {
			f := artifactKeysFixtureForSchema(t, observedFilesTestSchema())
			if scenario != "unread" && scenario != "empty" {
				f.readSource(t)
			}
			evidence := f.ref.ID
			if scenario == "short_key" {
				evidence = f.key
			}
			if strings.HasPrefix(scenario, "typo_full_id") {
				last := "0"
				if strings.HasSuffix(evidence, last) {
					last = "1"
				}
				evidence = evidence[:len(evidence)-1] + last
			} else if strings.HasPrefix(scenario, "typo_key") {
				evidence = "ev_000000000000"
			}
			payload := fmt.Sprintf(`{"claims":[{"evidence":[%q]}]}`, evidence)
			if scenario == "empty" {
				payload = `{"claims":[]}`
			}
			fields := map[string]any{"status": "success", "summary": "reviewed", "structured_payload": jsontext.Value(payload)}
			if strings.HasSuffix(scenario, "_receipt") {
				fields["receipt_ids"] = []string{"forged"}
			}
			switch scenario {
			case "other_model":
				f.ctx = withInvocationMetadata(f.ctx, InvocationMetadata{RunID: "run-key", TaskID: f.item.ID, AgentName: f.item.Agent, Attempt: 1, ModelExecutionID: "different-model"})
			case "corrupt":
				if err := os.WriteFile(filepath.Join(f.c.session.Workspace, logsDir, "artifacts", "data", f.ref.ID), []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "override":
				fields["files_read"] = []any{map[string]any{"path": f.ref.ID}}
			case "null_override":
				fields["files_read"] = nil
			case "empty_override":
				fields["files_read"] = []any{}
			}
			tool := &submitResultTool{coordinator: f.c, todoID: f.item.ID}
			if _, exposed := tool.Info().Parameters["files_read"]; exposed {
				t.Fatal("runtime-owned field exposed to the model")
			}
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			response, err := tool.Run(f.ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(raw)})
			valid := scenario == "full_id" || scenario == "short_key" || scenario == "empty"
			if err != nil || response.IsError == valid {
				t.Fatalf("valid=%t response=%+v error=%v", valid, response, err)
			}
			result := f.c.GetTaskResult(f.item.ID)
			if !valid {
				if result != nil {
					t.Fatal("rejected evidence published")
				}
				if strings.HasSuffix(scenario, "_receipt") {
					if submissionFailureMetadata(response.Metadata) != RepairFailureSource {
						t.Fatal("citation typo concealed a forged receipt")
					}
					return
				}
				if strings.HasPrefix(scenario, "typo_") {
					if reason, replay := classifyRepairFailure(failureResponseSteps(response), nil); reason != RepairFailureInvalidSchema || replay {
						t.Fatalf("citation typo prevented result-only protocol repair: reason=%s work_replay=%t", reason, replay)
					}
					if !strings.Contains(response.Content, f.key) {
						t.Fatal("invalid citation did not receive exact issued choices")
					}
					if !strings.Contains(response.Content, "/structured_payload/claims/0/evidence/0") {
						t.Fatalf("invalid citation lacks an exact field path: %s", response.Content)
					}
					fields["structured_payload"] = jsontext.Value(fmt.Sprintf(`{"claims":[{"evidence":[%q]}]}`, f.key))
					repaired, err := json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					response, err = tool.Run(f.ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(repaired)})
					accepted := f.c.GetTaskResult(f.item.ID)
					if err != nil || response.IsError || accepted == nil || len(accepted.FilesRead) != 1 || accepted.FilesRead[0].Path != f.ref.ID {
						t.Fatalf("same-work citation correction failed: %+v %v", response, err)
					}
				}
				return
			}
			expected := []FileRef{}
			if scenario != "empty" {
				expected = append(expected, FileRef{Path: f.ref.ID})
			}
			if !slices.Equal(result.FilesRead, expected) {
				t.Fatalf("read projection=%+v", result.FilesRead)
			}
			if err := f.c.CommitTaskTransition(t.Context(), f.item.ID, TaskPending, TaskInProgress, "work returned", "", nil); err != nil {
				t.Fatal(err)
			}
			if err := f.c.CommitTaskTransition(t.Context(), f.item.ID, TaskInProgress, TaskDone, "accepted", "", nil); err != nil {
				t.Fatal(err)
			}
			events, err := f.c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			restored := todoItemByID(ReduceToTodoList(events), f.item.ID)
			if restored.TypedResult == nil || !slices.Equal(restored.TypedResult.FilesRead, result.FilesRead) {
				t.Fatal("replay lost canonical read evidence")
			}
		})
	}
}

func TestRuntimeOwnedFilesReadRejectsExternalProposal(t *testing.T) {
	t.Parallel()
	compiled, err := CompileResultContractDocument("reads.json", []byte(observedFilesTestSchema()))
	if err != nil {
		t.Fatal(err)
	}
	for _, reads := range [][]string{nil, {}, {"claimed-read"}} {
		proposal := &WorkerResultProposal{Status: "success", Summary: "reviewed", FilesRead: reads, StructuredPayloadJSON: new(`{"claims":[]}`)}
		request := AttemptRequest{Task: TaskDef{ResultContract: new(compiled.ref(true))}, resultContractSchema: compiled}
		if _, err := canonicalExternalStructuredPayload(request, proposal, nil); err == nil {
			t.Fatal("external proposal fabricated native read authority")
		}
	}
}

func TestRuntimeOwnedFilesReadPreflightsNativeCapability(t *testing.T) {
	t.Parallel()
	compiled, err := CompileResultContractDocument("reads.json", []byte(observedFilesTestSchema()))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"native", "external", "no_view", "denied", "mcp"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestCoordinator(t)
			def := c.session.Agents["worker"]
			def.Tools = "view"
			switch mode {
			case "external":
				def.SubagentProvider = "codex"
			case "no_view":
				def.Tools = ""
			case "denied":
				c.session.Config.ToolsDenied = []string{"view"}
			case "mcp":
				def.ForceMCP = true
			}
			if err := validateResultDecisionCatalog(c.session, def, compiled); (err == nil) != (mode == "native") {
				t.Fatalf("mode=%s err=%v", mode, err)
			}
		})
	}
}

func TestRuntimeOwnedFilesReadSelectionSchema(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"array", "fixed_rows", "no_reads"} {
		t.Run(shape, func(t *testing.T) {
			f := artifactKeysFixtureForSchema(t, observedFilesTestSchema())
			if shape != "no_reads" {
				f.readSource(t)
			}
			compiled := f.c.session.ResultContracts["keys.json"]
			refs, err := f.c.verifiedResultEvidenceKeyRefs(f.ctx, f.item.ID, compiled)
			if err != nil {
				t.Fatal(err)
			}
			info, err := collectionGuidanceInfo(f.c, f.item.ID)
			if err != nil {
				t.Fatal(err)
			}
			schema := info.Parameters["structured_payload"].(map[string]any)
			if shape == "fixed_rows" {
				properties := schema["properties"].(map[string]any)
				row := properties["claims"].(map[string]any)["items"]
				properties["claims"] = map[string]any{"type": "object", "properties": map[string]any{"issued_row": row}, "required": []string{"issued_row"}, "additionalProperties": false}
			}
			constrainResultEvidenceChoices(schema, compiled.evidenceKeys, refs)
			for _, value := range []string{f.key, f.ref.ID, "ev_000000000000"} {
				claims := any([]any{map[string]any{"evidence": []string{value}}})
				if shape == "fixed_rows" {
					claims = map[string]any{"issued_row": map[string]any{"evidence": []string{value}}}
				}
				raw, err := json.Marshal(map[string]any{"status": "success", "summary": "unchanged judgment", "structured_payload": map[string]any{"claims": claims}})
				if err != nil {
					t.Fatal(err)
				}
				valid := shape != "no_reads" && (value == f.key || value == f.ref.ID)
				// The collector's complete schema/feedback validator supports
				// impossible empty-source selections, including JSON Schema not.
				issues, _, err := resultSubmissionIssues(string(raw), info)
				if err != nil || (len(issues) == 0) != valid {
					t.Fatalf("shape=%s choice=%s valid=%t issues=%v error=%v", shape, value, valid, issues, err)
				}
			}
			if shape != "fixed_rows" {
				if issues, _, err := resultSubmissionIssues(`{"status":"success","summary":"no findings","structured_payload":{"claims":[]}}`, info); err != nil || len(issues) != 0 {
					t.Fatalf("empty sources cannot report an empty review: %v %v", issues, err)
				}
			}
		})
	}
}
