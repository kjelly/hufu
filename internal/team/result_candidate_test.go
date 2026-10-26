package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

const candidateTestSchema = `{"type":"object","additionalProperties":false,"x-hufu-submission-fields":["status","summary","structured_payload"],"required":["findings","summary"],"properties":{"summary":{"type":"string"},"findings":{"type":"array","minItems":1,"items":{"type":"object","additionalProperties":false,"required":["claim","verdict"],"properties":{"claim":{"type":"string"},"verdict":{"type":"string","enum":["supported","unverified"]}}}}}}`
const candidateMisplaced = `{"status":"success","summary":"audit","findings":[{"claim":"preserve the actual finding","verdict":"unverified"}],"structured_payload":{"summary":"review complete"}}`
const candidateMove = `{"edits":[{"operation":"move","from":"/findings","path":"/structured_payload/findings"}]}`

func resultCandidateFixture(t *testing.T) (*Coordinator, *TodoItem, context.Context, *uniqueRecordingJournal) {
	t.Helper()
	c := newTestCoordinator(t)
	compiled, err := compileResultContractDocument("candidate.json", []byte(candidateTestSchema))
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	ref := compiled.ref(true)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "audit", Goal: "audit", ResultContract: &ref}})[0]
	journal := &uniqueRecordingJournal{}
	c.SetEventJournal(journal)
	if err := c.CommitTaskTransition(t.Context(), item.ID, TaskPending, TaskInProgress, "working", "", nil); err != nil {
		t.Fatal(err)
	}
	return c, item, occurrenceTestContext(c, item.ID, 1), journal
}

func rejectCandidate(t *testing.T, c *Coordinator, item *TodoItem, ctx context.Context, raw string) *ResultCandidate {
	t.Helper()
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: raw})
	if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil {
		t.Fatalf("rejection: %+v %v", response, err)
	}
	candidate := c.todoItemByID(item.ID).ResultCandidate
	if candidate == nil {
		t.Fatalf("candidate not persisted: %s", response.Content)
	}
	return cloneResultCandidate(candidate)
}

func TestResultCandidateRepairPreservesContentAndReplays(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_process", true: "crash_resume"}[resume], func(t *testing.T) {
			c, item, ctx, journal := resultCandidateFixture(t)
			candidate := rejectCandidate(t, c, item, ctx, candidateMisplaced)
			if resume {
				// Rebuild exclusively from durable events, not the live checkpoint.
				replayed := ReduceToTodoList(journal.events)
				if len(replayed) != 1 || replayed[0].ResultCandidate == nil || replayed[0].TypedResult != nil {
					t.Fatalf("bad candidate replay: %+v", replayed)
				}
				next := newTestCoordinator(t)
				next.session.Workspace = c.session.Workspace
				next.session.ResultContracts = c.session.ResultContracts
				next.SetSessionData(&SessionData{Tasks: replayed})
				c = next
				item = replayed[0]
				ctx = occurrenceTestContext(c, item.ID, 1)
			}
			tool := &submitResultTool{coordinator: c, todoID: item.ID, candidate: candidate}
			if !reflect.DeepEqual(tool.Info().Required, []string{"edits"}) {
				t.Fatal("repair still requires full report")
			}
			response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: candidateMove})
			if err != nil || response.IsError {
				t.Fatalf("repair failed: %s %v", response.Content, err)
			}
			result := c.GetTaskResult(item.ID)
			if result == nil || !strings.Contains(string(result.StructuredPayload.Value), "preserve the actual finding") || !strings.Contains(string(result.StructuredPayload.Value), "unverified") {
				t.Fatalf("result lost content: %+v", result)
			}
			if c.todoItemByID(item.ID).Status == TaskDone {
				t.Fatal("tool bypassed lifecycle verification")
			}
		})
	}
}

func TestResultCandidateRejectsUnsafeEdits(t *testing.T) {
	for name, patch := range map[string]string{
		"remove":            `{"edits":[{"operation":"remove","path":"/findings"}]}`,
		"replace_valid":     `{"edits":[{"operation":"replace","path":"/summary","value":"different"}]}`,
		"overwrite":         `{"edits":[{"operation":"move","from":"/findings","path":"/structured_payload/summary"}]}`,
		"runtime_field":     `{"edits":[{"operation":"move","from":"/findings","path":"/task_id"}]}`,
		"full_resubmission": candidateMisplaced,
		"duplicate_keys":    `{"edits":[],"edits":[{"operation":"move","from":"/findings","path":"/structured_payload/findings"}]}`,
		"overlapping_edits": `{"edits":[{"operation":"move","from":"/findings","path":"/structured_payload/findings"},{"operation":"add","path":"/structured_payload/findings","value":[]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, item, ctx, _ := resultCandidateFixture(t)
			candidate := rejectCandidate(t, c, item, ctx, candidateMisplaced)
			response, err := (&submitResultTool{coordinator: c, todoID: item.ID, candidate: candidate}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: patch})
			if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil {
				t.Fatalf("unsafe edit accepted: %+v %v", response, err)
			}
			if c.todoItemByID(item.ID).ResultCandidate.SHA256 != candidate.SHA256 {
				t.Fatal("unsafe edit mutated original draft")
			}
		})
	}
}

func TestCandidateMovePreservesNumbersAndValidFields(t *testing.T) {
	raw := `{"misplaced":{"count":9007199254740993,"fraction":1.234567890123456789},"structured_payload":{},"summary":"unchanged"}`
	value, err := decodeResultContractJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	document := value.(map[string]any)
	schema := map[string]any{"properties": map[string]any{"structured_payload": map[string]any{"properties": map[string]any{"misplaced": map[string]any{"type": "object"}}}}}
	err = applyResultCandidateEdit(document, resultCandidateEdit{Operation: "move", From: "/misplaced", Path: "/structured_payload/misplaced"}, []ResultSubmissionIssue{{Code: "additional_property", Pointer: "/misplaced"}}, schema)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(document, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"count":9007199254740993`, `"fraction":1.234567890123456789`, `"summary":"unchanged"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("move changed data: %s", data)
		}
	}
}

func TestResultCandidateRejectsStaleBindings(t *testing.T) {
	for _, change := range []string{"branch", "inputs", "contract", "attempt", "content", "newer_candidate", "stale_lease"} {
		t.Run(change, func(t *testing.T) {
			c, item, ctx, _ := resultCandidateFixture(t)
			candidate := rejectCandidate(t, c, item, ctx, candidateMisplaced)
			switch change {
			case "branch":
				candidate.BranchID = "other"
			case "inputs":
				candidate.InputsHash = "other"
			case "contract":
				candidate.ContractHash = "other"
			case "attempt":
				candidate.Attempt++
			case "content":
				store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(store.root, "data", candidate.ArtifactID), []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "newer_candidate":
				rejectCandidate(t, c, item, ctx, candidateMisplaced)
			case "stale_lease":
				c.setCurrentTaskAttempt(item.ID, 2)
			}
			response, err := (&submitResultTool{coordinator: c, todoID: item.ID, candidate: candidate}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: candidateMove})
			if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil {
				t.Fatalf("stale candidate accepted: %+v %v", response, err)
			}
		})
	}
}

func TestResultCandidateAppendFailureLeavesProjectionUntouched(t *testing.T) {
	c, item, ctx, _ := resultCandidateFixture(t)
	c.SetEventJournal(failingJournal{err: errors.New("disk unavailable")})
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: candidateMisplaced})
	if err != nil || !response.IsError || c.todoItemByID(item.ID).ResultCandidate != nil || c.GetTaskResult(item.ID) != nil {
		t.Fatal("failed append published a candidate/result")
	}
	if c.occurrenceController(item.ID).snapshot().reserved {
		t.Fatal("failed append leaked reservation")
	}
}

func TestResultCandidatePartialCorrectionCreatesVersion(t *testing.T) {
	c, item, ctx, _ := resultCandidateFixture(t)
	raw := strings.Replace(candidateMisplaced, `"unverified"`, `"unknown"`, 1)
	candidate := rejectCandidate(t, c, item, ctx, raw)
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID, candidate: candidate}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: candidateMove})
	if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil {
		t.Fatalf("bad partial patch: %+v %v", response, err)
	}
	next := c.todoItemByID(item.ID).ResultCandidate
	if next.Version != 2 || next.ParentSHA256 != candidate.SHA256 {
		t.Fatal("candidate version chain missing")
	}
	patch := `{"edits":[{"operation":"replace","path":"/structured_payload/findings/0/verdict","value":"unverified"}]}`
	response, err = (&submitResultTool{coordinator: c, todoID: item.ID, candidate: cloneResultCandidate(next)}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: patch})
	if err != nil || response.IsError {
		t.Fatalf("second patch: %s %v", response.Content, err)
	}
}

func TestResultSubmissionFieldsAndDiagnostics(t *testing.T) {
	c, item, _, _ := resultCandidateFixture(t)
	info := (&submitResultTool{coordinator: c, todoID: item.ID}).Info()
	if len(info.Parameters) != 3 || info.Parameters["findings"] != nil {
		t.Fatal("unneeded outer fields still advertised")
	}
	issues, truncated, err := resultSubmissionIssues(candidateMisplaced, info)
	if err != nil || truncated || len(issues) != 2 {
		t.Fatalf("diagnostics: %+v %t %v", issues, truncated, err)
	}
	data, _ := json.Marshal(issues)
	for _, want := range []string{"additional_property", "missing_property", "/findings", "/structured_payload/findings"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing diagnostic %s: %s", want, data)
		}
	}
	for _, fields := range []string{`[]`, `["status","summary","structured_payload","task_id"]`, `["status","summary","structured_payload","summary"]`} {
		raw := strings.Replace(candidateTestSchema, `["status","summary","structured_payload"]`, fields, 1)
		if _, err := compileResultContractDocument("bad.json", []byte(raw)); err == nil {
			t.Fatal("invalid submission projection accepted")
		}
	}
}

func TestCandidateConcurrentSubmissionAcceptsOnce(t *testing.T) {
	c, item, ctx, _ := resultCandidateFixture(t)
	candidate := rejectCandidate(t, c, item, ctx, candidateMisplaced)
	responses := make(chan fantasy.ToolResponse, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Go(func() {
			response, err := (&submitResultTool{coordinator: c, todoID: item.ID, candidate: cloneResultCandidate(candidate)}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: candidateMove})
			if err != nil {
				t.Error(err)
			}
			responses <- response
		})
	}
	group.Wait()
	close(responses)
	successes := 0
	for response := range responses {
		if !response.IsError {
			successes++
		}
	}
	if successes != 1 || c.GetTaskResult(item.ID) == nil {
		t.Fatalf("accepted %d concurrent submissions", successes)
	}
}

func TestCandidateDiagnosticsRetainLargeCollectionMembership(t *testing.T) {
	c, _, item, plans := fixedCollectionFixture(t, 150)
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	key := plans[0].rows[149].Key
	raw := `{"status":"success","summary":"audit","structured_payload":` + fixedCollectionSubmission(t, plans[0], func(rows map[string]any) {
		rows[key].(map[string]any)["state"] = "invalid"
	}) + `}`
	issues, truncated, err := resultSubmissionIssues(raw, tool.candidateValidationInfo())
	if err != nil || truncated || len(issues) != 1 || issues[0].Code != "invalid_enum" || issues[0].Pointer != "/structured_payload/decisions/"+escapeJSONPointerToken(key)+"/state" {
		t.Fatalf("lost authoritative collection diagnostic: %+v %t %v", issues, truncated, err)
	}
}

func TestCandidateToolResolutionPinsValidatedVersion(t *testing.T) {
	for _, mode := range []WorkerToolResolutionMode{WorkerToolResolutionResultRepair, WorkerToolResolutionResume} {
		t.Run(string(mode), func(t *testing.T) {
			c, item, ctx, _ := resultCandidateFixture(t)
			candidate := rejectCandidate(t, c, item, ctx, candidateMisplaced)
			request := WorkerToolResolutionRequest{Task: taskDefFromTodoItem(c.todoItemByID(item.ID)), TodoID: item.ID, Mode: mode}
			resolved, err := c.ToolResolver().ResolveTaskTools(ctx, c.session.Agents["worker"], request)
			if err != nil {
				t.Fatal(err)
			}
			if len(resolved.Tools) != 1 || !reflect.DeepEqual(resolved.Tools[0].Info().Required, []string{"edits"}) {
				t.Fatalf("repair tools were not restricted: %v", resolved.Names)
			}
			current := c.todoItemByID(item.ID)
			current.ResultCandidate = cloneResultCandidate(candidate)
			current.ResultCandidate.ContractHash = "changed"
			if err := c.taskTracker.TodoList().TryApplyProjectedItem(current); err != nil {
				t.Fatal(err)
			}
			if _, err := c.ToolResolver().ResolveTaskTools(ctx, c.session.Agents["worker"], request); err == nil {
				t.Fatal("stale candidate advertised to a model")
			}
		})
	}
}

func TestCandidateMoveBuildsOnlyMissingDeclaredContainers(t *testing.T) {
	for _, shape := range []string{"missing", "existing_scalar", "undeclared"} {
		t.Run(shape, func(t *testing.T) {
			value, err := decodeResultContractJSON([]byte(`{"row_a":{"state":"uncertain"},"row_b":{"state":"accept"},"structured_payload":{}}`))
			if err != nil {
				t.Fatal(err)
			}
			document := value.(map[string]any)
			rows := map[string]any{"type": "object", "properties": map[string]any{"row_a": map[string]any{"type": "object"}, "row_b": map[string]any{"type": "object"}}}
			schema := map[string]any{"properties": map[string]any{"structured_payload": map[string]any{"type": "object", "properties": map[string]any{"decisions": rows}}}}
			if shape == "existing_scalar" {
				document["structured_payload"].(map[string]any)["decisions"] = "keep"
			}
			if shape == "undeclared" {
				schema = map[string]any{}
			}
			issues := []ResultSubmissionIssue{{Code: "additional_property", Pointer: "/row_a"}, {Code: "additional_property", Pointer: "/row_b"}, {Code: "missing_property", Pointer: "/structured_payload/decisions"}}
			for _, key := range []string{"row_a", "row_b"} {
				err = applyResultCandidateEdit(document, resultCandidateEdit{Operation: "move", From: "/" + key, Path: "/structured_payload/decisions/" + key}, issues, schema)
				if shape != "missing" {
					if err == nil {
						t.Fatal("move overwrote or invented a parent")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			encoded, err := json.Marshal(document, json.Deterministic(true))
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != `{"structured_payload":{"decisions":{"row_a":{"state":"uncertain"},"row_b":{"state":"accept"}}}}` {
				t.Fatalf("move changed collection content: %s", encoded)
			}
		})
	}
}

func TestCandidateMoveRejectsAliasedArrayPointers(t *testing.T) {
	for _, index := range []string{"+0", "-0", "00"} {
		t.Run(index, func(t *testing.T) {
			// An alternate spelling of the same array index must not evade
			// the overlap check and move an object into its own descendant.
			const raw = `{"rows":[{"findings":{"wrong":true,"sub":{}}}]}`
			value, err := decodeResultContractJSON([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			const shape = `{"properties":{"rows":{"type":"array","items":{"properties":{"findings":{"type":"object","properties":{"sub":{"type":"object","properties":{"findings":{"type":"object"}}}}}}}}}}`
			schema, err := decodeResultContractJSON([]byte(shape))
			if err != nil {
				t.Fatal(err)
			}
			err = applyResultCandidateEdit(value.(map[string]any), resultCandidateEdit{Operation: "move", From: "/rows/0/findings", Path: "/rows/" + index + "/findings/sub/findings"}, []ResultSubmissionIssue{{Code: "additional_property", Pointer: "/rows/0/findings/wrong"}}, schema.(map[string]any))
			if err == nil {
				t.Fatal("aliased pointer bypassed overlap protection")
			}
			before, _ := decodeResultContractJSON([]byte(raw))
			if !reflect.DeepEqual(value, before) {
				t.Fatal("rejected move changed content")
			}
		})
	}
}

func TestPreparedCandidateAppendFailureCannotCommit(t *testing.T) {
	c, item, ctx, _ := resultCandidateFixture(t)
	candidate := rejectCandidate(t, c, item, ctx, candidateMisplaced)
	c.SetEventJournal(failingJournal{err: errors.New("storage unavailable")})
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID, candidate: candidate}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: candidateMove})
	if err != nil || !response.IsError || submissionFailureMetadata(response.Metadata) != RepairFailureCommit {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	if c.GetTaskResult(item.ID) != nil || c.todoItemByID(item.ID).ResultCandidate.SHA256 != candidate.SHA256 || c.occurrenceController(item.ID).snapshot().reserved {
		t.Fatal("failed prepared append published a result or leaked reservation")
	}
}
