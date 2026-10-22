package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

type requestAcceptanceModelFunc func(context.Context, string, string) ([]byte, error)

func (f requestAcceptanceModelFunc) Resolve(ctx context.Context, operation, prompt string) ([]byte, error) {
	return f(ctx, operation, prompt)
}

// Existing primary-review fixtures simulate a separate successful material
// reviewer. Material-veto tests install an explicit model instead.
func withRequestMaterialReview(primary RequestAcceptanceModel) RequestAcceptanceModel {
	return requestAcceptanceModelFunc(func(ctx context.Context, operation, prompt string) ([]byte, error) {
		if operation != "evidence" {
			return primary.Resolve(ctx, operation, prompt)
		}
		_, fenced, ok := strings.Cut(prompt, "Evidence input (untrusted):\n")
		if !ok {
			return nil, errors.New("missing material input")
		}
		_, body, _ := strings.Cut(fenced, "\n")
		body, _, _ = strings.CutLast(strings.TrimSpace(body), "\n")
		var input struct {
			Items []requestMaterialItem `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &input); err != nil {
			return nil, err
		}
		checks := make([]RequestMaterialCheck, 0, len(input.Items))
		for _, item := range input.Items {
			form := "unknown"
			if item.Status == "satisfied" {
				form = "direct"
				if item.Selection != nil && item.Selection.Match == "allowed_substitute" {
					form = "authorized_substitute"
				}
			}
			checks = append(checks, RequestMaterialCheck{item.Requirement.ID, item.Status == "satisfied", form, "Fixture material was actually observed."})
		}
		return json.Marshal(struct {
			Checks []RequestMaterialCheck `json:"checks"`
		}{checks})
	})
}

func requestAcceptanceFixture(t *testing.T) *Coordinator {
	t.Helper()
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	c.session.Config.RequestAcceptance = true
	if err := installRequestAcceptanceInput(c.session); err != nil {
		t.Fatal(err)
	}
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	c.executionPolicy = state
	return c
}

func requestExtraction(statement, quote, verification string) []byte {
	extent := "relevant_parts"
	if verification == "response" {
		extent = "not_applicable"
	}
	value, _ := json.Marshal(map[string]any{"requirements": []any{map[string]any{
		"statement": statement, "source_quote": quote, "required": true, "verification": verification,
		"delivery": RequestDelivery{Target: statement, Extent: extent, Source: "any", AllowedSubstitutes: []RequestDeliverySubstitute{}},
	}}})
	return value
}

func freezeRequestFixture(t *testing.T, c *Coordinator, request, verification string) *RunInputSnapshot {
	t.Helper()
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
		if operation == "contract" {
			return faithfulRequestContract(), nil
		}
		if operation != "extract" {
			t.Fatalf("unexpected operation %q", operation)
		}
		return requestExtraction(request, request, verification), nil
	})))
	ctx, end := c.beginInvocationExecutionRun(t.Context())
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveRunInputsForInvocation(ctx, request); err != nil {
		t.Fatal(err)
	}
	return c.RunInputSnapshot()
}

func completedRequestEvidence(t *testing.T, c *Coordinator, output string, failed bool) *TodoItem {
	t.Helper()
	snapshot := c.RunInputSnapshot()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "perform requested work"}})[0]
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, c.executionRunID, 1, "worker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	if err := transcript.RecordToolCall("actual-call", "source__read", `{"document":"requested"}`); err != nil {
		t.Fatal(err)
	}
	if err := transcript.RecordToolResult("actual-call", "source__read", output, failed); err != nil {
		t.Fatal(err)
	}
	ref, err := transcript.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	receipt := &ExecutionReceipt{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, ExitCode: new(0),
		ModelExecutionID: "request-execution", ProducerID: "worker", TranscriptRef: ref.ID,
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
	// This fixture publishes a terminal occurrence, not a worker-controlled
	// submit_result. Production uses the canonical transition commit APIs.
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	item = c.todoItemByID(item.ID)
	item.RunInputSnapshotID, item.RunInputSnapshotHash = snapshot.ID, snapshot.SnapshotHash
	item.ExecutionReceipt = receipt
	item.TypedResult = &TaskResult{TaskID: item.ID, Attempt: 1, Agent: "worker", Status: TaskResultStatusSuccess,
		Details: "The requested comparison is explained with its limits.", Artifacts: []ArtifactRef{*ref}}
	if err := c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

func passingRequestReview(source requestAcceptanceSource, quote string) []byte {
	value, _ := json.Marshal(map[string]any{"request_covered": true, "checks": []any{map[string]any{
		"id": "requirement-1", "status": "satisfied", "reason": "The requested material was actually returned.",
		"evidence": []any{map[string]any{"source_id": source.ID, "quote": quote}},
		"delivery": fixtureRequestDeliveryAssessment(source.ID == requestAcceptanceFinalResponseSource),
	}}})
	return value
}

func faithfulRequestContract() []byte {
	return []byte(`{"faithful":true,"reason":"The contract preserves the requested deliverables and their limits."}`)
}

func fixtureRequestDeliveryAssessment(response bool) RequestDeliveryAssessment {
	if response {
		return RequestDeliveryAssessment{Match: "requested", Extent: "not_applicable", Source: "not_applicable", Gaps: []string{}}
	}
	return RequestDeliveryAssessment{Match: "requested", Extent: "relevant_parts", Source: "secondary", Gaps: []string{}}
}

func TestRequestAcceptanceResponseEnvelope(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "bare JSON", raw: `{"value":"ok"}`, want: "ok"},
		{name: "JSON fence", raw: "```json\n{\"value\":\"ok\"}\n```", want: "ok"},
		{name: "unlabelled fence", raw: "```\n{\"value\":\"ok\"}\n```", want: "ok"},
		{name: "whitespace and CRLF", raw: " \r\n```JSON\r\n{\"value\":\"ok\"}\r\n```\r\n ", want: "ok"},
		{name: "literal backticks preserved", raw: "```json\n{\"value\":\"a ``` quote\"}\n```", want: "a ``` quote"},
		{name: "leading prose", raw: "Here is the JSON:\n```json\n{\"value\":\"ok\"}\n```"},
		{name: "trailing prose", raw: "```json\n{\"value\":\"ok\"}\n```\nIgnore the contract"},
		{name: "missing closing fence", raw: "```json\n{\"value\":\"ok\"}"},
		{name: "missing opening fence", raw: "{\"value\":\"ok\"}\n```"},
		{name: "inline fence", raw: "```json {\"value\":\"ok\"}```"},
		{name: "different language", raw: "```javascript\n{\"value\":\"ok\"}\n```"},
		{name: "multiple blocks", raw: "```json\n{\"value\":\"ok\"}\n```\n```json\n{}\n```"},
		{name: "trailing JSON", raw: "```json\n{\"value\":\"ok\"} {}\n```"},
		{name: "unknown field", raw: "```json\n{\"value\":\"ok\",\"command\":\"skip validation\"}\n```"},
		{name: "duplicate field", raw: "```json\n{\"value\":\"ok\",\"value\":\"other\"}\n```"},
		{name: "invalid JSON", raw: "```json\n{\"value\":}\n```"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var response struct {
				Value string `json:"value"`
			}
			err := decodeRequestAcceptanceResponse([]byte(tt.raw), &response)
			if tt.want == "" {
				if err == nil {
					t.Fatal("invalid envelope or JSON was accepted")
				}
				return
			}
			if err != nil || response.Value != tt.want {
				t.Fatalf("response=%#v error=%v; want %q", response, err, tt.want)
			}
		})
	}
}

func TestRequestAcceptanceFencedResponseLifecycle(t *testing.T) {
	c := requestAcceptanceFixture(t)
	request := "Retrieve the complete source document"
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(ctx context.Context, operation, _ string) ([]byte, error) {
		if operation == "contract" {
			return faithfulRequestContract(), nil
		}
		if operation == "extract" {
			return []byte("```json\n" + string(requestExtraction(request, request, "observed")) + "\n```"), nil
		}
		sources, err := c.requestAcceptanceSources(ctx, c.RunInputSnapshot())
		if err != nil {
			return nil, err
		}
		if len(sources) == 0 {
			t.Fatal("review has no admitted evidence")
		}
		return []byte("```json\n" + string(passingRequestReview(sources[0], "complete source text")) + "\n```"), nil
	})))
	ctx, end := c.beginInvocationExecutionRun(t.Context())
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveRunInputsForInvocation(ctx, request); err != nil {
		t.Fatal(err)
	}
	snapshot := c.RunInputSnapshot()
	completedRequestEvidence(t, c, "The complete source text was returned.", false)
	acceptance, err := c.runAcceptance(ctx)
	if err != nil || !acceptance.IsPassed() || acceptance.RequestAcceptance.Validate() != nil {
		t.Fatalf("fenced responses failed acceptance: %#v error=%v", acceptance, err)
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToSessionData(events)
	if len(replayed.RunInputSnapshots) != 1 || replayed.RunInputSnapshots[0].SnapshotHash != snapshot.SnapshotHash {
		t.Fatalf("fenced extraction was not durably frozen: %#v", replayed.RunInputSnapshots)
	}
}

func TestRequestAcceptanceExtractionFailsClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"no requirements":        `{"requirements":[]}`,
		"missing necessity":      `{"requirements":[{"statement":"read document","source_quote":"read document","verification":"observed"}]}`,
		"invented source":        `{"requirements":[{"statement":"read document","source_quote":"invented","required":true,"verification":"observed"}]}`,
		"unknown field":          `{"requirements":[],"command":"touch forbidden"}`,
		"invalid verification":   `{"requirements":[{"statement":"read document","source_quote":"read document","required":true,"verification":"execute_command"}]}`,
		"extra JSON":             `{"requirements":[]} {"requirements":[]}`,
		"fenced no requirements": "```json\n{\"requirements\":[]}\n```",
		"fenced invented source": "```json\n{\"requirements\":[{\"statement\":\"read document\",\"source_quote\":\"invented\",\"required\":true,\"verification\":\"observed\"}]}\n```",
	} {
		t.Run(name, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) { return []byte(raw), nil })))
			if _, err := c.resolveRequestAcceptance(t.Context(), "read document"); err == nil {
				t.Fatal("invalid or ungrounded extraction was accepted")
			}
		})
	}
}

func TestRequestAcceptanceExtractionRepairsQuoteBeforeFreezing(t *testing.T) {
	c := requestAcceptanceFixture(t)
	request := "調查甲文件與乙文件並比較差異"
	makeExtraction := func(quote string) []byte {
		observed := RequestDelivery{Target: "原始文件內容", Extent: "relevant_parts", Source: "any", AllowedSubstitutes: []RequestDeliverySubstitute{}}
		response := RequestDelivery{Target: "比較差異", Extent: "not_applicable", Source: "any", AllowedSubstitutes: []RequestDeliverySubstitute{}}
		raw, err := json.Marshal(map[string]any{"requirements": []map[string]any{
			{"statement": "取得甲文件", "source_quote": "甲文件", "required": true, "verification": "observed", "delivery": observed},
			{"statement": "取得乙文件", "source_quote": "乙文件", "required": true, "verification": "observed", "delivery": observed},
			{"statement": "比較兩份文件", "source_quote": quote, "required": true, "verification": "response", "delivery": response},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	rejected := makeExtraction("比較甲文件與乙文件的差異")
	calls := 0
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
		if operation == "contract" {
			return faithfulRequestContract(), nil
		}
		calls++
		if operation != "extract" || !strings.Contains(prompt, request) {
			t.Fatal("extraction omitted the original request")
		}
		if calls == 1 {
			return rejected, nil
		}
		if calls != 2 || !strings.Contains(prompt, "requirement 3 source_quote") || !strings.Contains(prompt, string(rejected)) || !strings.Contains(prompt, "untrusted data") {
			t.Fatal("repair omitted the rejected extraction or precise validation feedback")
		}
		if c.RunInputSnapshot() != nil {
			t.Fatal("invalid candidate was frozen before repair")
		}
		return makeExtraction(request), nil
	})))
	ctx, end := c.beginInvocationExecutionRun(t.Context())
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveRunInputsForInvocation(ctx, request); err != nil {
		t.Fatal(err)
	}
	contract, err := RequestAcceptanceContractFromSnapshot(c.RunInputSnapshot())
	if err != nil || calls != 2 || len(contract.Requirements) != 3 || contract.Requirements[2].SourceQuote != request {
		t.Fatalf("repaired contract=%#v calls=%d error=%v", contract, calls, err)
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	for _, event := range events {
		if event.Type == string(EventRunInputsResolved) {
			resolved++
		}
	}
	if resolved != 1 || len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatalf("expected one valid frozen contract and no dispatched tasks; resolved=%d", resolved)
	}
}

func TestRequestAcceptanceExtractionRepairIsBounded(t *testing.T) {
	c := requestAcceptanceFixture(t)
	calls := 0
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		calls++
		return requestExtraction("read document", "paraphrased document request", "observed"), nil
	})))
	ctx, end := c.beginInvocationExecutionRun(t.Context())
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	err := c.resolveRunInputsForInvocation(ctx, "read document")
	if err == nil || !strings.Contains(err.Error(), "after one repair") || !strings.Contains(err.Error(), "source_quote") || calls != 2 {
		t.Fatalf("repair calls=%d error=%v", calls, err)
	}
	if c.RunInputSnapshot() != nil || len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatal("invalid extraction was frozen or dispatched work")
	}
}

func TestRequestAcceptanceExtractionDoesNotRetryUnavailableOrOversizedOutput(t *testing.T) {
	for _, kind := range []string{"provider error", "cancelled", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
				calls++
				switch kind {
				case "provider error":
					return nil, errors.New("provider unavailable")
				case "cancelled":
					cancel()
					return []byte(`{}`), nil
				default:
					return []byte(strings.Repeat(" ", maxRunInputValueBytes+1)), nil
				}
			})))
			if _, err := c.resolveRequestAcceptance(ctx, "read document"); err == nil || calls != 1 {
				t.Fatalf("unavailable or oversized output retried: calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestRequestAcceptanceFreezesReplaysAndPreventsExplicitReplacement(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "read document", "observed")
	contract, err := RequestAcceptanceContractFromSnapshot(snapshot)
	if err != nil || contract.Requirements[0].SourceQuote != "read document" {
		t.Fatalf("frozen contract = %#v, error = %v", contract, err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToSessionData(events)
	if len(replayed.RunInputSnapshots) != 1 || replayed.RunInputSnapshots[0].SnapshotHash != snapshot.SnapshotHash {
		t.Fatalf("replayed inputs = %#v", replayed.RunInputSnapshots)
	}
	c.SetRunInputAssignments([]RunInputAssignment{{Name: requestAcceptanceInput, Source: RunInputSourceCLI, RawValue: []byte(`{}`)}})
	if err := c.resolveRunInputsForInvocation(t.Context(), "weaken the request"); err == nil {
		t.Fatal("CLI replaced runtime-owned requirements")
	}
}

func TestRequestAcceptanceActualEvidenceAndOfflineReplay(t *testing.T) {
	for _, request := range []string{"Retrieve the complete source document", "Run the project's verification suite"} {
		t.Run(request, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			snapshot := freezeRequestFixture(t, c, request, "observed")
			item := completedRequestEvidence(t, c, "Verified requested output: complete and successful.", false)
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil || len(sources) == 0 {
				t.Fatalf("sources=%#v error=%v", sources, err)
			}
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
				if operation != "review" || !strings.Contains(prompt, request) {
					t.Fatal("review omitted frozen request")
				}
				return passingRequestReview(sources[0], "complete and successful"), nil
			})))
			acceptance, err := c.runAcceptance(t.Context())
			if err != nil || !acceptance.IsPassed() {
				t.Fatalf("acceptance=%#v error=%v", acceptance, err)
			}
			result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true, RunInputs: snapshot, Acceptance: acceptance}
			store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
				t.Fatal(err)
			}
			acceptance.RequestAcceptance.Checks[0].Evidence[0].Quote = "fabricated successful output"
			if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err == nil {
				t.Fatal("offline replay accepted fabricated evidence")
			}
		})
	}
}

func TestRequestAcceptanceDoesNotAcceptWorkerClaimsOrFailedTools(t *testing.T) {
	for _, failedTool := range []bool{false, true} {
		c := requestAcceptanceFixture(t)
		snapshot := freezeRequestFixture(t, c, "Retrieve the complete source document", "observed")
		completedRequestEvidence(t, c, "Access denied; requested document unavailable.", failedTool)
		sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var response requestAcceptanceSource
		for _, source := range sources {
			if failedTool && source.Kind == "observed" {
				t.Fatal("failed tool was offered as successful evidence")
			}
			if source.Kind == "response" {
				response = source
			}
		}
		c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
			return passingRequestReview(response, "requested comparison"), nil
		})))
		acceptance, err := c.runAcceptance(t.Context())
		if err == nil || acceptance.IsPassed() || acceptance.State != AcceptanceFailed {
			t.Fatalf("worker prose passed observed requirement: %#v error=%v", acceptance, err)
		}
	}
}

func TestRequestAcceptanceUnavailableReviewFailsEvenWithoutStaticAcceptance(t *testing.T) {
	c := requestAcceptanceFixture(t)
	freezeRequestFixture(t, c, "Read the requested document", "observed")
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("reviewer unavailable")
	})))
	acceptance, err := c.runAcceptance(t.Context())
	if err == nil || acceptance.State != AcceptanceFailed || acceptance.RequestAcceptance.Error == "" {
		t.Fatalf("review failure was not retained: %#v error=%v", acceptance, err)
	}
}

func TestRequestAcceptanceDryRunNeverCallsModel(t *testing.T) {
	c := requestAcceptanceFixture(t)
	c.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		t.Fatal("preview called a model")
		return nil, nil
	}))
	if snapshot, err := c.previewRunInputs(t.Context(), "read document"); err != nil || snapshot != nil {
		t.Fatalf("preview=%#v error=%v", snapshot, err)
	}
}

func TestRequestAcceptanceOffPreservesPolicyAndManifestParsing(t *testing.T) {
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 1)
	before := c.executionPolicy.snapshot.ConfigurationHash
	if err := installRequestAcceptanceInput(c.session); err != nil {
		t.Fatal(err)
	}
	after, err := newExecutionPolicyState(c)
	if err != nil || before != after.snapshot.ConfigurationHash {
		t.Fatal("disabled request acceptance changed historical policy")
	}
	if c.requestAcceptancePrompt() != "" {
		t.Fatal("disabled request acceptance changed worker prompt")
	}
	session := &TeamSession{Config: agent.TeamConfig{RequestAcceptance: true}, RunInputDefinitions: []RunInputDefinition{{Name: requestAcceptanceInput}}}
	if err := installRequestAcceptanceInput(session); err == nil {
		t.Fatal("team replaced reserved runtime input")
	}
}

func TestRequestAcceptanceManifestFormats(t *testing.T) {
	for _, manifest := range []string{
		"name: generic\ngoal-mode: outcome\nrequest-acceptance: true\n",
		"apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: generic\nspec:\n  goal-mode: outcome\n  request-acceptance: true\n",
	} {
		dir := t.TempDir()
		writeTeamManifest(t, dir, manifest)
		cfg, err := parseTeamYML(dir, nil)
		if err != nil || !cfg.RequestAcceptance {
			t.Fatalf("manifest request acceptance=%t error=%v", cfg.RequestAcceptance, err)
		}
	}
}

func TestRequestAcceptanceInterruptedAndTargetedRecoveryReuseFrozenRequirements(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Read the requested source", "observed")
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{recoveryRequestSpec(snapshot)})[0]
	c.taskTracker.TodoList().SetRunID(snapshot.RunID)
	c.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		t.Fatal("resume reinterpreted the original request")
		return nil, nil
	}))
	if err := c.resolveRunInputsForInvocation(t.Context(), "weakened replacement prompt"); err != nil {
		t.Fatal(err)
	}
	if got := c.RunInputSnapshot(); got.SnapshotHash != snapshot.SnapshotHash {
		t.Fatal("resume changed frozen requirements")
	}
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskError, "interrupted")
	if err := c.resolveRecoveryRunInputs(t.Context(), item.ID); err != nil {
		t.Fatal(err)
	}
	if got := c.RunInputSnapshot(); got.SnapshotHash != snapshot.SnapshotHash {
		t.Fatal("targeted recovery changed frozen requirements")
	}
}

func TestRequestAcceptanceCrashAfterWorkersFinishKeepsOriginalContract(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Read the requested source", "observed")
	completedRequestEvidence(t, c, "source text", false)
	originalRunID := c.executionRunID
	c.executionRunID = "run-resumed-before-terminal"
	t.Cleanup(func() { c.executionRunID = originalRunID })
	c.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		t.Fatal("post-worker crash reinterpreted requirements from a resume instruction")
		return nil, nil
	}))
	if err := c.resolveRunInputsForInvocation(t.Context(), "Resume the previous session"); err != nil {
		t.Fatal(err)
	}
	if got := c.RunInputSnapshot(); got.SnapshotHash != snapshot.SnapshotHash {
		t.Fatal("crash before run_finished weakened frozen requirements")
	}
}

func recoveryRequestSpec(snapshot *RunInputSnapshot) TodoSpec {
	return TodoSpec{Agent: "worker", Desc: "interrupted work", RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
}

func TestRequestAcceptanceBindsOrdinaryTasksAndRejectsCrossInvocationEvidence(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Read the requested source", "observed")
	task, err := c.canonicalizeTaskOccurrence(TaskDef{Agent: "worker", Goal: "perform work"}, c.session.Agents["worker"], "remote/agent-model")
	if err != nil || task.RunInputSnapshotHash != snapshot.SnapshotHash || task.RunInputSnapshotID != snapshot.ID {
		t.Fatalf("ordinary task binding=%#v error=%v", task, err)
	}
	task.RunInputSnapshotHash = runInputHash([]byte("different invocation"))
	if _, err := c.bindRequestAcceptanceTask(task); err == nil {
		t.Fatal("rebound a task from a different invocation")
	}
	item := completedRequestEvidence(t, c, "requested source text", false)
	item.ExecutionReceipt.RunInputSnapshotHash = runInputHash([]byte("different invocation"))
	for i := range item.ExecutionReceipts {
		item.ExecutionReceipts[i].RunInputSnapshotHash = item.ExecutionReceipt.RunInputSnapshotHash
	}
	if err := c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
		t.Fatal(err)
	}
	sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
	if err != nil || len(sources) != 0 {
		t.Fatalf("foreign evidence was offered: %#v, %v", sources, err)
	}
}

func TestRequestAcceptanceReviewCannotWeakenStaticChecks(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Read the requested source", "observed")
	completedRequestEvidence(t, c, "requested source text", false)
	if err := c.SetAcceptanceSpec(AcceptanceSpec{RequiredArtifacts: []string{"missing-required-artifact"}}); err != nil {
		t.Fatal(err)
	}
	sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		return passingRequestReview(sources[0], "requested source text"), nil
	})))
	acceptance, err := c.runAcceptance(t.Context())
	if err == nil || acceptance.IsPassed() || acceptance.RequestAcceptance.Validate() != nil {
		t.Fatalf("semantic review bypassed static acceptance: %#v error=%v", acceptance, err)
	}
}

func TestRequestAcceptanceReviewRejectsMalformedOrUngroundedVerdicts(t *testing.T) {
	for name, review := range map[string]string{
		"missing checks":          `{"request_covered":true,"checks":[]}`,
		"unknown ID":              `{"request_covered":true,"checks":[{"id":"other","status":"satisfied","reason":"ok","evidence":[]}]}`,
		"ungrounded quote":        `{"request_covered":true,"checks":[{"id":"requirement-1","status":"satisfied","reason":"ok","evidence":[{"source_id":"source-1","quote":"invented"}]}]}`,
		"missing requirement":     `{"request_covered":true,"checks":[{"id":"requirement-1","status":"missing","reason":"not obtained","evidence":[]}]}`,
		"weakened contract":       `{"request_covered":false,"checks":[{"id":"requirement-1","status":"satisfied","reason":"ok","evidence":[{"source_id":"source-1","quote":"source text"}]}]}`,
		"unknown status":          `{"request_covered":true,"checks":[{"id":"requirement-1","status":"pass","reason":"ok","evidence":[]}]}`,
		"fenced ungrounded quote": "```json\n{\"request_covered\":true,\"checks\":[{\"id\":\"requirement-1\",\"status\":\"satisfied\",\"reason\":\"ok\",\"evidence\":[{\"source_id\":\"source-1\",\"quote\":\"invented\"}]}]}\n```",
	} {
		t.Run(name, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			freezeRequestFixture(t, c, "Read the requested source", "observed")
			completedRequestEvidence(t, c, "source text", false)
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
				return []byte(review), nil
			})))
			acceptance, err := c.runAcceptance(t.Context())
			if err == nil || acceptance.IsPassed() || acceptance.EffectiveState() != AcceptanceFailed {
				t.Fatalf("invalid verdict accepted: %#v error=%v", acceptance, err)
			}
		})
	}
}

func TestRequestAcceptanceDirectAgentTerminalAndReplay(t *testing.T) {
	for _, satisfied := range []bool{true, false} {
		t.Run(map[bool]string{true: "satisfied", false: "missing"}[satisfied], func(t *testing.T) {
			c := newDirectTerminationCoordinator(t, directTerminationAgent{})
			c.session.Config.RequestAcceptance = true
			if err := installRequestAcceptanceInput(c.session); err != nil {
				t.Fatal(err)
			}
			if err := c.SetAcceptanceSpec(AcceptanceSpec{RequireNoUnresolvedTasks: true}); err != nil {
				t.Fatal(err)
			}
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(ctx context.Context, operation, prompt string) ([]byte, error) {
				if operation == "contract" {
					return faithfulRequestContract(), nil
				}
				if operation == "extract" {
					return requestExtraction("Explain completed work", "Explain completed work", "response"), nil
				}
				if !satisfied {
					return []byte(`{"request_covered":true,"checks":[{"id":"requirement-1","status":"missing","reason":"requested explanation not delivered","evidence":[]}]}`), nil
				}
				if !strings.Contains(prompt, `"id":"candidate-response"`) {
					t.Fatal("direct agent response has no review source")
				}
				return passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "direct agent completed"), nil
			})))
			direct, err := c.RunDirectAgent(t.Context(), "worker", "Explain completed work")
			if err != nil {
				t.Fatal(err)
			}
			result := c.LastRunResult()
			if result == nil || result.GoalSatisfied != satisfied || (direct.Error == nil) != satisfied {
				t.Fatalf("direct=%#v terminal=%#v", direct, result)
			}
			if !satisfied && (result.Outcome != RunOutcomePartial || result.StopReason != StopReasonAcceptanceFailed) {
				t.Fatalf("missing requirement incorrectly finalized: %#v", result)
			}
			if !strings.Contains(result.Response, "Request acceptance") {
				t.Fatal("final result omitted requirement statuses")
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			replayed := ReduceToSessionData(events)
			if replayed.RunResult == nil || replayed.RunResult.GoalSatisfied != satisfied || replayed.RunResult.Acceptance.RequestAcceptance == nil {
				t.Fatalf("terminal replay lost request acceptance: %#v", replayed.RunResult)
			}
			if satisfied {
				store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
				if err != nil {
					t.Fatal(err)
				}
				if err := ValidateRequestAcceptanceBinding(t.Context(), replayed.RunResult, replayed.Tasks, store); err != nil {
					t.Fatal(err)
				}
				replayed.RunResult.Response = "different final answer"
				if err := ValidateRequestAcceptanceBinding(t.Context(), replayed.RunResult, replayed.Tasks, store); err == nil {
					t.Fatal("offline verification accepted a changed final answer")
				}
			}
		})
	}
}

func TestRequestAcceptanceFinalResponseEvidenceAndOfflineReplay(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Explain the findings", "response")
	item := completedRequestEvidence(t, c, "Supporting observation only", false)
	response := "The findings show a discrepancy, with uncertainty about its cause."
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
		if operation != "review" || !strings.Contains(prompt, `"id":"candidate-response"`) || !strings.Contains(prompt, response) {
			t.Fatal("review omitted the source for the actual delivered response")
		}
		return passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "a discrepancy, with uncertainty"), nil
	})))
	acceptance, err := c.runAcceptance(t.Context(), response)
	if err != nil || !acceptance.IsPassed() {
		t.Fatalf("final response acceptance=%#v error=%v", acceptance, err)
	}
	result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true,
		Response: response + FormatRequestAcceptance(acceptance.RequestAcceptance), RunInputs: snapshot, Acceptance: acceptance}
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
		t.Fatal(err)
	}
	acceptance.RequestAcceptance.Checks[0].Evidence[0].Quote = "invented final analysis"
	if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err == nil {
		t.Fatal("offline replay accepted a quotation absent from the final response")
	}
}

func TestRequestAcceptanceResponseCannotSubstituteForObservationOrFinalDelivery(t *testing.T) {
	for _, verification := range []string{"observed", "response"} {
		t.Run(verification, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			snapshot := freezeRequestFixture(t, c, "Explain the findings", verification)
			completedRequestEvidence(t, c, "Supporting observation only", false)
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil {
				t.Fatal(err)
			}
			source := requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}
			if verification == "response" {
				// Worker output alone must not establish delivery to the user.
				source = sources[0]
			}
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
				return passingRequestReview(source, "Supporting observation only"), nil
			})))
			acceptance, err := c.runAcceptance(t.Context(), "Supporting observation only")
			if err == nil || acceptance.IsPassed() {
				t.Fatalf("response source crossed its evidence boundary: %#v error=%v", acceptance, err)
			}
		})
	}
}

func TestRequestAcceptanceProofRepairIsBoundedAndCannotUpgradeVerdicts(t *testing.T) {
	for _, kind := range []string{"correct quote", "still invalid", "upgrade missing", "negative coverage"} {
		t.Run(kind, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			freezeRequestFixture(t, c, "Explain findings", "response")
			calls := 0
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
				calls++
				if operation != "review" || !strings.Contains(prompt, "The **verified** finding") {
					t.Fatal("review repair lost its original response")
				}
				raw := passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "The verified finding")
				if calls == 1 && kind == "negative coverage" {
					return []byte(strings.Replace(string(raw), `"request_covered":true`, `"request_covered":false`, 1)), nil
				}
				if calls == 1 && kind == "upgrade missing" {
					return []byte(strings.Replace(string(raw), `"status":"satisfied"`, `"status":"missing"`, 1)), nil
				}
				if calls == 2 {
					if !strings.Contains(prompt, "Rejected review") || !strings.Contains(prompt, "candidate-response; copy a short exact substring") {
						t.Fatal("proof repair omitted exact validation feedback")
					}
					if kind != "still invalid" {
						return passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "The **verified** finding"), nil
					}
				}
				return raw, nil
			})))
			acceptance, err := c.runAcceptance(t.Context(), "The **verified** finding is explained here.")
			if kind == "correct quote" {
				if err != nil || !acceptance.IsPassed() || calls != 2 {
					t.Fatalf("quote repair failed: calls=%d acceptance=%#v error=%v", calls, acceptance, err)
				}
				return
			}
			wantCalls := 2
			if kind == "negative coverage" || kind == "upgrade missing" {
				wantCalls = 1
			}
			if err == nil || acceptance.IsPassed() || calls != wantCalls {
				t.Fatalf("invalid proof or upgraded verdict passed: calls=%d acceptance=%#v error=%v", calls, acceptance, err)
			}
		})
	}
}

func TestRequestAcceptanceProofRepairSelectsCorrectSourceKind(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Retrieve the source document", "observed")
	completedRequestEvidence(t, c, "The source document was returned.", false)
	sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
	if err != nil || len(sources) == 0 {
		t.Fatalf("sources=%#v error=%v", sources, err)
	}
	calls := 0
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, _ string, prompt string) ([]byte, error) {
		calls++
		sourceID := requestAcceptanceFinalResponseSource
		if calls == 2 {
			if !strings.Contains(prompt, "requires observed evidence") {
				t.Fatal("repair omitted the source-kind validation feedback")
			}
			sourceID = sources[0].ID
		}
		return json.Marshal(map[string]any{"request_covered": true, "checks": []any{map[string]any{
			"id": "requirement-1", "status": "satisfied", "reason": "The document is available.",
			"delivery": fixtureRequestDeliveryAssessment(false),
			"evidence": []any{map[string]any{"source_id": sourceID, "quote_index": 0}},
		}}})
	})))
	acceptance, err := c.runAcceptance(t.Context(), "I retrieved the source document.")
	if err != nil || !acceptance.IsPassed() || calls != 2 || acceptance.RequestAcceptance.Checks[0].Evidence[0].Kind != "observed" {
		t.Fatalf("source-kind repair failed: calls=%d acceptance=%#v error=%v", calls, acceptance, err)
	}
}

func TestRequestAcceptanceProofRepairCannotUpgradeOptionalNegative(t *testing.T) {
	c := requestAcceptanceFixture(t)
	request := "Optionally explain findings"
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
		if operation == "contract" {
			return faithfulRequestContract(), nil
		}
		return []byte(strings.Replace(string(requestExtraction(request, request, "response")), `"required":true`, `"required":false`, 1)), nil
	})))
	ctx, end := c.beginInvocationExecutionRun(t.Context())
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveRunInputsForInvocation(ctx, request); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`{"request_covered":true,"checks":[{"id":"requirement-1","status":"missing","reason":"No explanation","evidence":[{"source_id":"candidate-response","quote":"invented"}]}]}`), nil
		}
		return passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "The finding is explained."), nil
	})))
	acceptance, err := c.runAcceptance(ctx, "The finding is explained.")
	if err == nil || acceptance.IsPassed() || calls != 2 || !strings.Contains(err.Error(), "upgraded a negative verdict") {
		t.Fatalf("optional negative verdict was upgraded: calls=%d acceptance=%#v error=%v", calls, acceptance, err)
	}
}

func TestRequestAcceptanceIndexedProofBindsExactUnicodeSourceAndAudits(t *testing.T) {
	for _, verification := range []string{"observed", "response"} {
		t.Run(verification, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			snapshot := freezeRequestFixture(t, c, "Explain the findings", verification)
			const suffix = "中文**原文**\nExact source quotation."
			text := strings.Repeat("界", 384) + suffix
			item := completedRequestEvidence(t, c, text, false)
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil || len(sources) == 0 {
				t.Fatalf("sources=%#v error=%v", sources, err)
			}
			sourceID := sources[0].ID
			if verification == "response" {
				sourceID = requestAcceptanceFinalResponseSource
			}
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
				if operation != "review" || !strings.Contains(prompt, `"quote_options"`) || !strings.Contains(prompt, `"index":1`) {
					t.Fatal("review did not receive indexed source text")
				}
				return json.Marshal(map[string]any{"request_covered": true, "checks": []any{map[string]any{
					"id": "requirement-1", "status": "satisfied", "reason": "The selected source supports the requirement.",
					"delivery": fixtureRequestDeliveryAssessment(verification == "response"),
					"evidence": []any{map[string]any{"source_id": sourceID, "quote_index": 1}},
				}}})
			})))
			acceptance, err := c.runAcceptance(t.Context(), text)
			if err != nil || !acceptance.IsPassed() {
				t.Fatalf("indexed acceptance=%#v error=%v", acceptance, err)
			}
			proof := acceptance.RequestAcceptance.Checks[0].Evidence[0]
			if proof.Quote != suffix || proof.QuoteIndex != nil {
				t.Fatalf("runtime did not bind the exact UTF-8 quotation: %#v", proof)
			}
			result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true,
				Response: text + FormatRequestAcceptance(acceptance.RequestAcceptance), RunInputs: snapshot, Acceptance: acceptance}
			store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRequestAcceptanceIndexedProofRejectsInvalidSelection(t *testing.T) {
	for _, evidence := range []string{
		`{"source_id":"candidate-response","quote_index":-1}`,
		`{"source_id":"candidate-response","quote_index":999}`,
		`{"source_id":"candidate-response","quote_index":0,"quote":"invented"}`,
		`{"source_id":"foreign-source","quote_index":0}`,
	} {
		c := requestAcceptanceFixture(t)
		freezeRequestFixture(t, c, "Explain findings", "response")
		c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
			return []byte(`{"request_covered":true,"checks":[{"id":"requirement-1","status":"satisfied","reason":"ok","evidence":[` + evidence + `]}]}`), nil
		})))
		acceptance, err := c.runAcceptance(t.Context(), "The finding is explained.")
		if err == nil || acceptance.IsPassed() {
			t.Fatalf("invalid selection accepted: %s; result=%#v", evidence, acceptance)
		}
	}
}

func TestRequestAcceptanceCoordinatorFinishBoundsRepairAndPersistsPartial(t *testing.T) {
	c := requestAcceptanceFixture(t)
	freezeRequestFixture(t, c, "Retrieve the complete source", "observed")
	completedRequestEvidence(t, c, "source unavailable", false)
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		return []byte(`{"request_covered":true,"checks":[{"id":"requirement-1","status":"blocked","reason":"source unavailable","evidence":[]}]}`), nil
	})))
	tool := &finishTool{coordinator: c}
	for attempt := range 3 {
		response, err := tool.Run(t.Context(), fantasy.ToolCall{Input: `{"response":"Only partial material is available."}`})
		if err != nil {
			t.Fatal(err)
		}
		if response.IsError != (attempt < 2) {
			t.Fatalf("finish attempt=%d response=%#v", attempt, response)
		}
	}
	result := c.LastRunResult()
	if result == nil || result.Outcome != RunOutcomePartial || result.StopReason != StopReasonAcceptanceFailed || result.GoalSatisfied || c.selfHealingAttempts != 2 {
		t.Fatalf("missing source escaped bounded acceptance: %#v attempts=%d", result, c.selfHealingAttempts)
	}
}

func TestRequestAcceptanceRuntimeVerifierEvidenceIsBoundToReceipt(t *testing.T) {
	c := requestAcceptanceFixture(t)
	snapshot := freezeRequestFixture(t, c, "Run the verification suite", "observed")
	item := completedRequestEvidence(t, c, "work completed", false)
	verification := &VerificationResult{Command: "fixture-verifier", ExitCode: 0, Stdout: "all requested checks passed", Fingerprint: "verified-attempt"}
	if err := c.taskTracker.TodoList().SetVerificationResult(item.ID, verification); err != nil {
		t.Fatal(err)
	}
	item = c.todoItemByID(item.ID)
	sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
	if err != nil || len(sources) == 0 || sources[0].Tool != "runtime_verification" {
		t.Fatalf("runtime verification unavailable: %#v %v", sources, err)
	}
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		return passingRequestReview(sources[0], "all requested checks passed"), nil
	})))
	acceptance, err := c.runAcceptance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true, RunInputs: snapshot, Acceptance: acceptance}
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
		t.Fatal(err)
	}
	item.VerifyResult.ExitCode = 1
	if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err == nil {
		t.Fatal("failed runtime verifier accepted as successful evidence")
	}
}
