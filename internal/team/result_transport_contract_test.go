package team

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func TestResultTransportDecodersRejectAmbiguousJSON(t *testing.T) {
	c := newTestCoordinator(t)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "report", Execution: ExecutionContract{RequiresResult: true}}})[0]
	decoders := map[string]func([]byte) error{
		"tool": func(raw []byte) error {
			_, err := decodeSubmitResultInput(raw, localResultSubmissionContract(c, item.ID))
			return err
		},
		"collector": func(raw []byte) error {
			_, err := c.decodeCollectionClaims(item.ID, raw)
			return err
		},
		"external": func(raw []byte) error {
			_, err := DecodeWorkerResultProposal(raw)
			return err
		},
	}
	for _, test := range []struct {
		name  string
		raw   string
		valid bool
	}{
		{"valid", `{"status":"success","summary":"accepted"}`, true},
		{"whitespace suffix", "{\"status\":\"success\",\"summary\":\"accepted\"}\n\t ", true},
		{"duplicate status", `{"status":"failed","status":"success","summary":"ambiguous"}`, false},
		{"nested duplicate", `{"status":"success","summary":"ambiguous","facts":{"flag":false,"flag":true}}`, false},
		{"trailing object", `{"status":"success","summary":"ambiguous"} {"status":"failed"}`, false},
		{"trailing junk", `{"status":"success","summary":"ambiguous"} garbage`, false},
		{"runtime identity", `{"status":"success","summary":"ambiguous","task_id":"forged"}`, false},
	} {
		for transport, decode := range decoders {
			t.Run(test.name+"/"+transport, func(t *testing.T) {
				if err := decode([]byte(test.raw)); (err == nil) != test.valid {
					t.Fatalf("valid=%t, decoder error=%v", test.valid, err)
				}
			})
		}
	}
}

type transportCommonClaims struct {
	Status        string
	Summary       string
	Details       string
	Findings      []Finding
	Risks         []Risk
	OpenQuestions []string
	Facts         map[string]any
	Confidence    float64
}

func transportClaims(result *TaskResult) transportCommonClaims {
	return transportCommonClaims{Status: result.Status, Summary: result.Summary, Details: result.Details,
		Findings: result.Findings, Risks: result.Risks, OpenQuestions: result.OpenQuestions,
		Facts: result.Facts, Confidence: result.Confidence}
}

const transportArtifactContent = "native file evidence\n"

// Exercise each adapter at its real admission boundary. External proposals
// are canonicalized before storage; local adapters commit an occurrence.
// None of these boundaries owns task completion.
func admitTransportContract(t *testing.T, transport string, raw []byte, artifact bool) (*TaskResult, TaskStatus, error) {
	t.Helper()
	c, item, ctx, receipt := resultCollectionProviderFixture(t, "http://127.0.0.1:1/v1", transport == "collector", func(c *Coordinator) {
		if transport != "collector" {
			c.session.Agents["worker"].ResultTransport = agent.ResultTransportTool
		}
	})
	worker := scriptWorker(textTurn("admission must not execute work"))
	c.workerAgentOverride = worker
	t.Cleanup(func() {
		if worker.Calls() != 0 {
			t.Error("result admission executed worker work")
		}
	})
	var delta WorkspaceDelta
	if artifact {
		snapshotter := NewWorkspaceSnapshotter()
		before, err := snapshotter.Snapshot(t.Context(), c.session.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(c.session.Workspace, "report.txt"), []byte(transportArtifactContent), 0o600); err != nil {
			t.Fatal(err)
		}
		after, err := snapshotter.Snapshot(t.Context(), c.session.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		delta, err = snapshotter.Diff(t.Context(), before, after)
		if err != nil {
			t.Fatal(err)
		}
	}
	var result *TaskResult
	var err error
	switch transport {
	case "tool":
		response, submitErr := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(raw)})
		err = submitErr
		if err == nil && response.IsError {
			err = fmt.Errorf("tool rejected result: %s", response.Content)
		}
	case "collector":
		var checkpoint *ResultCollectionCheckpoint
		checkpoint, err = c.beginResultCollection(ctx, item.ID, receipt, "complete original work report")
		if err == nil {
			checkpoint, err = c.reserveResultCollection(ctx, checkpoint, "collector-contract", []byte(`{"tools":[]}`))
		}
		if err == nil {
			checkpoint, err = c.recordCollectionResponse(ctx, checkpoint, raw)
		}
		if err == nil {
			checkpoint, err = c.prepareCollectionClaims(ctx, checkpoint)
		}
		if err == nil {
			err = c.commitPreparedCollection(ctx, checkpoint)
		}
	case "external":
		var proposal *WorkerResultProposal
		proposal, err = DecodeWorkerResultProposal(raw)
		if err == nil {
			request := scopedAttemptRequest("external", item.ID)
			request.Task = taskDefFromTodoItem(item)
			result, err = NewExternalResultCanonicalizer().Canonicalize(ctx, request, AttemptResult{ResultProposal: proposal}, delta, c.session.Workspace)
		}
	default:
		t.Fatalf("unknown fixture transport %s", transport)
	}
	if transport != "external" {
		result = c.GetTaskResult(item.ID)
	}
	if err != nil && result != nil {
		t.Fatal("rejected result produced a canonical handoff")
	}
	return result, c.todoItemByID(item.ID).Status, err
}

func TestResultTransportCommonClaimsAndCompletionBoundary(t *testing.T) {
	for _, status := range []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked} {
		t.Run(status, func(t *testing.T) {
			var expected *transportCommonClaims
			for _, transport := range []string{"tool", "collector", "external"} {
				t.Run(transport, func(t *testing.T) {
					raw, err := json.Marshal(map[string]any{"status": status, "summary": "common report", "details": "complete details", "confidence": 1,
						"findings": []Finding{{Severity: "warning", Category: "correctness", Summary: "finding", Detail: "detail"}}, "risks": []Risk{{Description: "risk"}}, "open_questions": []string{"question"}, "facts": map[string]any{"count": 2}})
					if err != nil {
						t.Fatal(err)
					}
					result, taskStatus, err := admitTransportContract(t, transport, raw, false)
					if err != nil || result == nil {
						t.Fatalf("common claims rejected: result=%+v error=%v", result, err)
					}
					if taskStatus != TaskInProgress {
						t.Fatalf("admission bypassed completion: %s", taskStatus)
					}
					if result.TaskID == "" || result.Attempt != 1 || result.Agent != "worker" {
						t.Fatalf("runtime identity missing: %+v", result)
					}
					wantSource := map[string]string{"tool": "submitted", "collector": "collected", "external": ExternalProviderProposalSource}[transport]
					if result.Source != wantSource {
						t.Fatalf("source=%s, want %s", result.Source, wantSource)
					}
					view := transportClaims(result)
					if expected == nil {
						expected = new(view)
					} else if !reflect.DeepEqual(*expected, view) {
						t.Fatalf("common claims changed across transports: want=%+v actual=%+v", *expected, view)
					}
				})
			}
		})
	}
}

func TestResultTransportRejectsInvalidClaimsWithoutSuccess(t *testing.T) {
	for _, test := range []struct{ name, raw string }{
		{"unknown status", `{"status":"unknown","summary":"invalid status"}`},
		{"empty summary", `{"status":"success","summary":""}`},
		{"runtime identity", `{"status":"success","summary":"forged identity","task_id":"forged"}`},
		{"forged receipt", `{"status":"success","summary":"forged receipt","receipt_ids":["forged"]}`},
	} {
		for _, transport := range []string{"tool", "collector", "external"} {
			t.Run(test.name+"/"+transport, func(t *testing.T) {
				result, taskStatus, err := admitTransportContract(t, transport, []byte(test.raw), false)
				if err == nil || result != nil || taskStatus == TaskDone {
					t.Fatalf("invalid claims produced success: result=%+v status=%s error=%v", result, taskStatus, err)
				}
			})
		}
	}
}

func TestResultTransportArtifactHashesComeFromNativeBytes(t *testing.T) {
	wantSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(transportArtifactContent)))
	for _, transport := range []string{"tool", "collector", "external"} {
		t.Run(transport, func(t *testing.T) {
			input := map[string]any{"status": "success", "summary": "native artifact"}
			field := "artifacts"
			if transport == "external" {
				field = "proposed_files"
			}
			input[field] = []map[string]string{{"path": "report.txt"}}
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			result, taskStatus, err := admitTransportContract(t, transport, raw, true)
			if err != nil || result == nil || len(result.Artifacts) != 1 {
				t.Fatalf("native artifact rejected: result=%+v error=%v", result, err)
			}
			if taskStatus == TaskDone || result.Artifacts[0].SHA256 != wantSHA || result.Artifacts[0].Bytes != int64(len(transportArtifactContent)) {
				t.Fatalf("artifact hashes did not bind native bytes: %+v", result.Artifacts)
			}
		})
	}
}
