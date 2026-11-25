package team

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/execution"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

func TestRuntimeMediaRevalidatesNormalizedResultAfterRestart(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		prefinal bool
		corrupt  bool
	}{
		{name: "verified_media"}, {name: "corrupt_media", corrupt: true},
		{name: "acknowledged_media", prefinal: true}, {name: "acknowledged_corrupt_media", prefinal: true, corrupt: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c := newMediaPolicyCoordinator(t)
			openMediaInvocation(t, c)
			_, tools, authorization := gatewayProjectionFixture(t, 1)
			item := admitDirectNormalizationTask(t, c, echoNormalization, func(spec *TodoSpec) {
				spec.Verify = "true"
				spec.DynamicToolAuthorization = authorization
			})
			worker, _ := normalizationStreamedSubmission(c, item.ID, normalizationWire(approvedNormalizationDraft), nil)
			var projection *RuntimeMediaProjection
			c.workerAgentOverride = scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
				var err error
				projection, err = publishNormalizationFixtureImage(t, c, ctx, tools[0])
				if err != nil {
					t.Errorf("cannot publish fixture image: %v", err)
					return nil, err
				}
				result, err := worker.Stream(ctx, call)
				if scenario.prefinal && err == nil && c.GetTaskResult(item.ID) != nil {
					panic(interruptedAfterNormalizedSubmission{})
				}
				return result, err
			}})
			if scenario.prefinal {
				interruptAfterNormalizedSubmission(t, c, item)
				if result := c.GetTaskResult(item.ID); result.RuntimeMedia != nil {
					t.Fatal("fixture already attached media before worker acknowledgment")
				}
			} else {
				stopBeforeObjectiveVerification(t, c, item)
			}
			if projection == nil || len(projection.artifacts()) != 1 {
				t.Fatal("no genuine published image in the normalized result")
			}
			snapshot, checkpoint := c.RunInputSnapshot(), normalizationResumeProjection(t, c)
			reader := reopenRecoveryCoordinator(t, c)
			reader.SetSessionData(checkpoint)
			if err := reader.selectRecoveryRunInputSnapshot(snapshot); err != nil {
				t.Fatal(err)
			}
			forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
			reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
			if scenario.corrupt {
				corruptNormalizationFixtureImage(t, reader, projection)
			}
			count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			got := todoByID(t, reader, item.ID)
			if count != 1 || forbidden.Calls() != 0 {
				t.Fatalf("normalized media did not resume its original dispatch: count=%d calls=%d error=%v", count, forbidden.Calls(), err)
			}
			if scenario.corrupt {
				if localResultRejectionReason(err) != RepairFailureSource || got.Status != TaskBlocked || got.VerifyResult != nil {
					t.Fatalf("corrupt media admitted verification: status=%s error=%v", got.Status, err)
				}
			} else if err != nil || got.Status != TaskDone || got.VerifyResult == nil ||
				got.ExecutionReceipt.RuntimeMediaHash != projection.Hash || !sameCanonicalEvidence(projection, got.TypedResult.RuntimeMedia) {
				t.Fatalf("canonical media did not settle through verification: status=%s error=%v", got.Status, err)
			}
		})
	}
}

func TestRuntimeMediaValidatesLiveNormalizedResult(t *testing.T) {
	for _, scenario := range []struct {
		name string
		want RepairFailureReason
	}{
		{name: "intact"},
		{name: "corrupt", want: RepairFailureSource},
		{name: "transient", want: RepairFailureCommit},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c := newMediaPolicyCoordinator(t)
			openMediaInvocation(t, c)
			_, tools, authorization := gatewayProjectionFixture(t, 1)
			var normalizations int
			item := admitDirectNormalizationTask(t, c, func(_ context.Context, draft []byte) ([]byte, error) {
				normalizations++
				return draft, nil
			}, func(spec *TodoSpec) {
				spec.Verify = "true"
				spec.DynamicToolAuthorization = authorization
			})
			wire := normalizationWire(approvedNormalizationDraft)
			if scenario.want == RepairFailureCommit {
				const path = "observation.txt"
				if err := os.WriteFile(filepath.Join(c.session.Workspace, path), []byte("original observation"), 0o600); err != nil {
					t.Fatal(err)
				}
				wire = normalizedWireWithArtifact(approvedNormalizationDraft, path)
			}
			worker, model := normalizationStreamedSubmission(c, item.ID, wire, nil)
			var restore func()
			var projection *RuntimeMediaProjection
			c.workerAgentOverride = scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
				var err error
				projection, err = publishNormalizationFixtureImage(t, c, ctx, tools[0])
				if err != nil || projection == nil {
					t.Fatalf("publish fixture image: %v", err)
				}
				result, err := worker.Stream(ctx, call)
				if err != nil || c.GetTaskResult(item.ID) == nil {
					t.Fatalf("original normalized submission was not accepted: %v", err)
				}
				switch scenario.want {
				case RepairFailureSource:
					corruptNormalizationFixtureImage(t, c, projection)
				case RepairFailureCommit:
					refs := c.GetTaskResult(item.ID).Artifacts
					if len(refs) != 1 {
						t.Fatalf("missing original observation: %v", refs)
					}
					restore = faultNormalizedEvidencePath(t, normalizedArtifactData(c, refs[0].ID), true)
				}
				return result, nil
			}})
			ctx := withTestProtocolRepairInvocationContext(t.Context())
			_, err := c.executeTask(ctx, taskDefFromTodoItem(item), item.ID)
			c.terminalizeTaskErrorIfUnresolved(item.ID, err)
			got := todoByID(t, c, item.ID)
			if scenario.want != "" {
				if localResultRejectionReason(err) != scenario.want || got.VerifyResult != nil {
					t.Fatalf("unreadable live normalized media reached verifier: status=%s error=%v", got.Status, err)
				}
				if scenario.want == RepairFailureSource {
					if got.Status != TaskBlocked || got.ExecutionReceipt.RepairProvenance.FailureReason != scenario.want {
						t.Fatalf("corrupt live media was not durably blocked: %+v", got)
					}
					return
				}
				if !isInterruptedStatus(got.Status) {
					t.Fatalf("transient fault lost continuation: %s", got.Status)
				}
				restore()
				snapshot, checkpoint := c.RunInputSnapshot(), normalizationResumeProjection(t, c)
				c = reopenRecoveryCoordinator(t, c)
				c.SetSessionData(checkpoint)
				if err := c.selectRecoveryRunInputSnapshot(snapshot); err != nil {
					t.Fatal(err)
				}
				forbidden := scriptWorker(failTurn(errors.New("unexpected worker or correction call")))
				c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
				count, resumeErr := c.ResumeInterruptedTasks(WithSessionResume(t.Context()))
				if count != 1 || resumeErr != nil || forbidden.Calls() != 0 {
					t.Fatalf("restored media did not resume original work: count=%d error=%v", count, resumeErr)
				}
				got, err = todoByID(t, c, item.ID), nil
			}
			if err != nil || got.Status != TaskDone || got.VerifyResult == nil || normalizations != 1 || model.calls != 1 ||
				got.ExecutionReceipt.RuntimeMediaHash != projection.Hash || !sameCanonicalEvidence(projection, got.TypedResult.RuntimeMedia) {
				t.Fatalf("live completion lost canonical media: status=%s error=%v", got.Status, err)
			}
		})
	}
}

func publishNormalizationFixtureImage(t *testing.T, c *Coordinator, ctx context.Context, tool internalmcp.MCPTool) (*RuntimeMediaProjection, error) {
	t.Helper()
	_, err := c.publishToolMediaResult(ctx, tool, fantasy.ToolCall{ID: "capture"}, internalmcp.RuntimeToolResult{
		InvocationID: "mcp-normalization", Content: []internalmcp.RuntimeToolContent{
			{Type: "image", Data: base64.StdEncoding.EncodeToString(mediaStorePNG(t)), MIMEType: "image/png"},
		},
	})
	if err != nil {
		return nil, err
	}
	identity, _, err := c.toolMediaOccurrence(ctx)
	if err != nil {
		return nil, err
	}
	return c.runtimeMediaForOccurrence(ctx, identity, "")
}

func corruptNormalizationFixtureImage(t *testing.T, c *Coordinator, projection *RuntimeMediaProjection) {
	t.Helper()
	workspace := c.artifactStoreRootPath()
	store, err := OpenFileArtifactStoreReadOnly(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.mediaPath("data", projection.artifacts()[0].ID), []byte("corrupt image bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeMediaRebuildsAcceptedNormalizedCollection(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "published_media"
		if corrupt {
			name = "corrupt_media"
		}
		t.Run(name, func(t *testing.T) {
			c := newMediaPolicyCoordinator(t)
			ctx := openMediaInvocation(t, c)
			_, tools, authorization := gatewayProjectionFixture(t, 1)
			var normalizations int
			item := admitDirectNormalizationTask(t, c, func(_ context.Context, draft []byte) ([]byte, error) {
				normalizations++
				return draft, nil
			}, func(spec *TodoSpec) {
				spec.Verify = "true"
				spec.DynamicToolAuthorization = authorization
				target := execution.ExecutionTarget{Backend: "fixture", Model: "vision"}
				spec.Model, spec.ExecutionTarget, spec.ExecutionTopology = "fixture/vision", target, []execution.ExecutionTarget{target}
			})
			if err := c.CommitTaskTransition(ctx, item.ID, TaskPending, TaskInProgress, "working", "", nil); err != nil {
				t.Fatal(err)
			}
			c.setCurrentTaskAttempt(item.ID, 1)
			identity, ok := c.activeTaskResultOccurrence(item.ID)
			if !ok {
				t.Fatal("missing original worker lease")
			}
			ctx = withSubmitResultRuntimeIdentity(ctx, identity)
			ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: identity.RunID, TaskID: item.ID, AgentName: "worker", AgentRole: "worker", Attempt: 1, Purpose: "task_execution"})
			projection, err := publishNormalizationFixtureImage(t, c, ctx, tools[0])
			if err != nil || projection == nil {
				t.Fatalf("publish collection work media: %v", err)
			}
			transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, identity.RunID, 1, item.Agent)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = transcript.Close() })
			if err := transcript.RecordAssistantOutput("original collection work report"); err != nil {
				t.Fatal(err)
			}
			ref, err := sealVerbatimTaskTranscript(transcript)
			if err != nil {
				t.Fatal(err)
			}
			receipt := ExecutionReceipt{RunID: identity.RunID, Attempt: 1, OccurrenceAttempt: 1, TaskID: item.ID, ProducerID: item.Agent,
				ExitCode: new(0), Model: item.Model, ExecutionTarget: item.ExecutionTarget, ModelExecutionID: "original-context-model-execution",
				TranscriptRef: ref.ID, RunInputSnapshotID: item.RunInputSnapshotID, RunInputSnapshotHash: item.RunInputSnapshotHash}
			cp, err := c.beginResultCollection(ctx, item.ID, receipt, "original collection work report")
			if err != nil {
				t.Fatal(err)
			}
			commitNormalizedCollectionFixture(t, c, ctx, cp, approvedNormalizationDraft)
			if got := todoByID(t, c, item.ID); !isCommittedNormalizedCollection(got) || got.TypedResult.RuntimeMedia != nil || normalizations != 1 {
				t.Fatal("fixture did not interrupt after collection acceptance and before media attachment")
			}
			snapshot, checkpoint := c.RunInputSnapshot(), normalizationResumeProjection(t, c)
			reader := reopenRecoveryCoordinator(t, c)
			reader.SetSessionData(checkpoint)
			if err := reader.selectRecoveryRunInputSnapshot(snapshot); err != nil {
				t.Fatal(err)
			}
			forbidden := scriptWorker(failTurn(errors.New("accepted collection dispatched a worker or repair")))
			reader.workerAgentOverride, reader.repairAgentOverride = forbidden, forbidden
			if corrupt {
				corruptNormalizationFixtureImage(t, reader, projection)
			}
			count, err := reader.ResumeInterruptedTasks(WithSessionResume(t.Context()))
			got := todoByID(t, reader, item.ID)
			if count != 1 || normalizations != 1 || forbidden.Calls() != 0 {
				t.Fatalf("accepted media collection was repeated: count=%d normalizations=%d error=%v", count, normalizations, err)
			}
			if corrupt {
				if localResultRejectionReason(err) != RepairFailureSource || got.Status != TaskBlocked || got.VerifyResult != nil {
					t.Fatalf("corrupt original media reached a verifier: status=%s error=%v", got.Status, err)
				}
			} else if err != nil || got.Status != TaskDone || got.VerifyResult == nil ||
				got.ExecutionReceipt.RuntimeMediaHash != projection.Hash || !sameCanonicalEvidence(projection, got.TypedResult.RuntimeMedia) {
				t.Fatalf("collection did not publish original media: status=%s error=%v", got.Status, err)
			}
		})
	}
}

func TestRuntimeMediaResultRemainsStableAcrossPersistenceRedaction(t *testing.T) {
	c, ctx, tool := toolMediaPublicationFixture(t, true)
	const caption = "canonical-media-caption-8bf3902a"
	_, err := c.publishToolMediaResult(ctx, tool, fantasy.ToolCall{ID: "capture"}, internalmcp.RuntimeToolResult{
		InvocationID: "mcp-redaction", Content: []internalmcp.RuntimeToolContent{
			{Type: "text", Text: caption},
			{Type: "image", Data: base64.StdEncoding.EncodeToString(mediaStorePNG(t)), MIMEType: "image/png"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, _, err := c.toolMediaOccurrence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := c.runtimeMediaForOccurrence(ctx, identity, "")
	if err != nil || projection == nil {
		t.Fatal("missing canonical media projection", err)
	}
	result := &TaskResult{TaskID: identity.TaskID, Agent: identity.Agent, Attempt: identity.Attempt, Status: TaskResultStatusSuccess,
		RuntimeMedia: projection, Facts: map[string]any{"ordinary": caption,
			"imitation": map[string]any{"task_id": identity.TaskID, "agent": identity.Agent, "runtime_media": projection}}}
	encoded, err := json.Marshal(map[string]any{"typed_result": result})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewSecretRegistry()
	if err := registry.Register(tools.SecretRef{Name: "test.media.caption", Source: "test", ExactValue: caption}); err != nil {
		t.Fatal(err)
	}
	utils.RegisterSecretRedactor(registry)
	redacted, err := redactJSONPreservingRuntimeOutputs(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var restored struct {
		Result *TaskResult `json:"typed_result"`
	}
	if err := json.Unmarshal(redacted, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Result == nil || !reflect.DeepEqual(restored.Result.RuntimeMedia, projection) || restored.Result.RuntimeMedia.Validate() != nil {
		t.Fatal("persistence changed receipt-bound media bytes")
	}
	if restored.Result.Facts["ordinary"] == caption {
		t.Fatal("ordinary model facts escaped redaction")
	}
	imitation, err := json.Marshal(restored.Result.Facts["imitation"])
	if err != nil || string(imitation) == "" || reflect.DeepEqual(restored.Result.Facts["imitation"], result.Facts["imitation"]) {
		t.Fatal("model-authored imitation escaped redaction")
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	historical, _, err := ProjectHistoricalTaskResult(encodedResult)
	if err != nil {
		t.Fatal(err)
	}
	var projected TaskResult
	if err := json.Unmarshal(historical, &projected); err != nil || !reflect.DeepEqual(projected.RuntimeMedia, projection) || projected.Facts["ordinary"] == caption {
		t.Fatal("historical result projection changed canonical media or exposed model facts", err)
	}
}
