package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShadowSessionFixturesProduceComparableBundlesWithoutMutatingLegacyPrompt(t *testing.T) {
	var fixtures []struct {
		Name         string `json:"name"`
		Role         string `json:"role"`
		Goal         string `json:"goal"`
		STM          string `json:"stm"`
		LTM          string `json:"ltm"`
		LegacyPrompt string `json:"legacy_prompt"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "shadow_sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) < 5 {
		t.Fatalf("fixtures=%d, want at least 5", len(fixtures))
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			workspace := t.TempDir()
			c := &Coordinator{session: &TeamSession{Workspace: workspace}}
			c.contextCompiler = &defaultContextCompiler{c: c}
			spec := ModelContextSpec{ModelID: "qwen3", ContextWindow: 1200, MaxOutputTokens: 200, SafetyMarginTokens: 100}
			if fixture.Role == "coordinator" {
				c.compileShadowCoordinator(context.Background(), CoordinatorContextInput{Goal: fixture.Goal, RawSTM: fixture.STM, RawLTM: fixture.LTM, ModelContext: spec}, fixture.LegacyPrompt)
			} else {
				c.compileShadowWorker(context.Background(), WorkerContextInput{Goal: fixture.Goal, RawSTM: fixture.STM, RawLTM: fixture.LTM, ModelContext: spec, MaxAuxChars: 1200}, fixture.LegacyPrompt)
			}
			rawTrace, err := os.ReadFile(filepath.Join(workspace, "context-shadow-traces.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			var trace ShadowContextTrace
			if err := json.Unmarshal([]byte(strings.TrimSpace(string(rawTrace))), &trace); err != nil {
				t.Fatal(err)
			}
			if trace.Kind != fixture.Role || trace.Fingerprint == "" || trace.CanonicalTokens <= 0 {
				t.Fatalf("incomplete persisted trace: %#v", trace)
			}
			if trace.CanonicalTokens > trace.BudgetTokens {
				t.Fatalf("trace exceeds budget: %#v", trace)
			}
			anchors := shadowAnchorRE.FindAllString(fixture.LegacyPrompt, -1)
			if len(trace.MissingAnchors) > len(anchors) {
				t.Fatalf("invalid anchor comparison: missing=%v anchors=%v", trace.MissingAnchors, anchors)
			}
			if len(trace.MissingAnchors)+len(trace.CriticalCoverage) != len(anchors) {
				t.Fatalf("critical coverage does not account for every anchor: %#v anchors=%v", trace, anchors)
			}
			if trace.DuplicateRatio < 0 || trace.DuplicateRatio > 1 {
				t.Fatalf("invalid duplicate ratio: %f", trace.DuplicateRatio)
			}
			if fixture.LegacyPrompt != "" && len(trace.LegacyOnlyData)+len(trace.CanonicalOnlyData) == 0 {
				t.Fatal("trace did not record legacy/canonical comparison evidence")
			}
		})
	}
}

func TestShadowCompilerProducesDeterministicBudgetedFingerprint(t *testing.T) {
	input := WorkerContextInput{
		Goal: "Implement context tracing", RawSTM: "# 發現\n- context_shadow.go changed\n",
		RawLTM: "# 決策\n- preserve legacy prompt\n", ModelContext: ModelContextSpec{ModelID: "qwen3", ContextWindow: 600, MaxOutputTokens: 100, SafetyMarginTokens: 50}, MaxAuxChars: 600,
	}
	first, err := CompileWorkerContext(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileWorkerContext(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == "" || first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprint must be stable: %q != %q", first.Fingerprint, second.Fingerprint)
	}
	if first.UsedTokens > 150 {
		t.Fatalf("bundle used %d tokens, exceeds budget", first.UsedTokens)
	}
}

func TestShadowTraceRecordsRequestAndEligibilityWithoutContent(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}
	request := validTestContextRequest()
	compiled := CompiledContext{
		Prompt: "secret canonical prompt", Fingerprint: "compiled-fingerprint", UsedTokens: 10,
		IncludedItems: []ContextItem{{ID: "goal", Required: true, Authority: ContextAuthorityNormative, TokenCount: 4}},
		OmittedItems:  []ContextItem{{ID: "optional-policy", Authority: ContextAuthorityNormative, TokenCount: 6}},
	}
	decisions := []ContextRouteDecision{{ContextItemID: "memory-a", Included: false, Reason: ContextOmittedPhase}, {ContextItemID: "memory-b", Included: true, Reason: ContextIncludedRelevant}}
	c.recordShadowTrace(context.Background(), "worker", "secret legacy prompt api_key=trace-secret", request, decisions, ModelContextSpec{ContextWindow: 1000}, compiled, nil)
	raw, err := os.ReadFile(filepath.Join(workspace, "context-shadow-traces.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("shadow trace leaked prompt/context content: %s", raw)
	}
	var trace ShadowContextTrace
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &trace); err != nil {
		t.Fatal(err)
	}
	if trace.RequestFingerprint != request.Fingerprint() || trace.EligibilityReasons[string(ContextOmittedPhase)] != 1 || trace.EligibilityReasons[string(ContextIncludedRelevant)] != 1 {
		t.Fatalf("request/eligibility trace = %#v", trace)
	}
	if len(trace.MissingNormativeAnchors) != 1 || trace.MissingNormativeAnchors[0] != "optional-policy" {
		t.Fatalf("normative omission trace = %#v", trace.MissingNormativeAnchors)
	}
}

func TestBudgetContextItemsNeverExceedsBudgetForOptionalItems(t *testing.T) {
	for n := 0; n < 80; n++ {
		items := make([]ContextItem, n)
		for i := range items {
			items[i] = ContextItem{ID: string(rune('a'+i%26)) + string(rune(i/26+'0')), Content: strings.Repeat("word ", i+1), Priority: PriorityRecentSTM + i%3, TokenCount: i + 1}
		}
		budget := ContextBudget{Available: n/2 + 1}
		selected, _, err := BudgetContextItems(items, budget)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		used := 0
		for _, item := range selected {
			used += item.TokenCount
		}
		if used > budget.Available {
			t.Fatalf("n=%d: selected %d > budget %d", n, used, budget.Available)
		}
	}
}

func FuzzBudgetContextItemsNeverExceedsBudget(f *testing.F) {
	f.Add(uint8(3), uint8(12))
	f.Add(uint8(20), uint8(1))
	f.Fuzz(func(t *testing.T, itemCount, budgetRaw uint8) {
		count := int(itemCount % 64)
		budget := ContextBudget{Available: int(budgetRaw % 128)}
		items := make([]ContextItem, count)
		for i := range items {
			items[i] = ContextItem{ID: string(rune('a' + i%26)), Content: strings.Repeat("token ", i%16+1), Priority: PriorityRecentSTM + i%4, TokenCount: i%32 + 1}
		}
		selected, _, err := BudgetContextItems(items, budget)
		if budget.Available == 0 {
			if err == nil {
				t.Fatal("zero budget must be rejected")
			}
			return
		}
		if err != nil {
			t.Fatalf("BudgetContextItems: %v", err)
		}
		used := 0
		for _, item := range selected {
			used += item.TokenCount
		}
		if used > budget.Available {
			t.Fatalf("used=%d exceeds budget=%d", used, budget.Available)
		}
	})
}
