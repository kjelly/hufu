package team

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/utils"
)

func TestCodexNativeResultSourcesAreRecheckedForDispatchAndReplay(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, name := range []string{"omitted payload", "populated payload"} {
		t.Run(name, func(t *testing.T) {
			testCodexNativeResultSourceRevalidation(t, name == "populated payload")
		})
	}
}

func testCodexNativeResultSourceRevalidation(t *testing.T, populated bool) {
	t.Helper()
	workspace := newCodexWorkspace(t)
	proposal := ""
	if populated {
		proposal = `"structured_payload_json":"{\"references\":[]}"`
	}
	c := newCodexE2ECoordinatorBase(t, workspace,
		[]fakeCodexStep{codexInitializeStep(t), codexThreadStartStep(t, "optional-result", workspace),
			fakeCodexTurnCompletedStep(t, "work", validProposalJSON(proposal))},
		&agent.AgentDef{Name: "worker", Role: "worker", SubagentProvider: "codex", Generation: agent.GenerationParams{Model: "gpt-5-codex"}})
	defer func() { _ = c.Close() }()
	runID := c.executionRunID
	seed, source := runtimeOutputRefFixture(t)
	c.session.RunInputDefinitions = seed.session.RunInputDefinitions
	c.SetRunInputAssignments([]RunInputAssignment{{Name: "request", RawValue: []byte(`{"count":2}`), Source: RunInputSourceCLI}})
	if err := c.resolveRunInputsForInvocation(t.Context(), "optional native payload"); err != nil {
		t.Fatal(err)
	}
	snapshot := c.RunInputSnapshot()
	source.ID = "native-producer"
	source.TypedResult.TaskID = source.ID
	source.TypedResult.RunInputSnapshotID, source.TypedResult.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	source.RunInputSnapshotID, source.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	source.ExecutionReceipt.TaskID, source.ExecutionReceipt.RunID = source.ID, runID
	source.ExecutionReceipt.RunInputSnapshotID, source.ExecutionReceipt.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	source.TypedResult.Attempt = 1
	c.taskTracker.TodoList().AddReserved([]*TodoItem{source})
	c.storeSubmittedTaskResult(source.ID, source.TypedResult)
	body, err := json.Marshal(source, collectionDurationFormat)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []EventType{EventTaskCreated, EventTaskCompleted} {
		if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(kind), BranchID: "main",
			RunID: runID, TaskID: source.ID, Actor: source.Agent, Payload: body}); err != nil {
			t.Fatal(err)
		}
	}
	schema := strings.Replace(collectionReferenceSchema, `"properties":{"references"`, `"properties":{
		"observation":{"type":"object","properties":{"count":{"type":"integer"},"literal":{"type":"string"}}},
		"references"`, 1)
	schema = strings.Replace(schema, `"x-hufu-evidence-references":`, `"x-hufu-runtime-inputs":[
		{"target":"/observation","source_task":"prepare-outputs","output":"observation"}],"x-hufu-evidence-references":`, 1)
	compiled := compileFixtureJSONSchema(t, "optional-native.json", schema)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	c.providerManager, err = agent.NewProviderManager("http://127.0.0.1:1/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.executionPolicy, err = newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ensureExecutionPolicySnapshot(); err != nil {
		t.Fatal(err)
	}
	item := admitCodexE2ETaskWithSpec(t, c, c.session.Agents["worker"], nil, func(spec *TodoSpec) {
		spec.ResultContract = new(compiled.ref(false))
		spec.RunInputSnapshotID, spec.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	})
	if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID); err != nil {
		t.Fatal(err)
	}
	accepted := c.todoItemByID(item.ID)
	if got := accepted; got.Status != TaskDone || got.TypedResult == nil || (got.TypedResult.StructuredPayload != nil) != populated {
		t.Fatalf("optional result did not settle: status=%s result=%+v failure=%+v", got.Status, got.TypedResult, got.FailureEvent)
	}
	original := c.todoItemByID(source.ID).ExecutionReceipt
	changed := *original
	changed.RuntimeOutputsHash = "changed"
	setNativeReceipt := func(receipt *ExecutionReceipt) {
		items := c.taskTracker.TodoList().Items()
		native := findTodoItem(items, source.ID)
		native.ExecutionReceipt, native.ExecutionReceipts = receipt, nil
		c.taskTracker.TodoList().Restore(items)
	}
	rawLog := filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")
	before := len(readRawCalls(t, rawLog))
	for _, corrupt := range []*ExecutionReceipt{nil, &changed} {
		setNativeReceipt(original)
		dependent := admitCodexE2ETaskWithSpec(t, c, c.session.Agents["worker"], nil, func(spec *TodoSpec) {
			spec.EvidenceFrom = []string{item.ID}
			spec.RunInputSnapshotID, spec.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
		})
		setNativeReceipt(corrupt)
		if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(dependent), dependent.ID); err == nil ||
			!strings.Contains(err.Error(), "runtime input /observation") {
			t.Fatalf("same-run dispatch did not reject the missing or changed native receipt: %v", err)
		}
	}
	if after := len(readRawCalls(t, rawLog)); before != after {
		t.Fatalf("source failure reached the Codex provider: %d calls before, %d after", before, after)
	}
	setNativeReceipt(original)
	c = reopenRecoveryCoordinator(t, c)
	c.executionRunID = runID
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	restored := ReduceToSessionData(events)
	restored.Tasks = c.taskTracker.TodoList().Items()
	c.SetSessionData(restored)
	if err := c.selectRecoveryRunInputSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	got := c.todoItemByID(item.ID)
	if got.Status != TaskDone || got.TypedResult == nil ||
		!sameCanonicalEvidence(accepted.TypedResult.StructuredPayload, got.TypedResult.StructuredPayload) {
		t.Fatalf("SQLite did not replay the optional result: status=%s result=%+v", got.Status, got.TypedResult)
	}
	if err := c.validateRequiredEvidenceDeliveryForResult(t.Context(), got.ID, got.TypedResult.Attempt, got.TypedResult, runID); err != nil {
		t.Fatalf("optional result failed replay validation: %v", err)
	}
	if err := c.validateCompletedResumeEvidence(t.Context(), got, snapshot); err != nil {
		t.Fatalf("optional result could not be reused: %v", err)
	}
	for _, corrupt := range []*ExecutionReceipt{nil, &changed} {
		setNativeReceipt(corrupt)
		if err := c.validateRequiredEvidenceDeliveryForResult(t.Context(), got.ID, got.TypedResult.Attempt, got.TypedResult, runID); err == nil {
			t.Fatal("replay accepted the missing or changed source receipt")
		}
		if err := c.validateCompletedResumeEvidence(t.Context(), got, snapshot); err == nil {
			t.Fatal("resume reused the result without its original source receipt")
		}
	}
}

func TestCodexCorrectionReservationSurvivesRestart(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	const originalWork = "written by the first turn"
	const credential = "external-correction-fixture-secret-91823"
	const originalCredential = "original-rejected-fixture-secret-81514"
	const nestedCredential = "nested-payload-fixture-secret-95267"
	for _, tc := range []struct {
		message  string
		returned bool
		exitCode int
	}{
		{"Codex result correction accepted", false, 0},
		{"Codex result correction recorded", true, 1},
	} {
		t.Run(tc.message, func(t *testing.T) {
			workspace := newCodexWorkspace(t)
			payload := fmt.Sprintf(`{"api_\u0074oken":%q,"truncated":`, nestedCredential)
			original := fmt.Sprintf(`Malformed draft: {"status":"success","summary":"","facts":{"api_\u0074oken":%q},"structured_payload_json":%q,"truncated":`, originalCredential, payload)
			correction := fmt.Sprintf(`{"status":"success","summary":"did the work api_token=%s","facts":{"api_token":%q}}`, credential, credential)
			steps := codexRepairScript(t, workspace, "restart-correction",
				original, correction,
				fakeCodexStep{WriteFile: "preserved.txt", WriteFileContent: originalWork}, fakeCodexStep{})
			worker := &agent.AgentDef{Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 2, Generation: agent.GenerationParams{Model: "gpt-5-codex"}}
			c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
			var err error
			c.providerManager, err = agent.NewProviderManager("http://127.0.0.1:1/v1", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			c.executionPolicy, err = newExecutionPolicyState(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.ensureExecutionPolicySnapshot(); err != nil {
				t.Fatal(err)
			}
			verify := &VerificationSpec{Type: VerifyCommandExit, Command: fmt.Sprintf("exit %d", tc.exitCode)}
			if tc.exitCode == 0 {
				verify.Command = fmt.Sprintf("test -f %q", filepath.Join(workspace, "preserved.txt"))
			}
			item := admitCodexE2ETask(t, c, worker, verify)
			task := taskDefFromTodoItem(item)
			if !CanAutomaticallyReplay(task) {
				t.Fatal("fixture must otherwise allow another work dispatch")
			}
			c.reportStatus = func(event StatusEvent) {
				if event.Type == "codex_activity" && event.Message == tc.message {
					panic(tc.message)
				}
			}
			func() {
				defer func() {
					if got := recover(); got != tc.message {
						t.Fatalf("did not interrupt at %q: %v", tc.message, got)
					}
				}()
				_, _ = c.executeTask(t.Context(), task, item.ID)
			}()
			c = reopenRecoveryCoordinator(t, c)
			got := c.todoItemByID(item.ID)
			if got == nil || got.Status != TaskInProgress {
				t.Fatalf("wanted an interrupted dispatch, got %+v", got)
			}
			if report, err := c.ReconcileTask(t.Context(), item.ID); err == nil ||
				!strings.Contains(err.Error(), "owned by external result settlement") || report.Status != TaskInProgress {
				t.Fatalf("targeted reconciliation bypassed the correction checkpoint: report=%+v, error=%v", report, err)
			}
			if tc.exitCode == 1 {
				// A prior not-started observation still cannot confer replay rights
				// when a durable correction reservation proves work ran.
				observed := ReconcileResult{Resolution: ResolutionNotStarted, Source: ReconcileSourceVerifySpec, ExitCode: &tc.exitCode}
				if err := c.recordReconcileObservation("operator", got, observed); err != nil {
					t.Fatal(err)
				}
				c = reopenRecoveryCoordinator(t, c)
				got = c.todoItemByID(item.ID)
				if got.RecoveryState != RecoveryStateNotStarted {
					t.Fatalf("fixture lost its earlier reconciliation: %+v", got)
				}
				if err := validateTargetedRetry(got, c.RepairController()); err != nil {
					t.Fatalf("fixture must otherwise allow explicit retry after reconciliation: %v", err)
				}
			}
			if report, err := c.RetryTask(t.Context(), item.ID); err == nil ||
				!strings.Contains(err.Error(), "owned by external result settlement") || report.Status != TaskInProgress {
				t.Fatalf("targeted retry bypassed the correction checkpoint: report=%+v, error=%v", report, err)
			}
			reservation, err := c.pendingExternalCorrection(t.Context(), got)
			if err != nil || reservation == nil {
				t.Fatalf("missing correction reservation before resume: %v", err)
			}
			if bytes.Contains(reservation.body, []byte(originalCredential)) || bytes.Contains(reservation.body, []byte(nestedCredential)) {
				t.Fatal("original rejection stored a credential from an escaped JSON key")
			}
			body, found, err := workspaceDocs(c.artifactStoreRootPath()).getDocument(reservation.key + "/response")
			if err != nil || found != tc.returned || bytes.Contains(body, []byte(credential)) {
				t.Fatalf("correction response checkpoint: found=%t contains unwanted credential=%t error=%v", found, bytes.Contains(body, []byte(credential)), err)
			}
			if count, err := c.ResumeInterruptedTasks(t.Context()); count != 1 || err != nil {
				t.Fatalf("interrupted correction was not resolved: count=%d error=%v", count, err)
			}
			c = reopenRecoveryCoordinator(t, c)
			got = c.todoItemByID(item.ID)
			if got.Status != TaskBlocked || got.TypedResult != nil || got.FailureEvent == nil ||
				got.FailureEvent.FailureClass != FailureProtocol || got.FailureEvent.RetryDisposition != ReconcileOnly ||
				got.ExecutionReceipt == nil || got.ExecutionReceipt.WorkSubmissionFailure != RepairFailureInvalidSchema {
				t.Fatalf("lost interrupted correction decision: status=%s failure=%+v receipt=%+v", got.Status, got.FailureEvent, got.ExecutionReceipt)
			}
			proof := got.ExecutionReceipt.RepairProvenance
			if proof == nil || !proof.Attempted || proof.RepairAttempts != 1 || proof.ProviderWorkTurnID != "turn-original" ||
				proof.ProviderWorkTranscriptRef == "" || proof.ProviderCorrectionTurnID != "turn-repair" || (proof.SubmittedResult != nil) != tc.returned {
				t.Fatalf("lost durable correction evidence: %+v", proof)
			}
			_, transcript := readCodexTranscriptArtifact(t, workspace, proof.ProviderWorkTranscriptRef)
			if bytes.Contains(transcript, []byte(originalCredential)) || bytes.Contains(transcript, []byte(nestedCredential)) {
				t.Fatal("original provider transcript stored credentials from truncated JSON")
			}
			if tc.returned {
				digest, err := collectionDigest(proof.SubmittedResult)
				if err != nil || digest != proof.PreparedCandidateSHA256 || !strings.Contains(proof.SubmittedResult.Summary, "did the work") ||
					strings.Contains(proof.SubmittedResult.Summary, credential) || proof.SubmittedResult.Facts["api_token"] == credential {
					t.Fatal("replayed receipt did not preserve the redacted response and its digest")
				}
			}
			if report, err := c.ReconcileTask(t.Context(), item.ID); err == nil ||
				!strings.Contains(err.Error(), "owned by external result settlement") || report.Status != TaskBlocked {
				t.Fatalf("targeted reconciliation bypassed the retained correction checkpoint: report=%+v, error=%v", report, err)
			}
			if report, err := c.RetryTask(t.Context(), item.ID); err == nil ||
				!strings.Contains(err.Error(), "owned by external result settlement") || report.Status != TaskBlocked {
				t.Fatalf("targeted retry bypassed the retained correction checkpoint: report=%+v, error=%v", report, err)
			}
			if body, err := os.ReadFile(filepath.Join(workspace, "preserved.txt")); err != nil || string(body) != originalWork {
				t.Fatalf("original work changed: error=%v", err)
			}
			methods := rawCallMethods(t, readRawCalls(t, filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")))
			if len(methods) != len(steps) {
				t.Fatalf("unexpected work or correction replay: %v", methods)
			}
		})
	}
}

func TestCodexCrossFrameDiagnosticsRedactRepeatedCredentials(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	const originalCredential = "work-opaque-fixture-value-22443"
	const correctionCredential = "correction-opaque-fixture-value-33844"
	proposal := func(credential string) string {
		framed := fmt.Sprintf(`{}[{"\u0061pi_key":%q}]`, credential)
		return fmt.Sprintf(`{"status":%q,"summary":%q,"structured_payload_json":%q}`, credential, "diagnostic "+credential, framed)
	}
	workspace := newCodexWorkspace(t)
	steps := codexRepairScript(t, workspace, "nested-secret-frames", proposal(originalCredential), proposal(correctionCredential),
		fakeCodexStep{}, fakeCodexStep{})
	worker := &agent.AgentDef{Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 2,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"}}
	c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
	provider, err := agent.NewProviderManager("http://127.0.0.1:1/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = provider
	c.executionPolicy, err = newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ensureExecutionPolicySnapshot(); err != nil {
		t.Fatal(err)
	}
	item := admitCodexE2ETask(t, c, worker, nil)
	var mu sync.Mutex
	var activities []string
	var reservationKey string
	var reservationErr error
	c.verbose = true
	c.reportStatus = func(event StatusEvent) {
		if event.Type != "codex_activity" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		activities = append(activities, event.Message)
		if event.Message == "Codex result correction accepted" {
			var reservation *externalResultCorrectionReservation
			reservation, reservationErr = c.pendingExternalCorrection(t.Context(), c.todoItemByID(item.ID))
			if reservation != nil {
				reservationKey = reservation.key
			}
		}
	}
	_, err = c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID)
	if err == nil || localResultRejectionReason(err) != RepairFailureInvalidSchema {
		t.Fatalf("wanted a failed format correction, error class=%s", localResultRejectionReason(err))
	}
	noCredential := func(sink, body string) {
		t.Helper()
		if strings.Contains(body, originalCredential) || strings.Contains(body, correctionCredential) {
			t.Fatalf("%s retained a credential", sink)
		}
	}
	noCredential("protocol error", err.Error())
	mu.Lock()
	output := strings.Join(activities, "\n")
	key, lookupErr := reservationKey, reservationErr
	mu.Unlock()
	if strings.Count(output, "Codex final response") != 2 || strings.Count(output, "diagnostic") < 2 {
		t.Fatal("both malformed responses must appear in detailed status")
	}
	noCredential("detailed status", output)
	log := filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")
	calls := readRawCalls(t, log)
	if len(calls) != len(steps) || !strings.Contains(string(calls[len(calls)-1]["params"]), "unknown status") {
		t.Fatal("expected one correction with the status diagnostic")
	}
	noCredential("correction prompt", string(calls[len(calls)-1]["params"]))
	c = reopenRecoveryCoordinator(t, c)
	got := c.todoItemByID(item.ID)
	if got == nil || got.Status != TaskBlocked || got.ExecutionReceipt == nil || got.ExecutionReceipt.RepairProvenance == nil {
		t.Fatal("SQLite did not restore the failed correction receipt")
	}
	body, err := json.Marshal(got, collectionDurationFormat)
	if err != nil {
		t.Fatal(err)
	}
	noCredential("persisted rejection", string(body))
	if key == "" || lookupErr != nil {
		t.Fatalf("missing original correction reservation: %v", lookupErr)
	}
	persisted, found, err := workspaceDocs(c.artifactStoreRootPath()).getDocument(key)
	if err != nil || !found {
		t.Fatalf("SQLite did not restore the correction reservation: %v", err)
	}
	var checkpoint externalResultCorrectionCheckpoint
	if err := json.Unmarshal(persisted, &checkpoint); err != nil ||
		checkpoint.Receipt.RepairProvenance == nil || !strings.Contains(checkpoint.Receipt.RepairProvenance.Error, "unknown status") {
		t.Fatal("persisted reservation must contain the original rejection")
	}
	noCredential("persisted reservation", string(persisted))
	for _, ref := range []string{got.ExecutionReceipt.ProviderTranscriptRef, got.ExecutionReceipt.RepairProvenance.ProviderWorkTranscriptRef} {
		_, transcript := readCodexTranscriptArtifact(t, workspace, ref)
		noCredential("persisted transcript", string(transcript))
	}
}

func TestCodexFailedTurnRedactsPriorActivityAndError(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, correction := range []bool{false, true} {
		name, stepIndex, turnID := "original", 2, "turn-original"
		if correction {
			name, stepIndex, turnID = "correction", 5, "turn-repair"
		}
		t.Run(name, func(t *testing.T) {
			credential := "opaque-terminal-" + name + "-fixture-19051"
			plaintextCredential := "opaque-command-" + name + "-fixture-29163"
			deltaCredential := "opaque-split-delta-" + name + "-fixture-39267"
			errorCredential := "opaque-error-data-" + name + "-fixture-49339"
			advisoryCredential := "opaque-error-advisory-" + name + "-fixture-59104"
			failedCredential := "opaque-failure-advisory-" + name + "-fixture-61712"
			terminalFieldCredential := "opaque-terminal-field-" + name + "-fixture-74380"
			deltaFieldCredential := "opaque-delta-field-" + name + "-fixture-86310"
			answer := fmt.Sprintf(`{"structured_payload_json":%q}`, fmt.Sprintf(`{"\u0061pi_key":%q}`, credential))
			workspace := newCodexWorkspace(t)
			steps := codexRepairScript(t, workspace, "terminal-failure-"+name,
				strings.Replace(validProposalJSON(""), `"summary":"did the work"`, `"summary":""`, 1), "",
				fakeCodexStep{}, fakeCodexStep{})
			steps = steps[:stepIndex+1]
			steps[stepIndex].Notifications = []fakeCodexNotification{
				{Method: "item/started", Params: rawJSON(t, map[string]any{"item": map[string]any{
					"type": "commandExecution", "command": "prepared command " + credential,
				}})},
				{Method: codexCommandOutputDelta, Params: rawJSON(t, map[string]any{
					"itemId": "test", "delta": strings.Join([]string{"command diagnostic", credential, plaintextCredential, terminalFieldCredential, deltaFieldCredential}, " "),
					"credential": []string{deltaFieldCredential},
				})},
				{Method: codexCommandOutputDelta, Params: rawJSON(t, map[string]string{"itemId": "split", "delta": "split diagnostic API_TOKEN="})},
				{Method: codexCommandOutputDelta, Params: rawJSON(t, map[string]string{"itemId": "test", "delta": "\ncommand finished"})},
				{Method: codexCommandOutputDelta, Params: rawJSON(t, map[string]string{"itemId": "split", "delta": deltaCredential})},
				{Method: "item/completed", Params: rawJSON(t, map[string]any{"item": map[string]any{
					"type": "agentMessage", "text": strings.Join([]string{"earlier diagnostic", credential, plaintextCredential, deltaCredential, errorCredential, advisoryCredential, failedCredential}, " "),
				}})},
				{Method: "item/completed", Params: rawJSON(t, map[string]any{"item": map[string]any{
					"type": "commandExecution", "aggregatedOutput": "keyed diagnostic API_KEY=" + plaintextCredential,
				}})},
				{Method: "error", Params: rawJSON(t, map[string]string{"api_key": advisoryCredential})},
				{Method: "turn/failed", Params: rawJSON(t, map[string]string{"api_key": failedCredential})},
				{Method: codexNotificationTurnCompleted, Params: rawJSON(t, map[string]any{
					"credentials": map[string]string{"value": terminalFieldCredential},
					"turn": map[string]any{
						"id": turnID, "status": "failed", "error": map[string]any{
							"message": strings.Join([]string{"terminal diagnostic", credential, errorCredential, advisoryCredential, failedCredential, terminalFieldCredential, deltaFieldCredential}, " "),
							"data":    map[string]string{"api_token": errorCredential},
						},
						"items": []map[string]any{{"type": "agentMessage", "phase": "final_answer", "text": answer}},
					}})},
			}
			c, provider, item, callLog := newCodexHarness(t, workspace, steps)
			c.verbose = true
			var mu sync.Mutex
			var events []string
			var churn sync.Once
			c.reportStatus = func(event StatusEvent) {
				if event.Type == "codex_activity" {
					if event.Message == "Codex reported an app-server error" {
						churn.Do(func() {
							for i := range 512 {
								_ = utils.RedactSecrets(fmt.Sprintf("API_KEY=cache-pressure-credential-%04d", i))
							}
						})
					}
					mu.Lock()
					events = append(events, event.Message)
					mu.Unlock()
				}
			}
			result, err := provider.RunAttempt(t.Context(), codexAttemptRequest(item, "do the work"))
			if err == nil || !strings.Contains(err.Error(), "terminal diagnostic") {
				t.Fatalf("terminal provider error was lost: %v", err)
			}
			mu.Lock()
			status := strings.Join(events, "\n")
			mu.Unlock()
			_, transcript := readCodexTranscriptArtifact(t, workspace, result.TranscriptRef)
			for _, want := range []string{"earlier diagnostic", "prepared command", "command diagnostic", "split diagnostic", "command finished", "keyed diagnostic"} {
				if !strings.Contains(status, want) || !bytes.Contains(transcript, []byte(want)) {
					t.Errorf("verbose status or transcript lost %s", want)
				}
			}
			if !bytes.Contains(transcript, []byte("terminal diagnostic")) {
				t.Error("transcript lost the terminal diagnostic")
			}
			for sink, body := range map[string]string{"error": err.Error(), "verbose status": status, "transcript": string(transcript), "output": result.Output} {
				for _, secret := range []string{credential, plaintextCredential, deltaCredential, errorCredential, advisoryCredential, failedCredential, terminalFieldCredential, deltaFieldCredential} {
					if strings.Contains(body, secret) {
						t.Fatalf("%s retained a diagnostic credential", sink)
					}
				}
			}
			if got := len(readRawCalls(t, callLog)); got != len(steps) {
				t.Fatalf("unexpected provider calls: %d, want %d", got, len(steps))
			}
		})
	}
}

func TestCodexResultSettlementKeepsScopedDiagnosticRedaction(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, scenario := range []string{"original proposal", "original rejected envelope", "correction proposal", "correction rejected envelope"} {
		t.Run(scenario, func(t *testing.T) {
			const originalCredential = "opaque-schema-rejection-5834"
			const sourceCredential = "opaque-grounded-path-8818"
			workspace := newCodexWorkspace(t)
			raw := strings.TrimSuffix(validProposalJSON(""), "}") + `,"files_read":["/outside/` + sourceCredential + `"]}`
			if strings.Contains(scenario, "rejected envelope") {
				raw = strings.Replace(raw, `"status":"success"`, `"status":false`, 1)
			}
			inCorrection := strings.HasPrefix(scenario, "correction")
			original, correction, stepIndex := raw, "", 2
			if inCorrection {
				original = strings.Replace(validProposalJSON(""), `"status":"success"`, `"status":"`+originalCredential+`"`, 1)
				correction, stepIndex = raw, 5
			}
			steps := codexRepairScript(t, workspace, "scope-after-turn", original, correction, fakeCodexStep{}, fakeCodexStep{})[:stepIndex+1]
			prepend := func(index int, method string, params any) {
				steps[index].Notifications = append([]fakeCodexNotification{{Method: method, Params: rawJSON(t, params)}}, steps[index].Notifications...)
			}
			// The unsupported exitCode shape must not hide the readable
			// secret definition elsewhere in the same item notification.
			prepend(stepIndex, "item/completed", map[string]any{"item": map[string]any{
				"type": "commandExecution", "exitCode": "unknown",
				"credentials": map[string]string{"api_key": sourceCredential},
			}})
			if inCorrection {
				prepend(2, "error", map[string]any{"credentials": map[string]string{"api_key": originalCredential}})
			}
			c, provider, item, callLog := newCodexHarness(t, workspace, steps)
			c.verbose = true
			request := codexAttemptRequest(item, "do the work")
			request.Task.Execution.RequiresGroundedResult = true
			var mu sync.Mutex
			var status []string
			c.reportStatus = func(event StatusEvent) {
				mu.Lock()
				defer mu.Unlock()
				status = append(status, event.Message)
			}
			result, err := provider.RunAttempt(t.Context(), request)
			if localResultRejectionReason(err) != RepairFailureSource || !strings.Contains(err.Error(), "/outside/[REDACTED]") {
				t.Fatalf("grounded source diagnostic lost its rejection class or masked path: class=%s", localResultRejectionReason(err))
			}
			_, transcript := readCodexTranscriptArtifact(t, workspace, result.TranscriptRef)
			if !bytes.Contains(transcript, []byte("/outside/[REDACTED]")) {
				t.Fatal("reopened transcript lost the masked source diagnostic")
			}
			mu.Lock()
			details := strings.Join(status, "\n")
			mu.Unlock()
			calls := readRawCalls(t, callLog)
			if len(calls) != len(steps) {
				t.Fatalf("unexpected provider call count %d", len(calls))
			}
			lastPrompt := string(calls[len(calls)-1]["params"])
			if inCorrection && (!strings.Contains(lastPrompt, "unknown status") || !strings.Contains(lastPrompt, "[REDACTED]")) {
				t.Fatal("repair prompt lost its scoped schema diagnostic")
			}
			for sink, body := range map[string]string{"error": err.Error(), "transcript": string(transcript), "status": details, "repair prompt": lastPrompt, "output": result.Output} {
				if strings.Contains(body, originalCredential) || strings.Contains(body, sourceCredential) {
					t.Fatalf("%s retained a provider credential", sink)
				}
			}
		})
	}
}

func TestCodexDurableResultUsesScopedRedaction(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, name := range []string{"original", "correction", "stderr", "verified payload conflict", "incomplete discovery", "incomplete malformed", "incomplete missing", "incomplete schema invalid", "unrelated overflow", "relevant drop"} {
		t.Run(name, func(t *testing.T) {
			const secret = "opaque-success-result-credential-72061"
			workspace := newCodexWorkspace(t)
			payload := `{"judgment":"source stayed intact"}`
			conflict, correction := name == "verified payload conflict", name == "correction"
			incomplete := strings.HasPrefix(name, "incomplete ") || name == "relevant drop"
			if conflict {
				payload = fmt.Sprintf(`{"judgment":%q}`, secret)
			}
			proposal := fmt.Sprintf(`{"status":"success","summary":%q,"details":%q,"structured_payload_json":%q}`, "result "+secret, "analysis "+secret, payload)
			original, repaired, stepCount := proposal, "", 3
			if correction {
				original, repaired, stepCount = `{"status":"invalid","summary":"fix envelope"}`, proposal, 6
			}
			switch name {
			case "incomplete malformed":
				original = `{"status":`
			case "incomplete missing":
				original = ""
			case "incomplete schema invalid":
				original = `{"status":"success","summary":"schema rejection","structured_payload_json":"{}"}`
			}
			steps := codexRepairScript(t, workspace, "masked-success", original, repaired, fakeCodexStep{}, fakeCodexStep{})[:stepCount]
			if name == "incomplete missing" {
				steps[2].Notifications[0].Params = rawJSON(t, map[string]any{
					"turn": map[string]string{"id": "turn-original", "status": "completed"},
				})
			}
			// A correction resumes the same provider thread and may repeat a
			// credential that was named only during its original work turn.
			if name == "stderr" {
				steps[2].StderrOnEOF = "shutdown api_key=" + secret + "\n"
			} else {
				steps[2].Notifications = slices.Insert(steps[2].Notifications, 0, fakeCodexNotification{
					Method: "error", Params: rawJSON(t, map[string]any{"api_key": map[string]string{"value": secret}}),
				})
			}
			switch name {
			case "incomplete discovery", "incomplete malformed", "incomplete missing", "incomplete schema invalid":
				credentials := make([]string, 300)
				for i := range credentials {
					credentials[i] = fmt.Sprintf("opaque-settlement-overflow-%04d", i)
				}
				steps[2].Notifications = slices.Insert(steps[2].Notifications, 1, fakeCodexNotification{
					Method: "error", Params: rawJSON(t, map[string]any{"credentials": credentials}),
				})
			case "unrelated overflow":
				steps[2].Notifications = slices.Insert(steps[2].Notifications, 0,
					fakeCodexNotification{Method: "account/rateLimits/updated", Params: rawJSON(t, strings.Repeat("x", codexMaxAdvisoryQueueBytes+1))},
					fakeCodexNotification{Method: codexNotificationTurnCompleted, Params: rawJSON(t, map[string]any{
						"threadId": "different-thread", "turn": map[string]string{"id": "work", "status": "completed"},
						"padding": strings.Repeat("x", resultCollectionResponseMaxBytes+1),
					})},
					fakeCodexNotification{Method: codexNotificationTurnCompleted, Params: rawJSON(t, map[string]any{
						"turn":    map[string]string{"id": "different-turn", "status": "completed"},
						"padding": strings.Repeat("x", resultCollectionResponseMaxBytes+1),
					})},
				)
			case "relevant drop":
				steps[2].Notifications = slices.Insert(steps[2].Notifications, 0, fakeCodexNotification{
					Method: "item/completed", Params: rawJSON(t, map[string]any{
						"api_key": secret, "padding": strings.Repeat("x", codexMaxAdvisoryQueueBytes+1),
					}),
				})
			}
			worker := &agent.AgentDef{Name: "worker", Role: "worker", SubagentProvider: "codex", Generation: agent.GenerationParams{Model: "gpt-5-codex"}}
			c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
			c.verbose = true
			var statusMu sync.Mutex
			var publicStatus []string
			c.reportStatus = func(event StatusEvent) {
				statusMu.Lock()
				defer statusMu.Unlock()
				publicStatus = append(publicStatus, event.Message)
			}
			compiled := compileFixtureJSONSchema(t, "scoped-success-result", `{"type":"object","required":["judgment"],"properties":{"judgment":{"type":"string"}}}`)
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			item := admitCodexE2ETaskWithSpec(t, c, worker, nil, func(spec *TodoSpec) {
				spec.ResultContract = new(compiled.ref(true))
			})
			output, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID)
			statusMu.Lock()
			statusText := strings.Join(publicStatus, "\n")
			statusMu.Unlock()
			if strings.Contains(statusText, secret) || (name == "stderr" && !strings.Contains(statusText, "result [REDACTED]")) {
				t.Fatal("public activity did not preserve its redacted result detail")
			}
			live := c.todoItemByID(item.ID)
			if conflict || incomplete {
				if err == nil || live.Status == TaskDone || live.TypedResult != nil || strings.Contains(err.Error(), secret) {
					t.Fatal("credential redaction altered or exposed verified payload bytes")
				}
				if incomplete && !strings.Contains(err.Error(), "credential discovery did not complete") {
					t.Fatalf("incomplete discovery was misclassified or sent to correction: %v", err)
				}
			} else if err != nil || live.Status != TaskDone || !strings.Contains(output, "result [REDACTED]") {
				t.Fatalf("redacted result failed successful live settlement: status=%s error=%v", live.Status, err)
			}
			if err := c.eventStore.Close(); err != nil {
				t.Fatal(err)
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
			for _, event := range events {
				if bytes.Contains(event.Payload, []byte(secret)) {
					t.Fatal("persisted event retained a scoped credential")
				}
			}
			items := ReduceToTodoList(events)
			index := slices.IndexFunc(items, func(candidate *TodoItem) bool { return candidate.ID == item.ID })
			if index < 0 || items[index].Status != live.Status || items[index].ExecutionReceipt == nil {
				t.Fatal("reopened workspace lost the result disposition or receipt")
			}
			reopened := items[index]
			_, transcript := readCodexTranscriptArtifact(t, workspace, reopened.ExecutionReceipt.ProviderTranscriptRef)
			if bytes.Contains(transcript, []byte(secret)) {
				t.Fatal("provider transcript retained a scoped credential")
			}
			if !conflict && !incomplete {
				result := reopened.TypedResult
				if result == nil || result.Summary != "result [REDACTED]" || result.Details != "analysis [REDACTED]" ||
					result.StructuredPayload == nil || string(result.StructuredPayload.Value) != payload || result.StructuredPayload.SHA256 != historicalResultDigest([]byte(payload)) {
					t.Fatal("reopened successful result lost its redacted prose or exact payload digest")
				}
				if correction {
					proof := reopened.ExecutionReceipt.RepairProvenance
					if proof == nil || proof.SubmittedResult == nil {
						t.Fatal("reopened correction lost the sealed candidate")
					}
					if digest, err := collectionDigest(proof.SubmittedResult); err != nil || digest != proof.PreparedCandidateSHA256 {
						t.Fatal("reopened correction lost the exact redacted candidate digest")
					}
				}
			}
			calls := filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")
			if len(readRawCalls(t, calls)) != stepCount {
				t.Fatal("result settlement unexpectedly requested another provider turn")
			}
		})
	}
}

func TestCodexRPCErrorRedactsCredentialFromErrorData(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for name, stepIndex := range map[string]int{"thread start stderr": 1, "original": 2, "correction": 5} {
		t.Run(name, func(t *testing.T) {
			credential := "opaque-rpc-error-" + name + "-fixture-18401"
			advisoryCredential := "opaque-rpc-advisory-" + name + "-fixture-76818"
			workspace := newCodexWorkspace(t)
			steps := codexRepairScript(t, workspace, "rpc-error-"+name, "", "", fakeCodexStep{}, fakeCodexStep{})[:stepIndex+1]
			steps[stepIndex] = fakeCodexStep{
				ErrorMessage: "provider diagnostic " + credential + " " + advisoryCredential,
				ErrorData:    rawJSON(t, map[string]string{"api_token": credential}),
				Notifications: []fakeCodexNotification{{
					Method: "error", Params: rawJSON(t, map[string]any{"credentials": map[string]string{"api_token": advisoryCredential}}),
				}},
			}
			if stepIndex == 1 {
				steps[stepIndex].Notifications = nil
				steps[stepIndex].StderrOnEOF = "shutdown api_key=" + advisoryCredential + "\n"
			}
			c, provider, item, callLog := newCodexHarness(t, workspace, steps)
			c.verbose = true
			var mu sync.Mutex
			var status []string
			c.reportStatus = func(event StatusEvent) {
				mu.Lock()
				defer mu.Unlock()
				status = append(status, event.Message)
			}
			result, err := provider.RunAttempt(t.Context(), codexAttemptRequest(item, "do the work"))
			if err == nil || !strings.Contains(err.Error(), "provider diagnostic") {
				t.Fatalf("RPC diagnostic was lost: %v", err)
			}
			_, transcript := readCodexTranscriptArtifact(t, workspace, result.TranscriptRef)
			if !bytes.Contains(transcript, []byte("provider diagnostic")) {
				t.Fatal("transcript lost the RPC diagnostic")
			}
			mu.Lock()
			statusText := strings.Join(status, "\n")
			mu.Unlock()
			for sink, body := range map[string]string{"error": err.Error(), "verbose status": statusText, "transcript": string(transcript), "output": result.Output} {
				if strings.Contains(body, credential) || strings.Contains(body, advisoryCredential) {
					t.Fatalf("%s retained a credential from the RPC reply or preceding notification", sink)
				}
			}
			if got := len(readRawCalls(t, callLog)); got != len(steps) {
				t.Fatalf("unexpected provider calls: %d, want %d", got, len(steps))
			}
		})
	}
}
