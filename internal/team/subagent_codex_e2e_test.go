package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// This file drives Codex through the FULL production dispatch path —
// Coordinator.executeTask -> SubagentRegistry().Resolve ->
// CodexSubagentProvider.RunAttempt -> coordinator_task_run.go's own
// verification/completion-gate logic — rather than calling RunAttempt
// directly the way subagent_codex_test.go's Phase 5/6 tests do. It exists
// specifically to close spec.md §37's last two mandatory-matrix gaps, both
// of which are only observable through this full path: whether a task
// reaches TaskDone or retries lives entirely in coordinator_task_run.go,
// which never runs at all when a test calls RunAttempt on its own.

// newCodexE2ECoordinatorBase builds a real, EventStore-backed Coordinator
// with "codex" registered as a SubagentProvider (via the fake app-server
// fixture), but admits no task — callers that need a durable, pre-admitted
// occurrence (coordinated dispatch) use newCodexEndToEndCoordinator below;
// callers that need RunDirectAgent to perform its own admission (the direct
// fast path) use this directly.
func newCodexE2ECoordinatorBase(t *testing.T, workspace string, steps []fakeCodexStep, worker *agent.AgentDef) *Coordinator {
	t.Helper()
	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Keep the full-dispatch harness away from the developer account. RunAttempt
	// snapshots this exact allowlisted value for preflight, locking, and process
	// launch, so the test must exercise the same child-state path as production.
	t.Setenv("CODEX_HOME", codexHome)
	childEnvironment := buildAllowlistedEnvironment([]string{"CODEX_HOME"})
	resolvedHome, err := codexHomePathFromEnvironment(childEnvironment)
	if err != nil || resolvedHome != codexHome {
		t.Fatalf("Codex E2E child CODEX_HOME = %q, want %q (err=%v)", resolvedHome, codexHome, err)
	}
	release, err := acquireCodexHomeStateLock(t.Context(), resolvedHome)
	if err != nil {
		t.Fatalf("acquire isolated Codex E2E state lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(codexHome, ".hufu-app-server.lock")); err != nil {
		release()
		t.Fatalf("isolated Codex E2E state lock was not created under %q: %v", codexHome, err)
	}
	release()
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "script.json")
	rawLogPath := filepath.Join(scriptDir, "raw_calls.log")
	// RawLogPath is what lets a second real attempt's fresh subprocess
	// continue consuming this script where the first attempt's process left
	// off, instead of restarting from step 0 — see codex_fake_server_test.go.
	data, err := json.Marshal(fakeCodexScript{Steps: steps, RawLogPath: rawLogPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeCodexServerScriptEnvVar, scriptPath)

	runID := "codex-e2e-run"
	store, err := NewEventStore(workspace, runID, "codex-e2e-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	session := &TeamSession{
		Dir: workspace, Workspace: workspace,
		Config: agent.TeamConfig{
			Name: "codex-e2e",
			SubagentProviders: map[string]agent.SubagentProviderConfig{
				"codex": {
					Type: codexAppServerProviderType, Command: []string{os.Args[0]},
					InheritEnv:     []string{fakeCodexServerScriptEnvVar, "CODEX_HOME"},
					StartupTimeout: "5s", InterruptGrace: "200ms", ShutdownGrace: "200ms",
				},
			},
		},
		Agents: map[string]*agent.AgentDef{worker.Name: worker},
	}
	return &Coordinator{
		session: session, projectDir: workspace,
		taskTracker: NewTaskTracker(), sessionData: NewSession(), sessionTime: time.Now(),
		eventStore: store, executionRunID: runID, reportStatus: func(StatusEvent) {},
	}
}

// newCodexEndToEndCoordinator additionally admits worker durably through
// admitCodexE2ETask below — so the resulting Todo is a genuine durable task
// occurrence, not an ephemeral in-memory one, ready for a direct
// c.executeTask call.
func newCodexEndToEndCoordinator(t *testing.T, workspace string, steps []fakeCodexStep, worker *agent.AgentDef, verify *VerificationSpec, grounded ...bool) (*Coordinator, *TodoItem) {
	t.Helper()
	c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
	item := admitCodexE2ETask(t, c, worker, verify, grounded...)
	return c, item
}

// admitCodexE2ETask durably admits a task the same way admitSubagentBaselineTask
// does (spec.md §37's "genuine durable task occurrence", not an ephemeral
// in-memory one), but with SideEffect explicitly workspace_write: every
// fake-server script in this file scripts a workspace-write sandbox
// (matching subagent_codex_test.go's own convention), and
// codexStartOrResumeThread fails closed on any sandbox mismatch (§14.1) — an
// admitted task whose SideEffect defaults to none/read-only would never
// reach a scripted turn at all.
func admitCodexE2ETask(t *testing.T, c *Coordinator, worker *agent.AgentDef, verify *VerificationSpec, grounded ...bool) *TodoItem {
	t.Helper()
	return admitCodexE2ETaskWithSpec(t, c, worker, verify, nil, grounded...)
}

func admitCodexE2ETaskWithSpec(t *testing.T, c *Coordinator, worker *agent.AgentDef, verify *VerificationSpec, configure func(*TodoSpec), grounded ...bool) *TodoItem {
	t.Helper()
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	resolvedModel := c.resolveAgentModel(worker, "")
	canonical, err := c.canonicalizeTaskOccurrence(TaskDef{Agent: worker.Name}, worker, resolvedModel)
	if err != nil {
		t.Fatalf("canonicalizeTaskOccurrence: %v", err)
	}
	execution := ExecutionContract{RequiresResult: true}
	if len(grounded) > 0 && grounded[0] {
		execution.RequiresGroundedResult = true
	}
	spec := TodoSpec{
		Agent: worker.Name, Desc: "baseline worker task", Goal: "baseline worker task",
		Model: canonical.Model, ModelTopology: initialTaskModelTopology(worker, canonical.Model),
		ExecutionTarget: canonical.ResolvedExecutionTarget, ExecutionTopology: canonical.ExecutionTopology,
		Source: TaskSourceCoordinator, Recovery: RecoveryRetry, SideEffect: SideEffectWorkspaceWrite,
		Execution:        execution,
		VerifySpec:       verify,
		SubagentProvider: canonical.SubagentProvider,
	}
	if configure != nil {
		configure(&spec)
	}
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatalf("taskOccurrenceProjectionFromSpec: %v", err)
	}
	if _, err := c.admitTaskOccurrence(context.Background(), projection, ids[0], 1); err != nil {
		t.Fatalf("admitTaskOccurrence: %v", err)
	}
	items, err := c.CommitTaskCreationResolved(context.Background(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatalf("CommitTaskCreationResolved: %v", err)
	}
	return items[0]
}

func TestCodexStructuredResultRepairsOnlyAuthenticatedNestedClaims(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	const inputPath = "source.go"
	producer := compileFixtureJSONSchema(t, "codex-accepted-source", `{"type":"object","properties":{"verified":{"type":"boolean"}}}`)
	schema := strings.Replace(collectionReferenceSchema, `"required":["references"]`, `"required":["references","judgment"]`, 1)
	schema = strings.Replace(schema, `"properties":{"references":`, `"properties":{"judgment":{"type":"string"},"references":`, 1)
	compiled := compileFixtureJSONSchema(t, "codex-review", schema)
	proposal := func(producerID, judgment string) string {
		raw := fmt.Sprintf(`{"references":[{"producer":%q,"paths":[%q]}],"judgment":%s}`, producerID, inputPath, judgment)
		return validProposalJSON(fmt.Sprintf(`"files_read":[%q],"structured_payload_json":%q`, inputPath, raw))
	}
	badSummary := func(body string) string { return strings.Replace(body, `"summary":"did the work"`, `"summary":""`, 1) }
	valid := proposal("producer", `"accepted"`)
	foreign := proposal("outside", "42")
	foreignAlias := strings.Replace(badSummary(foreign), "structured_payload_json", "Structured_Payload_JSON", 1)
	fencedForeign := "```json\n" + foreignAlias + "\n```"
	tooManyReads := badSummary(validProposalJSON(`"Files_Read":[` + strings.Repeat(`"source.go",`, workerResultProposalMaxFilesRead) + `"unobserved.go"]`))
	readAliases := validProposalJSON(`"files_read":["unobserved.go"],"FILES_READ":["source.go"]`)
	pathAliases := validProposalJSON(`"proposed_files":[{"path":"unobserved.go","PATH":"source.go"}]`)
	payloadAliases := strings.TrimSuffix(foreign, "}") + `,"Structured_Payload_JSON":"{\"references\":[]}"}`
	outerForgedRead := validProposalJSON(`"files_read":["unobserved.go"]`)
	foreignPayload := `{"references":[{"producer":"outside","paths":["source.go"]}]}`
	nestedFrames := validProposalJSON(fmt.Sprintf(`"structured_payload_json":%q`, `{"references":[]}`+foreignPayload))
	wrappedPayload := validProposalJSON(fmt.Sprintf(`"structured_payload_json":%q`, "[["+foreignPayload+"]]"))
	singletonPayload := validProposalJSON(fmt.Sprintf(`"structured_payload_json":%q`, `{"references":{"producer":"outside","paths":["source.go"]}}`))
	wrappedReference := validProposalJSON(fmt.Sprintf(`"structured_payload_json":%q`, `{"references":[[{"producer":"outside","paths":["source.go"]}]]}`))
	wrappedPath := validProposalJSON(fmt.Sprintf(`"structured_payload_json":%q`, `{"references":[{"producer":"producer","paths":[["unobserved.go"]]}]}`))
	wrappedRead := validProposalJSON(`"files_read":[["unobserved.go"]]`)
	wrappedArtifact := validProposalJSON(`"proposed_files":[[{"path":"unobserved.go"}]]`)
	for _, tc := range []struct {
		name, original, repair, diagnostic string
		reason, workReason                 RepairFailureReason
		turns                              int
	}{
		{"nested shape repair", proposal("producer", "42"), valid, "judgment", "", "", 2},
		{"outer shape repair", badSummary(valid), valid, "summary", "", "", 2},
		{"nested foreign source", foreign, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"outer foreign source", badSummary(foreign), valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"fenced foreign alias", fencedForeign, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"single artifact in invalid outer frame", badSummary(validProposalJSON(`"Proposed_Files":{"Path":"unobserved.go"}`)), valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"oversized readable claims", tooManyReads, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"read alias cannot replace source", readAliases, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"artifact path alias cannot replace source", pathAliases, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"payload alias cannot replace source", payloadAliases, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"unobserved artifact in valid DTO", validProposalJSON(`"proposed_files":[{"path":"unobserved.go"}]`), valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"appended outer source", valid + outerForgedRead, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"prefixed outer source", "Returned draft: " + outerForgedRead, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"array wrapped source", "[" + valid + "," + outerForgedRead + "]", valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"appended nested source", nestedFrames, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"wrapped nested source", wrappedPayload, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"singleton nested source", singletonPayload, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"wrapped reference entry", wrappedReference, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"wrapped reference path", wrappedPath, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"wrapped physical read", wrappedRead, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"wrapped physical artifact", wrappedArtifact, valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"unobserved read in valid DTO", validProposalJSON(`"files_read":["unobserved.go"]`), valid, "", RepairFailureSource, RepairFailureSource, 1},
		{"foreign source in rejected correction", badSummary(valid), badSummary(foreign), "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"foreign artifact in rejected correction", badSummary(valid), badSummary(validProposalJSON(`"Proposed_Files":{"Path":"unobserved.go"}`)), "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"read alias in correction", badSummary(valid), readAliases, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"artifact path alias in correction", badSummary(valid), pathAliases, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"payload alias in correction", badSummary(valid), payloadAliases, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"appended outer correction source", badSummary(valid), "Returned draft: " + valid + outerForgedRead, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"appended nested correction source", badSummary(valid), nestedFrames, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"wrapped nested correction source", badSummary(valid), wrappedPayload, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"singleton nested correction source", badSummary(valid), singletonPayload, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"wrapped correction reference entry", badSummary(valid), wrappedReference, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"wrapped correction reference path", badSummary(valid), wrappedPath, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"wrapped correction physical read", badSummary(valid), wrappedRead, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"wrapped correction physical artifact", badSummary(valid), wrappedArtifact, "summary", RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"correction exhausted", proposal("producer", "42"), proposal("producer", "42"), "judgment", RepairFailureInvalidSchema, RepairFailureInvalidSchema, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := newCodexWorkspace(t)
			if err := os.WriteFile(filepath.Join(workspace, inputPath), []byte("package fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			worker := &agent.AgentDef{
				Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 2,
				Generation: agent.GenerationParams{Model: "gpt-5-codex"},
			}
			steps := codexRepairScript(t, workspace, "nested-result", tc.original, tc.repair, fakeCodexStep{}, fakeCodexStep{})
			c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled, producer.ID: producer}
			appendEvidenceSiblingProducer(t, c, producer, inputPath)
			item := admitCodexE2ETaskWithSpec(t, c, worker, nil, func(spec *TodoSpec) {
				spec.ResultContract = new(compiled.ref(true))
				spec.EvidenceFrom = []string{"producer"}
			}, true)
			_, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID)
			got := c.todoItemByID(item.ID)
			if tc.reason == "" {
				if err != nil || got.Status != TaskDone || got.TypedResult == nil || got.TypedResult.StructuredPayload == nil ||
					!strings.Contains(string(got.TypedResult.StructuredPayload.Value), `"judgment":"accepted"`) {
					t.Fatalf("corrected result status=%s err=%v", got.Status, err)
				}
				receipt := got.ExecutionReceipt
				if receipt == nil || receipt.WorkSubmissionFailure != RepairFailureInvalidSchema || receipt.ProviderTurnID != "turn-repair" {
					t.Fatalf("corrected result lost its original rejection: %+v", receipt)
				}
				proof := receipt.RepairProvenance
				if proof == nil || !proof.Attempted || !proof.Success || proof.RepairAttempts != 1 || proof.ProviderWorkTurnID != "turn-original" ||
					proof.ProviderWorkTranscriptRef == "" || proof.ProviderCorrectionTurnID != "turn-repair" || proof.SubmittedResult == nil {
					t.Fatalf("corrected result lost its provenance: %+v", proof)
				}
			} else if err == nil || localResultRejectionReason(err) != tc.reason || got.Status != TaskBlocked ||
				got.ExecutionReceipt == nil || got.ExecutionReceipt.WorkSubmissionFailure != tc.workReason || got.TypedResult != nil {
				t.Fatalf("wrong blocked result status=%s reason=%s err=%v", got.Status, localResultRejectionReason(err), err)
			}
			rawLog := filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")
			calls := readRawCalls(t, rawLog)
			methods := rawCallMethods(t, calls)
			counts := make(map[string]int)
			var lastTurn json.RawMessage
			for index, method := range methods {
				counts[method]++
				if method == codexMethodTurnStart {
					lastTurn = calls[index]["params"]
				}
			}
			if counts[codexMethodTurnStart] != tc.turns || counts[codexMethodThreadStart] != 1 {
				t.Fatalf("worker replay or wrong correction count: %v", methods)
			}
			if tc.turns == 2 {
				resume := findRawCall(t, calls, codexMethodThreadResume)
				if !strings.Contains(string(resume["params"]), codexSandboxReadOnly) ||
					!strings.Contains(string(lastTurn), tc.diagnostic) {
					t.Fatalf("repair lost sandbox or validation diagnostic: %v", methods)
				}
			}
		})
	}
}

func TestCodexAuthenticatesSourcesBeforeRepairingRuntimeCompletion(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	const inputPath = "source.go"
	producer := compileFixtureJSONSchema(t, "completion-evidence", `{"type":"object","properties":{"verified":{"type":"boolean"}}}`)
	schema := strings.Replace(collectionReferenceSchema, `"properties":{"references":`,
		`"x-hufu-runtime-completion":"/completion","x-hufu-runtime-inputs":[{"target":"/completion","source_kind":"const"}],`+
			`"properties":{"completion":{"type":"object","required":["status","summary"],"properties":{"status":{"type":"string"},`+
			`"summary":{"type":"string"}},"const":{"status":"success","summary":"Verified completion"}},"references":`, 1)
	compiled := compileFixtureJSONSchema(t, "codex-native-completion", schema)
	foreignPayload := fmt.Sprintf(`"structured_payload_json":%q`, `{"references":[{"producer":"outside","paths":["source.go"]}]}`)
	for _, tc := range []struct{ name, proposal string }{
		{"unverified file with successful override", validProposalJSON(`"files_read":["unobserved.go"]`)},
		{"foreign producer with successful override", validProposalJSON(foreignPayload)},
		{"foreign producer with unsuccessful payload", `{"status":"blocked","summary":"Incomplete",` + foreignPayload + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := newCodexWorkspace(t)
			if err := os.WriteFile(filepath.Join(workspace, inputPath), []byte("package fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			steps := codexRepairScript(t, workspace, "completion-source", tc.proposal,
				`{"structured_payload_json":"{\"references\":[]}"}`, fakeCodexStep{}, fakeCodexStep{})
			worker := &agent.AgentDef{Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 2, Generation: agent.GenerationParams{Model: "gpt-5-codex"}}
			c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled, producer.ID: producer}
			appendEvidenceSiblingProducer(t, c, producer, inputPath)
			item := admitCodexE2ETaskWithSpec(t, c, worker, nil, func(spec *TodoSpec) {
				spec.ResultContract = new(compiled.ref(true))
				spec.EvidenceFrom = []string{"producer"}
			}, true)
			_, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID)
			got := c.todoItemByID(item.ID)
			if localResultRejectionReason(err) != RepairFailureSource || got.Status != TaskBlocked ||
				got.ExecutionReceipt == nil || got.ExecutionReceipt.WorkSubmissionFailure != RepairFailureSource || got.TypedResult != nil {
				t.Fatalf("unverified source became correctable: status=%s error=%v", got.Status, err)
			}
			if proof := got.ExecutionReceipt.RepairProvenance; proof != nil && proof.Attempted {
				t.Fatal("source failure consumed a correction allowance")
			}
			methods := rawCallMethods(t, readRawCalls(t, filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")))
			if len(methods) != 3 || methods[2] != codexMethodTurnStart {
				t.Fatalf("source failure opened another provider turn: %v", methods)
			}
		})
	}
}

func TestCodexContextualResultShapeRequiresVerifiedSources(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	const invariants = `"invariant_assessments":[]`
	const payload = `"structured_payload_json":"{}"`
	const unknownRead = `"files_read":["unobserved.go"]`
	for _, tc := range []struct {
		name, original, correction string
		reason, originalReason     RepairFailureReason
		turns                      int
	}{
		{"invariants without verification", invariants, "", "", RepairFailureInvalidSchema, 2},
		{"payload without contract", payload, "", "", RepairFailureInvalidSchema, 2},
		{"empty payload without contract", `"structured_payload_json":""`, "", "", RepairFailureInvalidSchema, 2},
		{"invariants and unverified read", invariants + "," + unknownRead, "", RepairFailureSource, RepairFailureSource, 1},
		{"payload and unverified read", payload + "," + unknownRead, "", RepairFailureSource, RepairFailureSource, 1},
		{"unverified read in correction", payload, invariants + "," + unknownRead, RepairFailureSource, RepairFailureInvalidSchema, 2},
		{"contextual rejection in both turns", invariants, payload, RepairFailureInvalidSchema, RepairFailureInvalidSchema, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := newCodexWorkspace(t)
			steps := codexRepairScript(t, workspace, "contextual-shape", validProposalJSON(tc.original),
				validProposalJSON(tc.correction), fakeCodexStep{}, fakeCodexStep{})
			worker := &agent.AgentDef{Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 2,
				Generation: agent.GenerationParams{Model: "gpt-5-codex"}}
			c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
			item := admitCodexE2ETaskWithSpec(t, c, worker, nil, func(*TodoSpec) {}, true)
			_, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID)
			got := c.todoItemByID(item.ID)
			if tc.reason == "" {
				if err != nil || got.Status != TaskDone || got.TypedResult == nil {
					t.Fatalf("result correction failed: status=%s error=%v", got.Status, err)
				}
			} else if err == nil || localResultRejectionReason(err) != tc.reason || got.Status != TaskBlocked || got.TypedResult != nil {
				t.Fatalf("unexpected rejection: status=%s error=%v", got.Status, err)
			}
			if got.ExecutionReceipt == nil || got.ExecutionReceipt.WorkSubmissionFailure != tc.originalReason {
				t.Fatalf("missing classified work receipt: %+v", got.ExecutionReceipt)
			}
			if proof := got.ExecutionReceipt.RepairProvenance; tc.turns == 2 && (proof == nil || !proof.Attempted || proof.RepairAttempts != 1) {
				t.Fatalf("unexpected correction evidence: %+v", proof)
			}
			rawLog := filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")
			methods := rawCallMethods(t, readRawCalls(t, rawLog))
			var turns, workStarts int
			for _, method := range methods {
				switch method {
				case codexMethodTurnStart:
					turns++
				case codexMethodThreadStart:
					workStarts++
				}
			}
			if turns != tc.turns || workStarts != 1 {
				t.Fatalf("unexpected work replay or correction count: %v", methods)
			}
		})
	}
}

func TestCodexRepairsOnlyTheResultEnvelope(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	const credential = "fake-live-correction-credential-82103"
	withPayload := compileFixtureJSONSchema(t, "external-envelope", resultEnvelopeTestSchema)
	withoutPayload := compileFixtureJSONSchema(t, "external-required-envelope", `{"type":"object","x-hufu-result-envelope":{"type":"object","properties":{"structured_payload":{"type":"object"}}}}`)
	for _, tc := range []struct {
		name, original, correction string
		contract                   *CompiledResultContract
		requirePayload             bool
		status                     string
	}{
		{"status disagrees with payload", validProposalJSON(`"structured_payload_json":"{\"decisions\":[{\"verdict\":\"unverified\"}]}"`),
			`{"status":"completed_with_gaps","summary":"did the work","structured_payload_json":"{\"decisions\":[{\"verdict\":\"unverified\"}]}"}`,
			withPayload, true, TaskResultStatusCompletedWithGaps},
		{"optional payload required by envelope", validProposalJSON(""), validProposalJSON(`"structured_payload_json":"{}"`), withoutPayload, false, TaskResultStatusSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := newCodexWorkspace(t)
			correction := strings.TrimSuffix(tc.correction, "}") + fmt.Sprintf(`,"details":"api_token=%s","facts":{"api_token":%q}}`, credential, credential)
			steps := codexRepairScript(t, workspace, "external-envelope", tc.original, correction, fakeCodexStep{}, fakeCodexStep{})
			worker := &agent.AgentDef{Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 2, Generation: agent.GenerationParams{Model: "gpt-5-codex"}}
			c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
			c.session.ResultContracts = map[string]*CompiledResultContract{tc.contract.ID: tc.contract}
			item := admitCodexE2ETaskWithSpec(t, c, worker, nil, func(spec *TodoSpec) {
				spec.ResultContract = new(tc.contract.ref(tc.requirePayload))
			})
			_, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID)
			got := c.todoItemByID(item.ID)
			if err != nil || got.Status != TaskDone || got.TypedResult == nil || got.TypedResult.Status != tc.status {
				t.Fatalf("envelope was not corrected: status=%s error=%v", got.Status, err)
			}
			proof := got.ExecutionReceipt.RepairProvenance
			if proof == nil || proof.SubmittedResult == nil || got.TypedResult.Facts["api_token"] == credential ||
				strings.Contains(got.TypedResult.Details, credential) || proof.SubmittedResult.Details != got.TypedResult.Details {
				t.Fatal("live handoff did not use the redacted correction candidate")
			}
			if digest, err := collectionDigest(proof.SubmittedResult); err != nil || digest != proof.PreparedCandidateSHA256 {
				t.Fatal("live receipt did not retain the digest of its redacted candidate")
			}
			calls := readRawCalls(t, filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log"))
			resume := findRawCall(t, calls, codexMethodThreadResume)
			if methods := rawCallMethods(t, calls); len(methods) != len(steps) || !strings.Contains(string(resume["params"]), codexSandboxReadOnly) {
				t.Fatalf("expected one read-only correction in the same session, calls=%v", methods)
			}
		})
	}
}

func TestCodexPersistsRepairInterruptionWithoutRedispatch(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	compiled := compileFixtureJSONSchema(t, "external-repair-shape", `{"type":"object","properties":{"judgment":{"type":"string"}}}`)
	for _, tc := range []struct {
		name   string
		class  TaskFailureClass
		status TaskStatus
		cause  error
	}{
		{"cancelled", FailureCancelled, TaskError, context.Canceled},
		{"resume refused", FailureProtocol, TaskBlocked, nil},
		{"initialization timed out", FailureTimeout, TaskBlocked, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := newCodexWorkspace(t)
			steps := codexRepairScript(t, workspace, "interrupted-repair",
				validProposalJSON(`"structured_payload_json":"{\"judgment\":42}"`), validProposalJSON(""), fakeCodexStep{}, fakeCodexStep{})
			switch tc.class {
			case FailureCancelled:
				steps[5] = fakeCodexStep{NoResponse: true}
			case FailureProtocol:
				steps[4] = fakeCodexStep{ErrorMessage: "cannot resume thread"}
			case FailureTimeout:
				steps[3] = fakeCodexStep{NoResponse: true}
			}
			worker := &agent.AgentDef{Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 2, Generation: agent.GenerationParams{Model: "gpt-5-codex"}}
			c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
			if tc.class == FailureTimeout {
				providers := c.session.Config.SubagentProviders
				provider := providers["codex"]
				provider.StartupTimeout = "1s"
				providers["codex"] = provider
			}
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			item := admitCodexE2ETaskWithSpec(t, c, worker, nil, func(spec *TodoSpec) {
				spec.ResultContract = new(compiled.ref(true))
			})
			logPath := filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.class == FailureCancelled {
				go func() {
					for {
						if raw, err := os.ReadFile(logPath); err == nil && strings.Count(string(raw), `"method":"turn/start"`) == 2 {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(5 * time.Millisecond):
						}
					}
				}()
			}
			_, err := c.executeTask(ctx, taskDefFromTodoItem(item), item.ID)
			got := c.todoItemByID(item.ID)
			if err == nil || tc.cause != nil && !errors.Is(err, tc.cause) || got.Status != tc.status ||
				got.FailureEvent == nil || got.FailureEvent.FailureClass != tc.class || got.TypedResult != nil {
				t.Fatalf("lost correction outcome: status=%s event=%+v error=%v", got.Status, got.FailureEvent, err)
			}
			receipt := got.ExecutionReceipt
			if receipt == nil || receipt.WorkSubmissionFailure != RepairFailureInvalidSchema || receipt.RepairProvenance == nil ||
				!receipt.RepairProvenance.Attempted || receipt.RepairProvenance.FailureReason != "" ||
				receipt.ProviderTurnID != "turn-original" || receipt.RepairProvenance.ProviderWorkTurnID != "turn-original" {
				t.Fatalf("original schema failure and interrupted correction conflated: %+v", receipt)
			}
			store, err := OpenEventStoreReadOnly(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			items := ReduceToTodoList(events)
			index := slices.IndexFunc(items, func(candidate *TodoItem) bool { return candidate.ID == item.ID })
			if index < 0 || items[index].FailureEvent == nil || items[index].FailureEvent.FailureClass != tc.class ||
				items[index].ExecutionReceipt == nil || items[index].ExecutionReceipt.WorkSubmissionFailure != RepairFailureInvalidSchema ||
				items[index].ExecutionReceipt.RepairProvenance == nil || !items[index].ExecutionReceipt.RepairProvenance.Attempted ||
				items[index].ExecutionReceipt.RepairProvenance.FailureReason != "" {
				t.Fatalf("SQLite replay lost the actual correction outcome: %v", items)
			}
			methods := rawCallMethods(t, readRawCalls(t, logPath))
			if strings.Count(strings.Join(methods, " "), codexMethodThreadStart) != 1 {
				t.Fatalf("interrupted correction redispatched the worker: %v", methods)
			}
		})
	}
}

// TestCodexSuccessStillNeedsCompletionGate proves §21/§40: even when Codex's
// own proposal claims success, a task only reaches TaskDone if it also
// passes Hufu's own objective verification — Codex's self-report is never a
// shortcut past the completion gate. A single attempt is enough to prove
// this (MaxRetries: 0), since the point is that Hufu's own gate ran at all,
// not what happens after it fails.
func TestCodexSuccessStillNeedsCompletionGate(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 0,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	verify := &VerificationSpec{Type: VerifyCommandExit, Command: "false"}
	c, item := newCodexEndToEndCoordinator(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-gate", workspace),
		fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
	}, worker, verify)

	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true}, VerifySpec: verify,
	}
	if _, err := c.executeTask(context.Background(), task, item.ID); err == nil {
		t.Fatal("expected executeTask to fail when Hufu's own verification rejects a Codex-reported success")
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got == TaskDone {
		t.Fatalf("task status = %s, want anything but done — Codex's self-reported success must not bypass Hufu's verify gate", got)
	}
}

// TestCodexRejectedProposalPersistsFailureEvidence proves the full
// coordinator path retains a rejected provider response as bounded diagnostic
// evidence. The proposal is never accepted as a TaskResult, but its output
// reaches the Hufu task transcript, failure projection, and the distinct
// provider transcript reference on the terminal receipt.
func TestCodexRejectedProposalPersistsFailureEvidence(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 0,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	c, item := newCodexEndToEndCoordinator(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-rejected-e2e", workspace),
		fakeCodexTurnCompletedStep(t, "turn-rejected-e2e", validProposalJSON(`"files_read":["unauthorized.txt"]`)),
	}, worker, nil, true)

	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true, RequiresGroundedResult: true},
	}
	_, runErr := c.executeTask(context.Background(), task, item.ID)
	if runErr == nil {
		t.Fatal("expected executeTask to reject the unauthorized files_read proposal")
	}
	got := c.todoItemByID(item.ID)
	if got.Status == TaskDone {
		t.Fatal("rejected provider proposal must not make the task done")
	}
	if !strings.Contains(got.Output, "unauthorized.txt") {
		t.Fatalf("executeTask err=%v, todo output = %q, want the rejected provider response retained as evidence", runErr, got.Output)
	}
	if got.ExecutionReceipt == nil {
		t.Fatal("expected a terminal execution receipt")
	}
	if got.ExecutionReceipt.ProviderTranscriptRef == "" {
		t.Fatal("provider transcript reference was dropped from the terminal receipt")
	}
	providerTranscript, providerTranscriptData := readCodexTranscriptArtifact(t, workspace, got.ExecutionReceipt.ProviderTranscriptRef)
	if providerTranscript.RunID != got.ExecutionReceipt.RunID || providerTranscript.TaskID != got.ExecutionReceipt.TaskID || providerTranscript.Attempt != got.ExecutionReceipt.Attempt {
		t.Fatalf("provider transcript ref = %#v, want the terminal receipt's exact run/task/attempt scope", providerTranscript)
	}
	if strings.Contains(string(providerTranscriptData), "super-secret") {
		t.Fatalf("provider transcript leaked a secret: %s", providerTranscriptData)
	}
	receiptJSON, marshalErr := json.Marshal(got.ExecutionReceipt)
	if marshalErr != nil {
		t.Fatalf("marshal execution receipt: %v", marshalErr)
	}
	if strings.Contains(string(receiptJSON), "super-secret") {
		t.Fatalf("execution receipt leaked provider transcript content: %s", receiptJSON)
	}
	if got.ExecutionReceipt.TranscriptRef == "" {
		t.Fatal("expected the Hufu task transcript reference")
	}
	transcript := readStoredArtifact(t, workspace, got.ExecutionReceipt.TranscriptRef)
	if !strings.Contains(string(transcript), "unauthorized.txt") {
		t.Fatalf("task transcript = %s, want rejected provider output", transcript)
	}
}

// TestFailedCodexVerificationRetriesThroughHufu proves §22.1/§37: when a
// Codex-driven attempt fails Hufu's own verification, the decision to retry
// — and the retry itself — is Hufu's, not Codex's. The fake server script
// spans two real, separate app-server processes (one per attempt, per
// §13.1); the second must resume the same durable thread rather than start
// fresh (§22.2), and only the retry's own passing verification result must
// reach TaskDone.
func TestFailedCodexVerificationRetriesThroughHufu(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 1,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	verify := &VerificationSpec{
		Type: VerifyTaskResultAssert,
		TaskResultAssertions: []agent.TaskResultAssertion{
			{Pointer: "/summary", Op: "equals", Value: "RETRY_OK"},
		},
	}
	firstAttempt := fakeCodexTurnCompletedStep(t, "turn-1", `{"status":"success","summary":"attempt one, still wrong"}`)
	secondAttempt := fakeCodexTurnCompletedStep(t, "turn-2", `{"status":"success","summary":"RETRY_OK"}`)
	c, item := newCodexEndToEndCoordinator(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-retry", workspace),
		firstAttempt,
		codexInitializeStep(t),
		codexThreadResumeStep(t, "thread-retry", workspace, "workspace-write"),
		secondAttempt,
	}, worker, verify)

	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true}, VerifySpec: verify,
	}
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskDone {
		t.Fatalf("task status = %s, want done after the retry's own passing result", got)
	}
	if got := c.todoItemByID(item.ID).ProviderBinding; got == nil || got.SessionID != "thread-retry" {
		t.Fatalf("durable ProviderBinding = %#v, want the same thread-retry session reused across the retry", got)
	}
}

// TestCodexPartialStatusStaysIncompleteWithoutVerifySpec proves §21's own
// example directly: "Codex says partial + tests pass -> task remains
// incomplete because provider did not claim terminal completion." With no
// task.VerifySpec/Verify at all — so the only thing that can catch this is
// Hufu's own completion-gate classification of the canonicalized result,
// not an independent verifier — a Codex proposal reporting "partial" must
// still not let the task reach TaskDone on that attempt; Hufu retries on
// its own authority instead. This is the scenario isSubmittedResultSource
// exists for: without recognizing ExternalProviderProposalSource as a
// genuine structured handoff, validateCompletedTaskResult's non-terminal-
// status check never runs for a Codex-driven attempt at all, and a
// "partial" would have silently reached TaskDone on the first attempt.
func TestCodexPartialStatusStaysIncompleteWithoutVerifySpec(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 1,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	firstAttempt := fakeCodexTurnCompletedStep(t, "turn-1", `{"status":"partial","summary":"not done yet"}`)
	secondAttempt := fakeCodexTurnCompletedStep(t, "turn-2", `{"status":"success","summary":"done now"}`)
	c, item := newCodexEndToEndCoordinator(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-partial", workspace),
		firstAttempt,
		codexInitializeStep(t),
		codexThreadResumeStep(t, "thread-partial", workspace, "workspace-write"),
		secondAttempt,
	}, worker, nil)

	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true},
	}
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskDone {
		t.Fatalf("task status = %s, want done after the retry reported success", got)
	}
	if got := c.GetTaskResult(item.ID); got == nil || got.Summary != "done now" {
		t.Fatalf("final TaskResult = %#v, want the retry's own successful result, not attempt 1's partial one", got)
	}
}

// TestDirectCodexWorkerPreservesNormalTaskSemantics is spec.md §36 Phase 7
// PR-15's named test: a Codex-backed worker invoked through the direct fast
// path (RunDirectAgent) must exhibit the same observable semantics — durable
// provider binding, canonicalized result, receipt provider identity (§20) —
// as the same worker dispatched through the normal coordinator task path.
// It does, because RunDirectAgent now escalates a non-hufu-local provider to
// the exact same executeTask dispatch (§28); this test drives a real fake
// app-server subprocess through that escalated path to prove it end to end,
// not just that the escalation branch is reached.
func TestDirectCodexWorkerPreservesNormalTaskSemantics(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", SideEffect: "workspace_write",
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	c := newCodexE2ECoordinatorBase(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-direct", workspace),
		fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
	}, worker)

	result, err := c.RunDirectAgent(context.Background(), "worker", "do the work")
	if err != nil {
		t.Fatalf("RunDirectAgent: %v", err)
	}
	if result == nil || result.AgentName != "worker" {
		t.Fatalf("RunDirectAgent result = %#v", result)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("todo items = %#v, want exactly one durable task occurrence", items)
	}
	item := items[0]
	if item.Status != TaskDone {
		t.Fatalf("task status = %s, want done", item.Status)
	}
	if item.ProviderBinding == nil || item.ProviderBinding.SessionID != "thread-direct" {
		t.Fatalf("ProviderBinding = %#v, want the durable Codex session — same as the coordinated path persists (§7.4)", item.ProviderBinding)
	}
	typedRes := c.GetTaskResult(item.ID)
	if typedRes == nil || typedRes.Source != ExternalProviderProposalSource || typedRes.Status != TaskResultStatusSuccess {
		t.Fatalf("TaskResult = %#v, want the canonicalized external-provider result (§9.3), same as the coordinated path produces", typedRes)
	}
	if item.ExecutionReceipt == nil || item.ExecutionReceipt.SubagentProvider != "codex" || item.ExecutionReceipt.ProviderSessionID != "thread-direct" {
		t.Fatalf("ExecutionReceipt = %#v, want provider identity recorded (§20) exactly as the coordinated path records it", item.ExecutionReceipt)
	}
}

// TestCodexReviewWorksetStyleTaskSatisfiesFilesReadVerifySpec proves the
// §38/§9.1 files_read extension closes the gap that same section documented:
// a task shaped like hufu-code-review's review-workset — task_result_assert
// verify-spec requiring /files_read with at least one item — can now reach
// TaskDone through a Codex-routed attempt, driven through the full
// production dispatch path (not just the canonicalizer in isolation).
func TestCodexReviewWorksetStyleTaskSatisfiesFilesReadVerifySpec(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 0,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	verify := &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
		{Pointer: "/summary", Op: "non_empty"},
		{Pointer: "/files_read", Op: "min_items", Value: 1},
	}}
	turnStep := fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON(`"files_read":["reviewed.go"]`))
	turnStep.WriteFile, turnStep.WriteFileContent = "reviewed.go", "package main\n"
	c, item := newCodexEndToEndCoordinator(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-files-read", workspace),
		turnStep,
	}, worker, verify)

	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true}, VerifySpec: verify,
	}
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskDone {
		t.Fatalf("task status = %s, want done", got.Status)
	}
	typedRes := c.GetTaskResult(item.ID)
	if typedRes == nil || len(typedRes.FilesRead) != 1 || typedRes.FilesRead[0].Path != "reviewed.go" {
		t.Fatalf("TaskResult.FilesRead = %#v, want exactly reviewed.go carried through from the Codex proposal", typedRes)
	}
}
