package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

func requiredReadFixture(t *testing.T, content string) (*Coordinator, *TodoItem, context.Context, ExecutionReceipt, fantasy.AgentTool) {
	t.Helper()
	var ref ArtifactRef
	c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
		ranges := []tools.ArtifactByteRange{}
		if len(content) > 0 {
			ranges = append(ranges, tools.ArtifactByteRange{Start: 0, End: int64(len(content))})
		}
		spec.WorksetBinding = &WorksetBinding{WorksetID: "evidence", ItemKey: "unit", Inputs: []ArtifactRef{ref}, RequiredReads: []tools.ArtifactReadCoverage{{ArtifactID: ref.ID, SHA256: ref.SHA256, ByteSize: ref.ByteSize, Ranges: ranges}}}
	}, func(c *Coordinator) {
		store, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := store.Put(t.Context(), PutArtifactRequest{Kind: "source", Path: "source.txt", Content: []byte(content), RunID: c.executionRunID, TaskID: "producer", Attempt: 1, Agent: "producer"})
		if err != nil {
			t.Fatal(err)
		}
		ref = stored.ArtifactRef
	})
	scope := &ArtifactAccessScope{RunID: receipt.RunID, TaskID: item.ID, Attempt: 1, AuthorizedRefs: []ArtifactRef{ref}}
	ctx = context.WithValue(ctx, artifactAccessScopeKey, scope)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: receipt.RunID, TaskID: item.ID, AgentName: item.Agent, Attempt: 1, ModelExecutionID: receipt.ModelExecutionID})
	ctx = tools.SetToolsAllowed(ctx, []string{"view"})
	receipt.ArtifactScope = scope
	return c, item, ctx, receipt, tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))
}

func deliverRequiredRead(t *testing.T, c *Coordinator, ctx context.Context, view fantasy.AgentTool, callID, input string) {
	t.Helper()
	call := fantasy.ToolCall{ID: callID, Name: "view", Input: input}
	response, err := view.Run(ctx, call)
	if err != nil || response.IsError {
		t.Fatalf("view: %v %s", err, response.Content)
	}
	if err := c.captureRequiredArtifactRead(ctx, view, call, response); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredArtifactReadsRequireReturnedBytes(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "source", true: "empty"}[empty], func(t *testing.T) {
			content := strings.Repeat("A", 600) + "界B needle " + strings.Repeat("z", 600)
			if empty {
				content = ""
			}
			c, item, ctx, receipt, view := requiredReadFixture(t, content)
			var gaps *artifactReadCoverageIncomplete
			if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); !errors.As(err, &gaps) || len(gaps.MissingReads) != 1 {
				t.Fatalf("unread admitted: %v", err)
			}
			if _, err := c.beginResultCollection(ctx, item.ID, receipt, "report"); err == nil {
				t.Fatal("unread work entered tool-less collection")
			}
			ref := item.WorksetBinding.Inputs[0]
			if !empty {
				deliverRequiredRead(t, c, ctx, view, "search", `{"artifact_ref":"`+ref.ID+`","search_text":"needle","context_bytes":0}`)
				if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); !errors.As(err, &gaps) {
					t.Fatalf("search scan counted as delivery: %v", err)
				}
				deliverRequiredRead(t, c, ctx, view, "prefix", `{"artifact_ref":"`+ref.ID+`","byte_offset":0,"byte_limit":5}`)
				if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); !errors.As(err, &gaps) {
					t.Fatalf("partial bytes admitted: %v", err)
				}
			}
			deliverRequiredRead(t, c, ctx, view, "full", `{"artifact_ref":"`+ref.ID+`","byte_offset":0}`)
			if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err != nil {
				t.Fatal(err)
			}
			cp, err := c.beginResultCollection(ctx, item.ID, receipt, "report")
			if err != nil {
				t.Fatal(err)
			}
			events, err := c.EventJournal().ReadEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			replay := ReduceToTodoList(events)
			if len(replay) != 1 || !reflect.DeepEqual(replay[0].WorksetBinding, item.WorksetBinding) {
				t.Fatal("required reads lost during replay")
			}
			if _, err := c.loadCollectionWork(ctx, replay[0], cp); err != nil {
				t.Fatal(err)
			}
			restored, err := c.restoreCollectionContext(context.WithValue(t.Context(), sessionResumeKey{}, true), cp)
			if err != nil {
				t.Fatal(err)
			}
			restored = context.WithValue(restored, resultCollectionAdmissionKey{}, cp)
			if err := c.validateRequiredArtifactReadSubmission(restored, item.ID); err != nil {
				t.Fatalf("result-only repair lost delivered work: %v", err)
			}
		})
	}
}

func TestRequiredArtifactReadsRejectDifferentExecutionIdentity(t *testing.T) {
	for _, field := range []string{"task", "run", "attempt", "model", "occurrence", "dispatch", "contract"} {
		t.Run(field, func(t *testing.T) {
			c, item, ctx, _, view := requiredReadFixture(t, "source")
			deliverRequiredRead(t, c, ctx, view, "read", `{"artifact_ref":"`+item.WorksetBinding.Inputs[0].ID+`"}`)
			metadata, _ := invocationMetadataFromContext(ctx)
			identity, err := submitResultRuntimeIdentityFromContext(ctx, c, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "task":
				metadata.TaskID = "other"
			case "run":
				metadata.RunID = "other"
			case "attempt":
				metadata.Attempt++
			case "model":
				metadata.ModelExecutionID = "other"
			case "occurrence":
				identity.OccurrenceRevision++
				ctx = withSubmitResultRuntimeIdentity(ctx, identity)
			case "dispatch":
				identity.DispatchID = "other"
				ctx = withSubmitResultRuntimeIdentity(ctx, identity)
			case "contract":
				item.WorksetBinding.ItemKey = "other"
				if err := c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
					t.Fatal(err)
				}
			}
			ctx = withInvocationMetadata(ctx, metadata)
			if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err == nil {
				t.Fatal("different identity reused read evidence")
			}
		})
	}
}

func TestRequiredArtifactReadRangeUnion(t *testing.T) {
	for _, tc := range []struct {
		name           string
		observed, want []tools.ArtifactByteRange
	}{
		{name: "overlap", observed: []tools.ArtifactByteRange{{Start: 4, End: 8}, {Start: 0, End: 5}, {Start: 8, End: 10}}},
		{name: "hole", observed: []tools.ArtifactByteRange{{Start: 0, End: 4}, {Start: 6, End: 10}}, want: []tools.ArtifactByteRange{{Start: 4, End: 6}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := missingArtifactByteRanges([]tools.ArtifactByteRange{{Start: 0, End: 10}}, tc.observed)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("missing=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestRequiredArtifactReadsValidateAssignedIdentity(t *testing.T) {
	ref := ArtifactRef{ID: "source", SHA256: strings.Repeat("a", 64), ByteSize: 10}
	base := tools.ArtifactReadCoverage{ArtifactID: ref.ID, SHA256: ref.SHA256, ByteSize: 10, Ranges: []tools.ArtifactByteRange{{Start: 0, End: 10}}}
	for _, field := range []string{"valid", "hash", "size", "range", "duplicate", "unassigned"} {
		t.Run(field, func(t *testing.T) {
			required := cloneArtifactReadCoverage([]tools.ArtifactReadCoverage{base})
			switch field {
			case "hash":
				required[0].SHA256 = strings.Repeat("b", 64)
			case "size":
				required[0].ByteSize++
			case "range":
				required[0].Ranges[0].End++
			case "duplicate":
				required = append(required, base)
			case "unassigned":
				required[0].ArtifactID = "other"
			}
			err := validateRequiredArtifactReads([]ArtifactRef{ref}, required)
			if (err == nil) != (field == "valid") {
				t.Fatalf("validation=%v", err)
			}
		})
	}
}

func TestRequiredArtifactReadObservationRejectsContentTampering(t *testing.T) {
	c, item, ctx, _, view := requiredReadFixture(t, "source")
	call := fantasy.ToolCall{ID: "read", Name: "view", Input: `{"artifact_ref":"` + item.WorksetBinding.Inputs[0].ID + `"}`}
	response, err := view.Run(ctx, call)
	if err != nil || response.IsError {
		t.Fatal(err)
	}
	response.Content = "substituted"
	if err := c.captureRequiredArtifactRead(ctx, view, call, response); err != nil {
		t.Fatal(err)
	}
	if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err == nil {
		t.Fatal("tampered response counted")
	}
	raw, _ := json.Marshal(map[string]any{"version": 1})
	if err := validateArtifactReadObservationEvent(RunEvent{Payload: raw}); err == nil {
		t.Fatal("malformed durable observation accepted")
	}
}

func TestRequiredArtifactReadsRejectUnsupportedExecution(t *testing.T) {
	for _, kind := range []string{"local", "sidecar", "action", "steps", "closed_sequence", "fanout", "no_view", "no_journal"} {
		t.Run(kind, func(t *testing.T) {
			c, item, ctx, _, view := requiredReadFixture(t, "source")
			task := taskDefFromTodoItem(item)
			resolved := ResolvedWorkerTools{Tools: []fantasy.AgentTool{view}, AuthorizedNames: []string{"view"}}
			switch kind {
			case "sidecar":
				task.Sidecar = true
			case "action":
				task.Action = &Action{}
			case "steps":
				task.Execution.Steps = []ExecutionStep{{}}
			case "closed_sequence":
				task.Execution.ToolSequence = []string{"submit_result"}
			case "fanout":
				task.ExecutionTopology = append(task.ExecutionTopology, task.ResolvedExecutionTarget)
			case "no_view":
				resolved.Tools = nil
			case "no_journal":
				store, journal := c.eventStore, c.eventJournal
				t.Cleanup(func() { c.eventStore, c.eventJournal = store, journal })
				c.eventStore, c.eventJournal = nil, nil
			}
			err := c.validateRequiredArtifactReadExecution(ctx, task, item, resolved)
			if (err == nil) != (kind == "local") {
				t.Fatalf("preflight %s: %v", kind, err)
			}
		})
	}
}

func TestRequiredArtifactReadsVerifyPersistedEvidenceAfterDelivery(t *testing.T) {
	for _, kind := range []string{"source", "response"} {
		t.Run(kind, func(t *testing.T) {
			c, item, ctx, _, view := requiredReadFixture(t, "original source")
			ref := item.WorksetBinding.Inputs[0]
			budgetCtx, budget := WithArtifactReadBudget(ctx, 1<<20)
			deliverRequiredRead(t, c, budgetCtx, view, "bounded", `{"artifact_ref":"`+ref.ID+`","byte_offset":0}`)
			if budget.Err() != nil {
				t.Fatal(budget.Err())
			}
			if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err != nil {
				t.Fatal(err)
			}
			if kind == "response" {
				events, err := c.readActiveLineageEvents(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Type == string(EventArtifactReadCoverageObserved) {
						var observation artifactReadObservation
						if err := json.Unmarshal(event.Payload, &observation); err != nil {
							t.Fatal(err)
						}
						ref = observation.Output
					}
				}
			}
			store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.projectDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store.root, "data", ref.ID), []byte("tampered evidence"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := c.validateRequiredArtifactReadSubmission(ctx, item.ID); err == nil {
				t.Fatal("changed persisted evidence reused prior coverage")
			}
		})
	}
}

func TestRequiredArtifactReadsExcludeWithheldDecisionResponses(t *testing.T) {
	f, view, input := boundReadBudgetFixture(t, 7000, "UNSEEN"+strings.Repeat("\"\\\n", 1500))
	scope, _ := artifactAccessScopeFromContext(f.ctx)
	ref := scope.AuthorizedRefs[len(scope.AuthorizedRefs)-1]
	item := todoByID(t, f.c, f.item.ID)
	item.WorksetBinding = &WorksetBinding{WorksetID: "evidence", ItemKey: "unit", Inputs: []ArtifactRef{ref}, RequiredReads: []tools.ArtifactReadCoverage{{ArtifactID: ref.ID, SHA256: ref.SHA256, ByteSize: ref.ByteSize, Ranges: []tools.ArtifactByteRange{{Start: 0, End: ref.ByteSize}}}}}
	if err := f.c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
		t.Fatal(err)
	}
	response, err := view.Run(f.ctx, fantasy.ToolCall{ID: "withheld", Name: "view", Input: input})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "decision_read_budget_exhausted") {
		t.Fatalf("view: %v %s", err, response.Content)
	}
	events, err := f.c.readActiveLineageEvents(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == string(EventArtifactReadCoverageObserved) {
			t.Fatal("withheld source earned delivery coverage")
		}
	}
}
