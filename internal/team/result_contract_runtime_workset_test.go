package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
)

const runtimeWorksetInputSchema = `{"type":"object","required":["record"],"properties":{"record":{"$ref":"#/$defs/record"}},"$defs":{"record":{"type":"object","additionalProperties":false,"required":["decision","identity"],"properties":{"decision":{"type":"boolean"},"identity":{"type":"string","minLength":1}}}},"x-hufu-runtime-inputs":[{"target":"/record/identity","source_kind":"workset_binding","output":"identity"}]}`

func runtimeWorksetInputFixture(t *testing.T, paths ...string) (*Coordinator, *TodoItem, *TodoItem, *CompiledResultContract) {
	t.Helper()
	return runtimeWorksetInputFixtureWithSchema(t, runtimeWorksetInputSchema, map[string]string{"identity": "frozen-value"}, paths...)
}

func runtimeWorksetInputFixtureWithSchema(t *testing.T, schema string, bindings map[string]string, paths ...string) (*Coordinator, *TodoItem, *TodoItem, *CompiledResultContract) {
	t.Helper()
	compiled, err := CompileResultContractDocument("assigned.json", []byte(schema))
	if err != nil {
		t.Fatal(err)
	}
	c := newTestCoordinator(t)
	c.taskTracker.TodoList().SetRunID(c.executionRunID)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	body, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{{Key: "assigned-child", Bindings: bindings, TouchedPaths: paths}}})
	if err != nil {
		t.Fatal(err)
	}
	producer, manifest := runtimeWorksetProducerFixture(t, c, body)
	expanded, err := c.expandFanOutTasks(t.Context(), []TaskDef{{ID: "consume", Agent: "worker", FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "seed", Artifact: manifest.ID}, GoalTemplate: "Evaluate {identity}"}}})
	if err != nil || len(expanded) != 1 {
		t.Fatalf("expand: %v", err)
	}
	ref := compiled.ref(true)
	child := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", PlanTaskID: expanded[0].ID, WorksetBinding: expanded[0].WorksetBinding, ResultContract: &ref}})[0]
	receipts, err := buildWorksetReceipts(expanded, []string{child.ID}, c.executionRunID)
	if err != nil {
		t.Fatal(err)
	}
	child.WorksetReceipt = receipts[child.WorksetBinding.WorksetID]
	c.taskTracker.TodoList().UpdateStatus(child.ID, TaskInProgress, "evaluate")
	return c, producer, child, compiled
}

func TestExternalStandaloneWorksetInputsRequireVerifiedOccurrence(t *testing.T) {
	t.Parallel()
	const schema = `{"type":"object","properties":{"checks":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"}}}}},"x-hufu-workset-inputs":{"items_pointer":"/checks","item_id_pointer":"/id","binding":"assigned"}}`
	c, _, child, compiled := runtimeWorksetInputFixtureWithSchema(t, schema, map[string]string{"identity": "name", "assigned": `["first"]`})
	request := scopedAttemptRequest("codex", child.ID)
	request.Task = taskDefFromTodoItem(child)
	request.Task.ResultContract = new(compiled.ref(false))
	request.resultContractSchema = compiled
	request.resultWorksetInputIDs = func() ([]string, error) {
		return c.verifiedResultWorksetInputIDs(t.Context(), child.ID, compiled.worksetInputs)
	}
	root := t.TempDir()
	check := func(t *testing.T, valid bool) {
		t.Helper()
		for _, raw := range []*string{nil, new(`{"checks":[{"id":"first"}]}`)} {
			proposal := &WorkerResultProposal{Status: TaskResultStatusSuccess, Summary: "reviewed", StructuredPayloadJSON: raw}
			result, err := NewExternalResultCanonicalizer().Canonicalize(t.Context(), request, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
			if valid && (err != nil || result == nil || (result.StructuredPayload != nil) != (raw != nil)) ||
				!valid && (result != nil || localResultRejectionReason(err) != RepairFailureSource) {
				t.Fatalf("workset admission valid=%t: result=%+v err=%v", valid, result, err)
			}
		}
		err := authenticateRejectedExternalResult(t.Context(), request, `{"status":"success","summary":"",`, WorkspaceDelta{}, root)
		if valid && err != nil || !valid && localResultRejectionReason(err) != RepairFailureSource {
			t.Fatalf("workset source validation before format repair valid=%t: %v", valid, err)
		}
	}
	check(t, true)
	child.WorksetReceipt.Children[child.WorksetBinding.ItemKey] = "different-child"
	check(t, false)
}

func TestWorksetSubmissionSharesReadBudgetThroughOccurrenceCommit(t *testing.T) {
	t.Parallel()
	const schema = `{"type":"object","properties":{"checks":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"}}}}},"x-hufu-workset-inputs":{"items_pointer":"/checks","item_id_pointer":"/id","binding":"assigned"}}`
	for _, name := range []string{"assembly", "commit budget", "commit cancelled", "accepted"} {
		t.Run(name, func(t *testing.T) {
			c, _, child, _ := runtimeWorksetInputFixtureWithSchema(t, schema, map[string]string{"identity": "name", "assigned": `["first"]`})
			c.setCurrentTaskAttempt(child.ID, 1)
			identity, ok := c.activeTaskResultOccurrence(child.ID)
			if !ok {
				t.Fatal("missing active occurrence")
			}
			raw := []byte(`{"checks":[{"id":"first"}]}`)
			const limit = 1 << 20
			probeCtx, probeBudget := WithArtifactReadBudget(t.Context(), limit)
			if _, err, _ := c.validateStructuredPayloadClaims(probeCtx, child.ID, raw, false); err != nil {
				t.Fatal(err)
			}
			assemblyBytes := limit - probeBudget.remaining
			if assemblyBytes <= 1 {
				t.Fatal("assembly did not charge the frozen workset reads")
			}
			budgetBytes := int64(limit)
			switch name {
			case "assembly":
				budgetBytes = 1
			case "commit budget":
				budgetBytes = assemblyBytes
			}
			ctx, budget := WithArtifactReadBudget(t.Context(), budgetBytes)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			payload, err, repairable := c.validateStructuredPayloadClaims(ctx, child.ID, raw, false)
			if name == "assembly" {
				if payload != nil || repairable || !errors.Is(err, ErrReadLimit) || !errors.Is(budget.Err(), ErrReadLimit) {
					t.Fatalf("assembly failure: payload=%+v repairable=%t error=%v budget=%v", payload, repairable, err, budget.Err())
				}
				return
			}
			if err != nil || payload == nil {
				t.Fatalf("valid assembly: %v", err)
			}
			tx, err := c.beginTaskResultSubmission(identity)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.rollback() }()
			var want error
			switch name {
			case "commit budget":
				want = ErrReadLimit
			case "commit cancelled":
				cancel()
				want = context.Canceled
			}
			result := &TaskResult{Status: TaskResultStatusSuccess, Summary: "reviewed", StructuredPayload: payload}
			if err := tx.commit(ctx, result); !errors.Is(err, want) {
				t.Fatalf("commit error=%v, want %v", err, want)
			}
			if (c.GetTaskResult(child.ID) != nil) != (want == nil) {
				t.Fatal("result publication did not follow commit outcome")
			}
		})
	}
}

func runtimeWorksetProducerFixture(t *testing.T, c *Coordinator, content []byte, sharedInputs ...PutArtifactRequest) (*TodoItem, ArtifactRef) {
	t.Helper()
	producer := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", PlanTaskID: "seed"}})[0]
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	var artifacts []ArtifactRef
	if len(sharedInputs) != 0 {
		workset, err := normalizeStructuredWorkset(content)
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range sharedInputs {
			input.RunID, input.TaskID, input.Agent, input.Attempt = c.executionRunID, producer.ID, producer.Agent, 1
			artifact, err := store.Put(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			artifacts = append(artifacts, artifact.ArtifactRef)
			for i := range workset.Items {
				workset.Items[i].Inputs = append(workset.Items[i].Inputs, artifact.ArtifactRef)
			}
		}
		content, err = json.Marshal(workset)
		if err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := store.Put(t.Context(), PutArtifactRequest{
		Content: content,
		Path:    "manifest.json", Kind: "workset_manifest", RunID: c.executionRunID, TaskID: producer.ID, Agent: producer.Agent, Attempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	producer.TypedResult = &TaskResult{TaskID: producer.ID, Agent: producer.Agent, Attempt: 1, Status: TaskResultStatusSuccess, Source: "runtime", Artifacts: append(artifacts, manifest.ArtifactRef)}
	c.storeSubmittedTaskResult(producer.ID, producer.TypedResult)
	c.taskTracker.TodoList().UpdateStatus(producer.ID, TaskDone, "produced")
	return producer, manifest.ArtifactRef
}

func TestRuntimeResultInputsBindManifestValuesForSubmissionRoutes(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"local", "external"} {
		t.Run(route, func(t *testing.T) {
			c, _, child, compiled := runtimeWorksetInputFixture(t)
			submit := func(raw string) (*ResultPayload, string) {
				if route == "local" {
					return c.structuredPayloadForSubmission(child.ID, []byte(raw))
				}
				request := AttemptRequest{Task: taskDefFromTodoItem(child), resultContractSchema: compiled,
					resultRuntimeInputs: func(spec *CompiledResultContract) (map[string]any, error) {
						return c.resultRuntimeInputValues(spec, child.ID)
					}}
				payload, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(raw)}, nil)
				if err != nil {
					return nil, err.Error()
				}
				return payload, ""
			}
			for _, value := range []string{`"frozen-value"`, `"different"`, `null`} {
				if result, rejection := submit(`{"record":{"decision":true,"identity":` + value + `}}`); result != nil || !strings.Contains(rejection, "/record/identity is runtime-owned") {
					t.Fatalf("override %s must fail runtime ownership at /record/identity: %s", value, rejection)
				}
			}
			payload, rejection := submit(`{"record":{"decision":true}}`)
			if rejection != "" || payload == nil || !bytes.Contains(payload.Value, []byte(`"identity":"frozen-value"`)) {
				t.Fatalf("canonical binding failed: %s", rejection)
			}
			for _, guidance := range []bool{false, true} {
				schema := visibleResultPayloadSchema(compiled.ref(true), compiled, guidance)
				if collectionFeedbackSchemaAt(schema, "/record")["properties"].(map[string]any)["identity"] != nil {
					t.Fatal("model schema exposes runtime-owned identity")
				}
			}
			if prompt := resultContractPromptSection(child.ResultContract, compiled, false); strings.Contains(prompt, `"identity":`) {
				t.Fatal("prompt asks the model to reconstruct identity")
			}
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var restored ResultPayload
			if err := json.Unmarshal(data, &restored); err != nil || !bytes.Equal(payload.Value, restored.Value) || c.validateAcceptedEvidenceInputs(t.Context(), child.ID, &restored) != nil {
				t.Fatal("canonical binding did not survive serialization/revalidation")
			}
			workspace := t.TempDir()
			journal, err := openTaskJournal(workspace)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			result := &TaskResult{TaskID: child.ID, Status: TaskResultStatusSuccess, StructuredPayload: payload}
			if err := journal.append(journalRecord{Op: "result", TaskID: child.ID, RunID: c.executionRunID, TypedResult: result}); err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			var exported bytes.Buffer
			if err := ExportTaskJournal(t.Context(), workspace, &exported); err != nil {
				t.Fatal(err)
			}
			var saved journalRecord
			if err := json.Unmarshal(bytes.TrimSpace(exported.Bytes()), &saved); err != nil || saved.TypedResult == nil {
				t.Fatal("journal lost the canonical result")
			}
			if !bytes.Equal(payload.Value, saved.TypedResult.StructuredPayload.Value) || payload.SHA256 != saved.TypedResult.StructuredPayload.SHA256 {
				t.Fatal("journal changed runtime-owned values or payload hash")
			}
			if err := c.taskTracker.TodoList().SetTypedResult(child.ID, saved.TypedResult); err != nil {
				t.Fatal(err)
			}
			tasks, err := json.Marshal(c.taskTracker.TodoList().Items())
			if err != nil {
				t.Fatal(err)
			}
			var checkpoint []*TodoItem
			if err := json.Unmarshal(tasks, &checkpoint); err != nil {
				t.Fatal(err)
			}
			replayed := newTestCoordinator(t, withCoordinatorWorkspace(c.session.Workspace))
			replayed.session.ResultContracts = c.session.ResultContracts
			replayed.SetSessionData(&SessionData{Tasks: checkpoint})
			if err := replayed.validateAcceptedEvidenceInputs(t.Context(), child.ID, saved.TypedResult.StructuredPayload); err != nil {
				t.Fatalf("reopened coordinator lost the frozen binding: %v", err)
			}
			child.WorksetBinding.Bindings["identity"] = "newer-value"
			if c.validateAcceptedEvidenceInputs(t.Context(), child.ID, &restored) == nil {
				t.Fatal("replay accepted a substituted binding")
			}
		})
	}
}

func TestRuntimeResultInputsPreserveOtherReferenceUses(t *testing.T) {
	t.Parallel()
	var root map[string]any
	if err := json.Unmarshal([]byte(runtimeWorksetInputSchema), &root); err != nil {
		t.Fatal(err)
	}
	root["properties"].(map[string]any)["other"] = map[string]any{"$ref": "#/$defs/record"}
	data, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := CompileResultContractDocument("shared.json", data)
	if err != nil {
		t.Fatal(err)
	}
	for _, guidance := range []bool{false, true} {
		schema := visibleResultPayloadSchema(compiled.ref(true), compiled, guidance)
		current := collectionFeedbackSchemaAt(schema, "/record")
		other := collectionFeedbackSchemaAt(schema, "/other")
		if current["properties"].(map[string]any)["identity"] != nil || other["properties"].(map[string]any)["identity"] == nil {
			t.Fatal("omission changed an unrelated reference use")
		}
	}
}

func TestRuntimeResultInputsWorksetDeclarationValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(map[string]any, map[string]any)
	}{
		{"missing_output", func(_ map[string]any, spec map[string]any) { delete(spec, "output") }},
		{"mixed_source", func(_ map[string]any, spec map[string]any) { spec["source_task"] = "producer" }},
		{"unknown_target", func(_ map[string]any, spec map[string]any) { spec["target"] = "/record/absent" }},
		{"constraint_sibling", func(root map[string]any, _ map[string]any) {
			root["properties"].(map[string]any)["record"].(map[string]any)["maxProperties"] = 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal([]byte(runtimeWorksetInputSchema), &root); err != nil {
				t.Fatal(err)
			}
			tc.edit(root, root["x-hufu-runtime-inputs"].([]any)[0].(map[string]any))
			data, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := CompileResultContractDocument("invalid.json", data); err == nil {
				t.Fatal("invalid source declaration accepted")
			}
		})
	}
}

func TestRuntimeResultInputsRejectUnverifiedWorksetValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(*testing.T, *Coordinator, *TodoItem, *TodoItem)
	}{
		{"missing_binding", func(_ *testing.T, _ *Coordinator, _ *TodoItem, child *TodoItem) { child.WorksetBinding = nil }},
		{"missing_receipt", func(_ *testing.T, _ *Coordinator, _ *TodoItem, child *TodoItem) { child.WorksetReceipt = nil }},
		{"wrong_child", func(_ *testing.T, _ *Coordinator, _ *TodoItem, child *TodoItem) {
			child.WorksetReceipt.Children[child.WorksetBinding.ItemKey] = "unrelated"
		}},
		{"stale_producer", func(t *testing.T, c *Coordinator, source *TodoItem, _ *TodoItem) {
			current := *c.GetTaskResult(source.ID)
			current.Attempt++
			if err := c.CommitTaskResetForRetry(t.Context(), source.ID, "retry producer"); err != nil {
				t.Fatal(err)
			}
			if err := c.CommitTaskTransition(t.Context(), source.ID, TaskPending, TaskInProgress, "new producer occurrence", "", attemptStartMetadata(current.Attempt)); err != nil {
				t.Fatal(err)
			}
			c.setCurrentTaskAttempt(source.ID, current.Attempt)
			c.storeSubmittedTaskResult(source.ID, &current)
		}},
		{"source_occurrence", func(_ *testing.T, _ *Coordinator, _ *TodoItem, child *TodoItem) {
			child.WorksetBinding.SourceArtifact.Attempt++
		}},
		{"changed_source", func(_ *testing.T, _ *Coordinator, _ *TodoItem, child *TodoItem) {
			child.WorksetBinding.SourceSHA256 = strings.Repeat("0", 64)
		}},
		{"changed_binding", func(_ *testing.T, _ *Coordinator, _ *TodoItem, child *TodoItem) {
			child.WorksetBinding.Bindings["identity"] = "replacement"
		}},
		{"missing_value", func(_ *testing.T, _ *Coordinator, _ *TodoItem, child *TodoItem) {
			delete(child.WorksetBinding.Bindings, "identity")
		}},
		{"stale_run", func(_ *testing.T, c *Coordinator, _ *TodoItem, _ *TodoItem) { c.executionRunID = "unrelated-run" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, source, child, _ := runtimeWorksetInputFixture(t)
			tc.edit(t, c, source, child)
			if payload, rejection := c.structuredPayloadForSubmission(child.ID, []byte(`{"record":{"decision":true}}`)); payload != nil || rejection == "" {
				t.Fatal("unverified binding was published")
			}
		})
	}
}
