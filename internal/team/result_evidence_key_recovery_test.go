package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

func contractCollectionArtifact(t *testing.T, c *Coordinator) ArtifactRef {
	t.Helper()
	store, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Put(t.Context(), PutArtifactRequest{Kind: "source", Path: "source.txt", Content: []byte("original observed source"), RunID: c.executionRunID, TaskID: "producer", Attempt: 1, Agent: "producer"})
	if err != nil {
		t.Fatal(err)
	}
	return source.ArtifactRef
}

func observeContractCollectionKeys(t *testing.T, c *Coordinator, item *TodoItem, ctx context.Context, receipt *ExecutionReceipt, response string, source ArtifactRef) (context.Context, string) {
	t.Helper()
	scope := &ArtifactAccessScope{RunID: receipt.RunID, TaskID: item.ID, Attempt: receipt.Attempt, AuthorizedRefs: []ArtifactRef{source}}
	ctx = context.WithValue(ctx, artifactAccessScopeKey, scope)
	ctx = context.WithValue(ctx, todoIDKey{}, item.ID)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: receipt.RunID, TaskID: item.ID, Attempt: receipt.Attempt, AgentName: item.Agent, AgentRole: "worker", ModelExecutionID: receipt.ModelExecutionID})
	ctx = tools.SetToolsAllowed(ctx, []string{"view"})
	f := &artifactKeysFixture{c: c, item: item, ctx: ctx, ref: source, view: c.gatePolicyTools([]fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))})[0]}
	f.read(t, fmt.Sprintf(`{"artifact_ref":%q}`, source.ID), "read-source")
	receipt.ArtifactScope = cloneArtifactAccessScope(scope)
	value, err := decodeUniqueJSON([]byte(response))
	if err != nil {
		t.Fatal(err)
	}
	findings := value.(map[string]any)["structured_payload"].(map[string]any)["findings"].([]any)
	for i := range findings {
		findings[i] = f.key
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, string(raw)
}

func TestContractRecoveryRepairsKeyedClaimsWithinOriginalWork(t *testing.T) {
	for _, test := range []struct{ succeeds, wrapped bool }{{true, false}, {false, false}, {true, true}, {false, true}} {
		succeeds := test.succeeds
		t.Run(fmt.Sprintf("success_%t_wrapped_%t", succeeds, test.wrapped), func(t *testing.T) {
			var calls atomic.Int32
			var accepted atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
					return
				}
				var request map[string]any
				if err := json.UnmarshalRead(r.Body, &request); err != nil {
					t.Error(err)
					return
				}
				assertCollectorResponseChannel(t, request)
				turn := calls.Add(1)
				if turn > 2 {
					t.Error("collector exceeded its bounded grant")
					http.Error(w, "extra request", 400)
					return
				}
				claims := accepted.Load().(string)
				if turn == 1 || !succeeds {
					claims = strings.Replace(claims, `"status":"success"`, `"status":42`, 1)
				}
				if turn == 2 && !strings.Contains(fmt.Sprint(request["messages"]), "format_feedback") {
					t.Error("repair lost the rejected response feedback")
				}
				envelope := fmt.Sprintf(`{"result":%s}`, claims)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"return\",\"type\":\"function\",\"function\":{\"name\":\"return_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", envelope)
			}))
			t.Cleanup(server.Close)
			invalid := strings.Replace(researchCollectionResponse(t, 14), `"status":"success"`, `"status":42`, 1)
			c, source, id := contractCollectionFixture(t, invalid, SideEffectNone, contractCollectionFixtureOptions{ArtifactKeys: true, Endpoint: server.URL + "/v1", StringEnvelope: test.wrapped})
			original := currentResultCollection(todoItemByID(source.Session.Tasks, id))
			raw, err := c.readCollectionBlob(t.Context(), original, "response", original.Response)
			if err != nil {
				t.Fatal(err)
			}
			if test.wrapped {
				var envelope struct {
					Result string `json:"result"`
				}
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Fatal(err)
				}
				raw = []byte(envelope.Result)
			}
			accepted.Store(strings.Replace(string(raw), `"status":42`, `"status":"success"`, 1))
			ctx := contractCollectionContext(t, id)
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			branch, err := c.ApplyContractRecovery(ctx, source, "bounded-keys", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			child, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			c = reopenContractCollection(t, c, child)
			a, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil || a.Disposition != RecoveryRepairClaims {
				t.Fatalf("assessment=%+v err=%v", a, err)
			}
			worker := scriptWorker(textTurn("do not replay work"))
			c.workerAgentOverride = worker
			if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
				t.Fatal(err)
			}
			_, err = c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id)
			item := c.todoItemByID(id)
			if (err == nil) != succeeds || calls.Load() != 2 || worker.Calls() != 0 || succeeds && item.Status != TaskDone || !succeeds && item.Status != TaskBlocked {
				t.Fatalf("succeeds=%t calls=%d work=%d status=%s err=%v", succeeds, calls.Load(), worker.Calls(), item.Status, err)
			}
			cp := currentResultCollection(item)
			if cp.Work != original.Work || cp.TurnsUsed != original.TurnsUsed+2 {
				t.Fatal("work or consumed budget changed")
			}
			if _, err := ReadResumeSource(c.session.Workspace, branch, ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestContractRecoveryPreservesOriginalArtifactEvidenceKeys(t *testing.T) {
	c, source, id := contractCollectionFixture(t, researchCollectionResponse(t, 14), SideEffectNone, contractCollectionFixtureOptions{ArtifactKeys: true})
	original := cloneTodoItem(todoItemByID(source.Session.Tasks, id))
	ctx := contractCollectionContext(t, id)
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := c.ApplyContractRecovery(ctx, source, "evidence-repair", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	child, err := ReadResumeSource(c.session.Workspace, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	c = reopenContractCollection(t, c, child)
	a, err := c.AssessResultCollectionRecovery(t.Context(), id)
	if err != nil || a.Disposition != RecoveryCommitLocal {
		t.Fatalf("original observed keys lost after upgrade/restart: %+v %v", a, err)
	}
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
		t.Fatal(err)
	}
	cp := currentResultCollection(c.todoItemByID(id))
	repairCtx, err := c.restoreCollectionContext(WithSessionResume(t.Context()), cp)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.readCollectionBlob(t.Context(), cp, "response", cp.Response)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := c.resolveResultEvidenceKeys(repairCtx, id, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	ref := original.ExecutionReceipt.ArtifactScope.AuthorizedRefs[0]
	value, err := decodeUniqueJSON([]byte(resolved))
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range value.(map[string]any)["structured_payload"].(map[string]any)["findings"].([]any) {
		if finding != ref.ID {
			t.Fatal("original evidence changed")
		}
	}
	for _, scenario := range []string{"unknown_key", "unauthorized_source", "changed_proof", "changed_schema", "changed_branch", "changed_model", "changed_attempt", "without_collection", "corrupt_source"} {
		t.Run(scenario, func(t *testing.T) {
			candidate, input := repairCtx, string(raw)
			before := c.taskTracker.TodoList().Items()
			items := c.taskTracker.TodoList().Items()
			modified := todoItemByID(items, id)
			defer c.taskTracker.TodoList().Restore(before)
			switch scenario {
			case "unknown_key":
				start := strings.Index(input, evidenceKeyPrefix)
				input = strings.ReplaceAll(input, input[start:start+15], "ev_000000000000")
			case "unauthorized_source":
				scope, _ := artifactAccessScopeFromContext(candidate)
				copy := cloneArtifactAccessScope(scope)
				copy.AuthorizedRefs = nil
				candidate = context.WithValue(candidate, artifactAccessScopeKey, copy)
			case "changed_proof":
				modified.ExecutionReceipt.ResultCollection.ContractUpgrade.SourceTaskSHA256 = strings.Repeat("a", 64)
			case "changed_schema":
				modified.ExecutionReceipt.ResultCollection.ContractUpgrade.OriginalContract.SchemaSHA256 = strings.Repeat("a", 64)
			case "changed_branch":
				modified.ExecutionReceipt.ResultCollection.ContractUpgrade.Original.BranchID = "unrelated-branch"
			case "changed_model":
				metadata, _ := invocationMetadataFromContext(candidate)
				metadata.ModelExecutionID = "another-work-execution"
				candidate = withInvocationMetadata(candidate, metadata)
			case "changed_attempt":
				metadata, _ := invocationMetadataFromContext(candidate)
				metadata.Attempt++
				candidate = withInvocationMetadata(candidate, metadata)
			case "without_collection":
				candidate = context.WithValue(candidate, resultCollectionEvidenceKey{}, (*taskTranscript)(nil))
			case "corrupt_source":
				path := filepath.Join(c.session.Workspace, logsDir, "artifacts", "data", ref.ID)
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.WriteFile(path, before, 0o600); err != nil {
						t.Fatal(err)
					}
				}()
			}
			c.taskTracker.TodoList().Restore(items)
			if _, err := c.resolveResultEvidenceKeys(candidate, id, input); err == nil {
				t.Fatal("invalid recovery evidence admitted")
			}
		})
	}
	worker := scriptWorker(textTurn("original work must not replay"))
	c.workerAgentOverride = worker
	if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id); err != nil {
		t.Fatal(err)
	}
	done := c.todoItemByID(id)
	if worker.Calls() != 0 || done.Status != TaskDone || currentResultCollection(done).TurnsUsed != cp.TurnsUsed || done.VerifyResult == nil || done.VerifyResult.ExitCode != 0 {
		t.Fatal("recovery replayed work, spent collector turns or skipped verification")
	}
	if len(done.TypedResult.FilesRead) != 1 || done.TypedResult.FilesRead[0].Path != ref.ID {
		t.Fatal("runtime-owned read projection lost the original observation")
	}
	if currentResultCollection(done).Work != original.ExecutionReceipt.ResultCollection.Work ||
		done.ExecutionReceipt.RunID != original.ExecutionReceipt.RunID || done.ExecutionReceipt.TranscriptRef != original.ExecutionReceipt.TranscriptRef ||
		done.ExecutionReceipt.ModelExecutionID != original.ExecutionReceipt.ModelExecutionID || done.RunInputSnapshotHash != original.RunInputSnapshotHash {
		t.Fatal("recovery replaced the original work identity")
	}
	stored, err := decodeUniqueJSON(done.TypedResult.StructuredPayload.Value)
	if err != nil || !reflect.DeepEqual(stored, value.(map[string]any)["structured_payload"]) {
		t.Fatalf("canonical evidence changed: %v", err)
	}
	parent, err := ReadResumeSource(c.session.Workspace, source.BranchID, "")
	if err != nil {
		t.Fatalf("parent changed: %v", err)
	}
	if err := compareSingleTaskProjection(parent.Session.Tasks[0], original); err != nil {
		t.Fatalf("parent changed: %v", err)
	}
}
