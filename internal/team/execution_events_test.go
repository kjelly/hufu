package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestNewInvocationIDIsDistinctFromExecutionRunID(t *testing.T) {
	first := NewInvocationID()
	second := NewInvocationID()
	runID := newExecutionRunID()
	if !strings.HasPrefix(first, "inv-") || !strings.HasPrefix(second, "inv-") {
		t.Fatalf("invocation IDs = %q, %q; want inv- prefix", first, second)
	}
	if first == second {
		t.Fatalf("NewInvocationID reused %q", first)
	}
	if !strings.HasPrefix(runID, "run-") {
		t.Fatalf("execution run ID = %q, want run- prefix", runID)
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
// budget (recordAttemptUsage prefers ProgressTokens over TotalTokens).
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

// TestRecordAttemptUsagePrefersProgressTokensForNoProgressBudget asserts
// recordAttemptUsage feeds the no-progress counter from ProgressTokens when
// present, not the raw TotalTokens receipt figure, and still falls back to
// TotalTokens for callers that never compute ProgressTokens (e.g. sidecar
// paths), preserving their existing behavior.
func TestRecordAttemptUsagePrefersProgressTokensForNoProgressBudget(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.recordAttemptUsage("1", 1, ExecutionUsage{TotalTokens: 900_000, ProgressTokens: 40_000})
	if got := c.noProgressCounters().Tokens; got != 40_000 {
		t.Fatalf("no-progress tokens = %d, want ProgressTokens (40000) not raw TotalTokens (900000)", got)
	}

	c2 := &Coordinator{taskTracker: NewTaskTracker()}
	c2.recordAttemptUsage("2", 1, ExecutionUsage{TotalTokens: 500})
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

func TestExecutionEventWritesBackendAndReadsLegacyProvider(t *testing.T) {
	encoded, err := json.Marshal(ExecutionEvent{
		Version:  executionEventSchemaVersion,
		Backend:  "ollama",
		Provider: "hufu-local",
	})
	if err != nil {
		t.Fatalf("marshal execution event: %v", err)
	}
	if strings.Contains(string(encoded), `"provider"`) || !strings.Contains(string(encoded), `"backend":"ollama"`) {
		t.Fatalf("canonical execution event = %s", encoded)
	}

	var legacy ExecutionEvent
	if err := json.Unmarshal([]byte(`{"version":4,"provider":"openai"}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy execution event: %v", err)
	}
	if legacy.Provider != "openai" {
		t.Fatalf("legacy provider was not retained for read compatibility: %#v", legacy)
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
