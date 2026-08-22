package team

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/config"

	"github.com/kjelly/hufu/internal/agent"

	"charm.land/fantasy"
)

func TestExecutionEventLoggerWritesStructuredEvent(t *testing.T) {
	workspace := t.TempDir()
	logger, err := newExecutionEventLogger(workspace)
	if err != nil {
		t.Fatal(err)
	}
	event := ExecutionEvent{Version: 1, Timestamp: "2026-07-12T12:00:00Z", RunID: "run-test", Team: "dev", TaskID: "42", Agent: "developer", Attempt: 2, Status: "done", Usage: ExecutionUsage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}}
	if err := logger.append(event); err != nil {
		t.Fatal(err)
	}
	logger.close()
	f, err := os.Open(filepath.Join(workspace, "logs", executionEventsFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		t.Fatal("expected an event")
	}
	var got ExecutionEvent
	if err := json.Unmarshal(scanner.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != event.RunID || got.TaskID != event.TaskID || got.Attempt != 2 || got.Usage.TotalTokens != 8 {
		t.Fatalf("event = %+v", got)
	}
}

func TestUsageFromStepsEstimatesMissingProviderUsage(t *testing.T) {
	steps := []fantasy.StepResult{{Messages: []fantasy.Message{fantasy.NewUserMessage("12345678")}}}
	got := usageFromSteps(steps)
	if got.TotalTokens != 2 {
		t.Fatalf("usageFromSteps total = %d, want estimated 2", got.TotalTokens)
	}
}

// TestUsageWithProgressTokensAvoidsResentConversationBlowup is the same
// long-resent-conversation shape as
// TestRunAgentAttemptBudgetSurvivesLongResentConversation (35 steps, ~20k
// tokens resent every step, small real output): usageFromSteps.TotalTokens
// sums every step's full usage and legitimately reflects real provider cost
// for receipts/billing, but summed across an attempt it charges the same
// resent history once per step, so a long, healthy attempt produces a total
// that scales with the square of its step count. ProgressTokens must instead
// track growth only, the same accounting attemptBudget already applies via
// reserveContext/chargeOutput, so a single expensive-but-legitimate task
// cannot look identical to unproven thrash to the run-level no-progress
// budget (recordExecutionEvent prefers ProgressTokens over TotalTokens).
func TestUsageWithProgressTokensAvoidsResentConversationBlowup(t *testing.T) {
	attemptTokens := newAttemptBudget(500_000)
	steps := make([]fantasy.StepResult, 35)
	for i := range steps {
		usage := fantasy.Usage{InputTokens: 20_000, OutputTokens: 200, TotalTokens: 20_200}
		steps[i] = fantasy.StepResult{Response: fantasy.Response{Usage: usage}}
		// Mirror how runAgentWithStatusAndHistory charges the live budget:
		// the resent conversation as context growth, generated output
		// separately (see attempt_budget.go's chargeOutput doc comment).
		if err := attemptTokens.reserveContext(usage.InputTokens); err != nil {
			t.Fatalf("unexpected attempt budget error at step %d: %v", i, err)
		}
		if err := attemptTokens.chargeOutput(usage.OutputTokens); err != nil {
			t.Fatalf("unexpected attempt budget error at step %d: %v", i, err)
		}
	}

	raw := usageFromSteps(steps)
	if raw.TotalTokens < 700_000 {
		t.Fatalf("usageFromSteps total = %d, want the raw per-step sum to be large (baseline for the bug this guards against)", raw.TotalTokens)
	}

	got := usageWithProgressTokens(steps, attemptTokens)
	if got.TotalTokens != raw.TotalTokens {
		t.Fatalf("usageWithProgressTokens changed TotalTokens: got %d, want unchanged %d (receipts/billing must stay raw)", got.TotalTokens, raw.TotalTokens)
	}
	if got.ProgressTokens <= 0 {
		t.Fatal("ProgressTokens = 0, want a positive growth-based snapshot")
	}
	if got.ProgressTokens >= raw.TotalTokens/2 {
		t.Fatalf("ProgressTokens = %d, want well under half of the raw sum %d (growth accounting must not blow up with resent history)", got.ProgressTokens, raw.TotalTokens)
	}
}

// TestRecordExecutionEventPrefersProgressTokensForNoProgressBudget asserts
// recordExecutionEvent feeds the no-progress counter from ProgressTokens when
// present, not the raw TotalTokens receipt figure, and still falls back to
// TotalTokens for callers that never compute ProgressTokens (e.g. sidecar
// paths), preserving their existing behavior.
func TestRecordExecutionEventPrefersProgressTokensForNoProgressBudget(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.recordExecutionEvent("1", "worker", 1, "done", "model", 0, ExecutionUsage{TotalTokens: 900_000, ProgressTokens: 40_000})
	if got := c.noProgressCounters().Tokens; got != 40_000 {
		t.Fatalf("no-progress tokens = %d, want ProgressTokens (40000) not raw TotalTokens (900000)", got)
	}

	c2 := &Coordinator{taskTracker: NewTaskTracker()}
	c2.recordExecutionEvent("2", "worker", 1, "done", "model", 0, ExecutionUsage{TotalTokens: 500})
	if got := c2.noProgressCounters().Tokens; got != 500 {
		t.Fatalf("no-progress tokens = %d, want fallback to TotalTokens (500) when ProgressTokens is unset", got)
	}
}

func TestTeamDefinitionRevisionChangesWithDefinition(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "developer.md"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := teamDefinitionRevision(dir)
	if first == "" {
		t.Fatal("expected a definition revision")
	}
	if err := os.WriteFile(filepath.Join(dir, "developer.md"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := teamDefinitionRevision(dir); got == first {
		t.Fatalf("revision = %q, want a new hash after definition change", got)
	}
}

func TestExecutionEvent_ModelProviderAndArtifacts(t *testing.T) {
	pm, _ := agent.NewProviderManager("", "", map[string]config.ProviderConfig{
		"openai": {ProviderURL: "https://api.openai.com/v1", ProviderAPIKey: "test"},
	})
	c := &Coordinator{
		taskResultCache: make(map[string][]cachedTaskEntry),
		providerManager: pm,
	}
	c.session = &TeamSession{Config: agent.TeamConfig{Name: "test-team"}}

	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{{Desc: "test", Agent: "tester"}})
	tid := items[0].ID
	tracker.TodoList().SetTypedResult(tid, &TaskResult{Artifacts: []ArtifactRef{{Path: "art1"}}})
	_ = tracker.TodoList().AppendFailureFingerprint(tid, FailureFingerprint{Digest: "fail1"})
	c.taskTracker = tracker

	workspace := t.TempDir()
	c.executionRunID = "run-1"
	logger, _ := newExecutionEventLogger(workspace)
	c.executionEvents = logger

	// Qualified
	c.recordExecutionEvent(tid, "tester", 1, "done", "openai/gpt-4o", 100, ExecutionUsage{})

	// Unqualified
	c.recordExecutionEvent(tid, "tester", 2, "error", "qwen3:8b", 200, ExecutionUsage{})

	logger.close()

	f, _ := os.Open(filepath.Join(workspace, "logs", executionEventsFile))
	defer f.Close()
	scanner := bufio.NewScanner(f)

	if !scanner.Scan() {
		t.Fatal("expected first event")
	}
	var ev1 ExecutionEvent
	json.Unmarshal(scanner.Bytes(), &ev1)
	if ev1.Provider != "openai" {
		t.Errorf("expected openai, got %v", ev1.Provider)
	}
	if len(ev1.ArtifactRefs) != 1 || ev1.ArtifactRefs[0].Path != "art1" {
		t.Errorf("expected art1, got %v", ev1.ArtifactRefs)
	}
	if ev1.FailureSignature != "fail1" {
		t.Errorf("expected fail1, got %v", ev1.FailureSignature)
	}

	if !scanner.Scan() {
		t.Fatal("expected second event")
	}
	var ev2 ExecutionEvent
	json.Unmarshal(scanner.Bytes(), &ev2)
	if ev2.Provider != "local" { // unqualified models use the generic default provider
		t.Errorf("expected local, got %v", ev2.Provider)
	}
}

func TestLifecycleEventPayloadSerialization(t *testing.T) {
	// Raw JSON test of the complete successful timeline plus failure
	payload := LifecycleEventPayload{
		Phase: "execute",
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"phase":"execute"`) {
		t.Errorf("missing phase")
	}
	if !strings.Contains(s, `"agent":""`) {
		t.Errorf("missing agent %s", s)
	}
	if !strings.Contains(s, `"provider":""`) {
		t.Errorf("missing provider %s", s)
	}
	if !strings.Contains(s, `"artifacts":null`) && !strings.Contains(s, `"artifacts":[]`) {
		t.Errorf("missing artifacts %s", s)
	}
	if !strings.Contains(s, `"failure_signature":""`) {
		t.Errorf("missing failure_signature %s", s)
	}
}
