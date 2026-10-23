package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

const artifactKeysTestSchema = `{
 "type":"object","additionalProperties":false,"required":["claims"],
 "properties":{"claims":{"type":"array","items":{"type":"object","additionalProperties":false,
 "required":["evidence"],"properties":{"evidence":{"type":"array","minItems":1,"items":{"type":"string","pattern":"^sha256-[0-9a-f]{64}$"}},"note":{"type":"string"}}}}},
 "x-hufu-evidence-keys":{"pointers":["/claims/*/evidence/*"]}}
`

type artifactKeysFixture struct {
	c    *Coordinator
	item *TodoItem
	ctx  context.Context
	ref  ArtifactRef
	key  string
	view fantasy.AgentTool
}

func newArtifactKeysFixture(t *testing.T) *artifactKeysFixture {
	t.Helper()
	compiled, err := CompileResultContractDocument("keys.json", []byte(artifactKeysTestSchema))
	if err != nil {
		t.Fatal(err)
	}
	c, item := resultContractCoordinator(t, true)
	c.session.Workspace = t.TempDir()
	c.session.Agents = map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker", Tools: "view"}}
	c.session.ResultContracts[compiled.ID] = compiled
	ref := compiled.ref(true)
	item.ResultContract = &ref
	c.executionRunID = "run-key"
	store, err := NewEventStore(c.session.Workspace, "run-key", "session-key")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("main")
	t.Cleanup(func() { _ = store.Close() })
	c.eventStore, c.eventJournal = store, eventStoreJournal{store: store}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{Type: string(EventTaskCreated), TaskID: item.ID, Actor: item.Agent, BranchID: "main", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	artifacts, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := artifacts.Put(t.Context(), PutArtifactRequest{Kind: "test", Path: "source.txt", Content: []byte("bounded source evidence\n"), TaskID: "producer"})
	if err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(item.ID, 1)
	identity, ok := c.activeTaskResultOccurrence(item.ID)
	if !ok {
		t.Fatal("missing occurrence")
	}
	ctx := withSubmitResultRuntimeIdentity(t.Context(), identity)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: "run-key", TaskID: item.ID, AgentName: item.Agent, Attempt: 1, ModelExecutionID: "model-a"})
	ctx = context.WithValue(ctx, todoIDKey{}, item.ID)
	ctx = context.WithValue(ctx, artifactAccessScopeKey, &ArtifactAccessScope{RunID: "run-key", TaskID: item.ID, Attempt: 1, AuthorizedRefs: []ArtifactRef{artifact.ArtifactRef}})
	ctx = tools.SetToolsAllowed(ctx, []string{"view", submitResultToolName})
	view := tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))
	return &artifactKeysFixture{c: c, item: item, ctx: ctx, ref: artifact.ArtifactRef, view: c.gatePolicyTools([]fantasy.AgentTool{view})[0]}
}

func (f *artifactKeysFixture) read(t *testing.T, input, callID string) fantasy.ToolResponse {
	t.Helper()
	response, err := f.view.Run(f.ctx, fantasy.ToolCall{Name: "view", ID: callID, Input: input})
	if err != nil || response.IsError {
		t.Fatalf("view failed: %#v %v", response, err)
	}
	for _, line := range strings.Split(response.Content, "\n") {
		if strings.HasPrefix(line, evidenceKeyPrefix) {
			f.key = strings.Fields(line)[0]
		}
	}
	if len(f.key) != 15 {
		t.Fatalf("no issued key: %s", response.Content)
	}
	return response
}

func (f *artifactKeysFixture) readSource(t *testing.T) {
	t.Helper()
	f.read(t, fmt.Sprintf(`{"artifact_ref":%q,"byte_limit":8}`, f.ref.ID), "read-source")
}

func (f *artifactKeysFixture) submission(key string) string {
	return fmt.Sprintf(`{"status":"success","summary":"reviewed","files_read":[{"path":%q}],"structured_payload":{"claims":[{"evidence":[%q],"note":%q}]}}`, key, key, "non-authoritative mention "+key)
}

func TestArtifactEvidenceKeysCanonicalSubmission(t *testing.T) {
	for _, gated := range []bool{false, true} {
		t.Run(fmt.Sprint(gated), func(t *testing.T) {
			f := newArtifactKeysFixture(t)
			f.readSource(t)
			var tool fantasy.AgentTool = &submitResultTool{coordinator: f.c, todoID: f.item.ID}
			if gated {
				tool = f.c.gatePolicyTools([]fantasy.AgentTool{tool})[0]
			}
			input := f.submission(f.key)
			if err := validateToolArguments(input, tool.Info()); err != nil {
				t.Fatalf("advertised schema rejects key: %v", err)
			}
			response, err := tool.Run(f.ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input})
			if err != nil || response.IsError {
				t.Fatalf("submit: %#v %v", response, err)
			}
			result := f.c.GetTaskResult(f.item.ID)
			if result == nil || len(result.FilesRead) != 1 || result.FilesRead[0].Path != f.ref.ID || result.StructuredPayload == nil {
				t.Fatalf("noncanonical result: %#v", result)
			}
			canonical := fmt.Sprintf(`{"claims":[{"evidence":[%q],"note":%q}]}`, f.ref.ID, "non-authoritative mention "+f.key)
			compiled := f.c.session.ResultContracts["keys.json"]
			expected, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(canonical))
			if err != nil {
				t.Fatal(err)
			}
			if result.StructuredPayload.SHA256 != expected.SHA256 || string(result.StructuredPayload.Value) != string(expected.Value) {
				t.Fatalf("canonical hash/value mismatch: %#v", result.StructuredPayload)
			}
			if _, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(fmt.Sprintf(`{"claims":[{"evidence":[%q]}]}`, f.key))); err == nil {
				t.Fatal("canonical validator accepted an unresolved key")
			}
		})
	}
}

func TestArtifactEvidenceKeysRejectUnknownUnreadAndChangedSources(t *testing.T) {
	for _, name := range []string{"unknown", "unread", "revoked", "corrupt", "observation", "attempt", "task", "actor", "model", "branch", "run", "schema", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			f := newArtifactKeysFixture(t)
			f.readSource(t)
			key, ctx := f.key, f.ctx
			submitID := f.item.ID
			metadata, _ := invocationMetadataFromContext(ctx)
			switch name {
			case "unknown":
				key = "ev_000000000000"
			case "unread":
				events, err := f.c.readActiveLineageEvents(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range events {
					if e.Type == string(EventArtifactEvidenceObserved) {
						var p decisionArtifactObservation
						if err := json.Unmarshal(e.Payload, &p); err != nil {
							t.Fatal(err)
						}
						other := f.ref
						other.ID = "sha256-" + strings.Repeat("a", 64)
						other.SHA256 = strings.Repeat("a", 64)
						key = artifactEvidenceKey(e, p, other)
					}
				}
			case "revoked":
				ctx = context.WithValue(ctx, artifactAccessScopeKey, &ArtifactAccessScope{RunID: "run-key", TaskID: f.item.ID, Attempt: 1})
			case "corrupt":
				if err := os.WriteFile(filepath.Join(f.c.session.Workspace, logsDir, "artifacts", "data", f.ref.ID), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "observation":
				events, err := f.c.readActiveLineageEvents(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Type != string(EventArtifactEvidenceObserved) {
						continue
					}
					var p decisionArtifactObservation
					if err := json.Unmarshal(event.Payload, &p); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(f.c.session.Workspace, logsDir, "artifacts", "data", p.Output.ID), []byte("corrupt read proof"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			case "attempt":
				metadata.Attempt = 2
				identity, _ := f.c.activeTaskResultOccurrence(f.item.ID)
				identity.Attempt = 2
				ctx = withSubmitResultRuntimeIdentity(ctx, identity)
			case "task":
				other := f.c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", ResultContract: f.item.ResultContract}})[0]
				f.c.setCurrentTaskAttempt(other.ID, 1)
				identity, ok := f.c.activeTaskResultOccurrence(other.ID)
				if !ok {
					t.Fatal("missing other occurrence")
				}
				ctx = withSubmitResultRuntimeIdentity(ctx, identity)
				metadata.TaskID, submitID = other.ID, other.ID
				ctx = context.WithValue(ctx, todoIDKey{}, other.ID)
				ctx = context.WithValue(ctx, artifactAccessScopeKey, &ArtifactAccessScope{RunID: "run-key", TaskID: other.ID, Attempt: 1, AuthorizedRefs: []ArtifactRef{f.ref}})
			case "actor":
				metadata.AgentName = "other"
			case "model":
				metadata.ModelExecutionID = "other-model"
			case "branch":
				tree := NewSessionTree()
				branch, err := tree.CreateBranch("other", "main", f.c.eventStore)
				if err != nil {
					t.Fatal(err)
				}
				tree.ActiveBranch = branch.ID
				if err := SaveSessionTree(f.c.session.Workspace, tree); err != nil {
					t.Fatal(err)
				}
			case "run":
				identity, _ := f.c.activeTaskResultOccurrence(f.item.ID)
				identity.RunID = "other-run"
				ctx = withSubmitResultRuntimeIdentity(ctx, identity)
			case "schema":
				f.c.session.ResultContracts["keys.json"].SchemaSHA256 = strings.Repeat("f", 64)
			}
			ctx = withInvocationMetadata(ctx, metadata)
			input := f.submission(key)
			if name == "duplicate" {
				input = strings.Replace(input, `"evidence":[`, `"evidence":[],"evidence":[`, 1)
			}
			response, err := (&submitResultTool{coordinator: f.c, todoID: submitID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input})
			if err != nil || !response.IsError || f.c.GetTaskResult(submitID) != nil {
				t.Fatalf("unsafe input accepted: %#v %v", response, err)
			}
		})
	}
}

func TestArtifactEvidenceKeysReplayAndPartialBatch(t *testing.T) {
	f := newArtifactKeysFixture(t)
	response := f.read(t, fmt.Sprintf(`{"items":[{"artifact_ref":%q,"byte_limit":4},{"artifact_ref":"not-authorized","byte_limit":4}]}`, f.ref.ID), "partial-batch")
	if !strings.Contains(response.Content, `status="partial_failure"`) || !reflect.DeepEqual(tools.ViewArtifactReads(response), []string{f.ref.ID}) {
		t.Fatalf("wrong successful reads: %#v", response)
	}
	// Close and reopen the journal. No in-memory alias registry is carried over.
	if err := f.c.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(f.c.session.Workspace, "run-key", "session-key")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("main")
	t.Cleanup(func() { _ = store.Close() })
	f.c.eventStore, f.c.eventJournal = store, eventStoreJournal{store: store}
	repaired := context.WithValue(f.ctx, protocolRepairExecutionKey{}, true)
	normalized, err := f.c.resolveResultEvidenceKeys(repaired, f.item.ID, f.submission(f.key))
	if err != nil || !strings.Contains(normalized, f.ref.ID) {
		t.Fatalf("durable repair replay failed: %s %v", normalized, err)
	}
	response, err = (&submitResultTool{coordinator: f.c, todoID: f.item.ID}).Run(tools.SetToolsAllowed(repaired, []string{submitResultToolName}), fantasy.ToolCall{Name: submitResultToolName, Input: f.submission(f.key)})
	if err != nil || response.IsError {
		t.Fatalf("result-only repair rejected: %#v %v", response, err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == string(EventArtifactEvidenceObserved) {
			count++
		}
		if event.Type == string(EventDecisionPrimitiveStarted) {
			t.Fatal("repair inferred")
		}
	}
	if count != 1 {
		t.Fatalf("read was replayed: %d", count)
	}
}

func TestArtifactEvidenceKeysLegacyAndLiteralCompatibility(t *testing.T) {
	f := newArtifactKeysFixture(t)
	f.readSource(t)
	input := f.submission(f.ref.ID)
	normalized, err := f.c.resolveResultEvidenceKeys(f.ctx, f.item.ID, input)
	if err != nil || normalized != input {
		t.Fatalf("literal input changed: %s %v", normalized, err)
	}
	compiled := f.c.session.ResultContracts["keys.json"]
	compiled.evidenceKeys = nil
	input = f.submission(f.key)
	normalized, err = f.c.resolveResultEvidenceKeys(f.ctx, f.item.ID, input)
	if err != nil || normalized != input {
		t.Fatalf("disabled contract changed: %s %v", normalized, err)
	}
}

func TestArtifactEvidenceKeysWithBoundDecisionAssembly(t *testing.T) {
	f := newBoundDecisionFixture(t, 1, func(root map[string]any) {
		row := root["properties"].(map[string]any)["decisions"].(map[string]any)["items"].(map[string]any)
		properties := row["properties"].(map[string]any)
		properties["evidence"] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "pattern": "^sha256-[0-9a-f]{64}$"}}
		root["x-hufu-evidence-keys"] = map[string]any{"pointers": []string{"/decisions/*/evidence/*"}}
	})
	refs, err := f.c.resultEvidenceKeyRefs(f.ctx, f.item.ID, f.c.session.ResultContracts[f.item.ResultContract.ID])
	if err != nil || len(refs) != 1 {
		t.Fatalf("issued refs: %v %v", refs, err)
	}
	var key string
	for k := range refs {
		key = k
	}
	if response := f.call(t, 0, "choose"); response.IsError {
		t.Fatal(response.Content)
	}
	raw := f.submission(t, func(rows map[string]any) {
		for _, r := range rows {
			r.(map[string]any)["evidence"] = []string{key}
		}
	})
	payload, err := decodeUniqueJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{"status": "success", "summary": "done", "structured_payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	tool := &submitResultTool{coordinator: f.c, todoID: f.item.ID}
	if err := validateToolArguments(string(input), tool.Info()); err != nil {
		t.Fatalf("keyed schema rejects evidence key: %v", err)
	}
	response, err := tool.Run(f.ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(input)})
	if err != nil || response.IsError {
		t.Fatalf("submit: %#v %v", response, err)
	}
	result := f.c.GetTaskResult(f.item.ID)
	if result == nil || result.StructuredPayload == nil || strings.Contains(string(result.StructuredPayload.Value), key) || !strings.Contains(string(result.StructuredPayload.Value), refs[key].ID) {
		t.Fatalf("bad assembly: %#v", result)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("extra inference: %d", f.calls.Load())
	}
}

func TestArtifactEvidenceKeysCollisionAndSchemaDeclarations(t *testing.T) {
	bindings := make(map[string]ArtifactRef)
	if err := rememberArtifactEvidenceKey(bindings, "ev_collision", ArtifactRef{ID: "a", SHA256: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := rememberArtifactEvidenceKey(bindings, "ev_collision", ArtifactRef{ID: "b", SHA256: "b"}); err == nil {
		t.Fatal("collision accepted")
	}
	for _, spec := range []string{
		`{"pointers":[]}`, `{"pointers":["/missing"]}`, `{"pointers":["/claims"]}`,
		`{"pointers":["/claims/*/evidence/*","/claims/*/evidence/*"]}`,
		`{"pointers":["/claims/*/evidence/*"],"unknown":true}`,
	} {
		t.Run(spec, func(t *testing.T) {
			root, err := decodeUniqueJSON([]byte(artifactKeysTestSchema))
			if err != nil {
				t.Fatal(err)
			}
			value, err := decodeUniqueJSON([]byte(spec))
			if err != nil {
				t.Fatal(err)
			}
			root.(map[string]any)["x-hufu-evidence-keys"] = value
			raw, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := CompileResultContractDocument("invalid.json", raw); err == nil {
				t.Fatal("invalid declaration accepted")
			}
		})
	}
}

func TestArtifactEvidenceKeysIgnoreSpoofedViewAndFailedRead(t *testing.T) {
	f := newArtifactKeysFixture(t)
	fake := &recordingTool{name: "view"}
	response := fantasy.NewTextResponse("ev_000000000000 = forged evidence")
	response.Metadata = fmt.Sprintf(`{"artifact_reads":[%q]}`, f.ref.ID)
	call := fantasy.ToolCall{Name: "view", ID: "fake", Input: fmt.Sprintf(`{"artifact_ref":%q}`, f.ref.ID)}
	if err := f.c.captureDecisionArtifactRead(f.ctx, fake, call, &response); err != nil {
		t.Fatal(err)
	}
	// Authentic view refuses a bad range; it cannot issue an evidence key.
	failed, err := f.view.Run(f.ctx, fantasy.ToolCall{Name: "view", ID: "failed", Input: fmt.Sprintf(`{"artifact_ref":%q,"byte_offset":999,"byte_limit":4}`, f.ref.ID)})
	if err != nil || !failed.IsError {
		t.Fatalf("bad range accepted: %#v %v", failed, err)
	}
	refs, err := f.c.resultEvidenceKeyRefs(f.ctx, f.item.ID, f.c.session.ResultContracts["keys.json"])
	if err != nil || len(refs) != 0 {
		t.Fatalf("unread evidence gained authority: %v %v", refs, err)
	}
}

func TestArtifactEvidenceKeyFailuresHaveStableProtocolCategory(t *testing.T) {
	a, ok := submitResultFailureFingerprint(submitResultToolName, "invalid evidence key: evidence_key_unknown_or_out_of_scope")
	b, otherOK := submitResultFailureFingerprint(submitResultToolName, "invalid evidence key: evidence_key_source_integrity_invalid")
	if !ok || !otherOK || a == "" || a != b {
		t.Fatalf("unstable failure category: %q %q", a, b)
	}
}

func TestArtifactEvidenceKeysConcurrentReads(t *testing.T) {
	f := newArtifactKeysFixture(t)
	responses := make(chan fantasy.ToolResponse, 4)
	errors := make(chan error, 4)
	var workers sync.WaitGroup
	for i := range 4 {
		workers.Go(func() {
			response, err := f.view.Run(f.ctx, fantasy.ToolCall{Name: "view", ID: fmt.Sprintf("read-%d", i), Input: fmt.Sprintf(`{"artifact_ref":%q,"byte_limit":8}`, f.ref.ID)})
			responses <- response
			errors <- err
		})
	}
	workers.Wait()
	close(responses)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for response := range responses {
		if response.IsError {
			t.Fatal(response.Content)
		}
	}
	refs, err := f.c.resultEvidenceKeyRefs(f.ctx, f.item.ID, f.c.session.ResultContracts["keys.json"])
	if err != nil || len(refs) != 1 {
		t.Fatalf("unstable concurrent keys: %v %v", refs, err)
	}
	events, err := f.c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	observed := 0
	for _, e := range events {
		if e.Type == string(EventArtifactEvidenceObserved) {
			observed++
		}
	}
	if observed != 4 {
		t.Fatalf("lost read provenance: %d", observed)
	}
}

func TestArtifactEvidenceKeysDoNotWidenDecisionContextLimit(t *testing.T) {
	f := newArtifactKeysFixture(t)
	store, err := NewFileArtifactStore(f.c.session.Workspace, f.c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	large, err := store.Put(t.Context(), PutArtifactRequest{Kind: "test", Path: "large.txt", Content: []byte(strings.Repeat("x", 100000)), TaskID: "producer"})
	if err != nil {
		t.Fatal(err)
	}
	f.ref = large.ArtifactRef
	f.ctx = context.WithValue(f.ctx, artifactAccessScopeKey, &ArtifactAccessScope{RunID: "run-key", TaskID: f.item.ID, Attempt: 1, AuthorizedRefs: []ArtifactRef{f.ref}})
	f.read(t, fmt.Sprintf(`{"items":[{"artifact_ref":%q,"byte_limit":32768},{"artifact_ref":%q,"byte_offset":32768,"byte_limit":32768}]}`, f.ref.ID, f.ref.ID), "large-read")
	refs, err := f.c.resultEvidenceKeyRefs(f.ctx, f.item.ID, f.c.session.ResultContracts["keys.json"])
	if err != nil || len(refs) != 1 {
		t.Fatalf("valid bounded read cannot issue keys: %v %v", refs, err)
	}
	events, err := f.c.readActiveLineageEvents(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := invocationMetadataFromContext(f.ctx)
	_, err = f.c.buildDecisionContext(f.ctx, events, decisionPrimitiveScope(events, "main", metadata), f.c.session.ResultContracts["keys.json"], resultFixedCollectionRow{Finding: map[string]any{"claim": "check"}}, resultDecisionContextMax)
	if err == nil || !strings.Contains(err.Error(), "decision_context_too_large") {
		t.Fatalf("decision context limit widened: %v", err)
	}
}

func TestArtifactEvidenceKeysPreserveNumericAndFileRefRepresentations(t *testing.T) {
	f := newArtifactKeysFixture(t)
	root, err := decodeUniqueJSON([]byte(artifactKeysTestSchema))
	if err != nil {
		t.Fatal(err)
	}
	properties := root.(map[string]any)["properties"].(map[string]any)
	properties["number"] = map[string]any{"type": "integer"}
	properties["ratio"] = map[string]any{"type": "number"}
	properties["flag"] = map[string]any{"type": "boolean"}
	properties["absent"] = map[string]any{"type": "null"}
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileResultContractDocument("numeric.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	ref := compiled.ref(true)
	f.c.session.ResultContracts[compiled.ID] = compiled
	f.item.ResultContract = &ref
	f.readSource(t)
	for _, filesRead := range []string{fmt.Sprintf(`[%q]`, f.key), fmt.Sprintf(`%q`, f.key), fmt.Sprintf(`[{"path":%q}]`, f.key)} {
		input := fmt.Sprintf(`{"status":"success","summary":"reviewed","files_read":%s,"structured_payload":{"claims":[{"evidence":[%q]}],"number":12345678901234567890123456789,"ratio":1.2300e+02,"flag":true,"absent":null}}`, filesRead, f.key)
		normalized, err := f.c.resolveResultEvidenceKeys(f.ctx, f.item.ID, input)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(normalized, `"number":12345678901234567890123456789`) || !strings.Contains(normalized, `"ratio":1.2300e+02`) || !strings.Contains(normalized, `"absent":null`) {
			t.Fatalf("values changed: %s", normalized)
		}
		decoded, err := decodeSubmitResultInput([]byte(normalized), taskResultSubmissionContract{AllowArtifacts: true, AllowEvidence: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(decoded.FilesRead) != 1 || decoded.FilesRead[0].Path != f.ref.ID {
			t.Fatalf("unresolved file reference: %#v", decoded.FilesRead)
		}
		if _, err := validateStructuredResultPayload(compiled, ref, decoded.StructuredPayload); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArtifactEvidenceKeysRejectForgedDurableProjection(t *testing.T) {
	f := newArtifactKeysFixture(t)
	f.readSource(t)
	events, err := f.c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != string(EventArtifactEvidenceObserved) {
			continue
		}
		var observation decisionArtifactObservation
		if err := json.Unmarshal(event.Payload, &observation); err != nil {
			t.Fatal(err)
		}
		if observation.EvidenceKeys[f.key] != f.ref.ID {
			t.Fatal("missing durable key projection")
		}
		observation.EvidenceKeys[f.key] = "sha256-" + strings.Repeat("f", 64)
		event.Payload, err = json.Marshal(observation)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateDecisionObservationEvent(event); err == nil {
			t.Fatal("forged key projection accepted")
		}
		return
	}
	t.Fatal("missing observation")
}

func TestArtifactEvidenceKeyCorrectionHintBounded(t *testing.T) {
	f := newArtifactKeysFixture(t)
	f.readSource(t)
	_, err := f.c.resolveResultEvidenceKeys(f.ctx, f.item.ID, f.submission("ev_000000000000"))
	if err == nil || !strings.Contains(err.Error(), f.key) || !strings.Contains(err.Error(), f.ref.ID) {
		t.Fatalf("missing authoritative repair map: %v", err)
	}
	refs := make(map[string]ArtifactRef)
	for i := range 100 {
		refs[fmt.Sprintf("ev_%012x", i)] = f.ref
	}
	hint := evidenceKeyCorrectionHint(refs)
	if len(hint) > 1024 || !strings.Contains(hint, "exceeds") || strings.Contains(hint, "ev_000000000000") {
		t.Fatalf("unbounded/partial repair map: %s", hint)
	}
}

func TestArtifactEvidenceKeysSearchUsesTheSameAuthorizedObservation(t *testing.T) {
	f := newArtifactKeysFixture(t)
	f.read(t, fmt.Sprintf(`{"artifact_ref":%q,"search_text":"bounded","context_bytes":16}`, f.ref.ID), "search-source")
	response, err := (&submitResultTool{coordinator: f.c, todoID: f.item.ID}).Run(f.ctx, fantasy.ToolCall{Name: submitResultToolName, Input: f.submission(f.key)})
	if err != nil || response.IsError {
		t.Fatalf("search evidence submit=%#v %v", response, err)
	}
	result := f.c.GetTaskResult(f.item.ID)
	if result == nil || len(result.FilesRead) != 1 || result.FilesRead[0].Path != f.ref.ID {
		t.Fatalf("result=%+v", result)
	}
}
