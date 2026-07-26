package team

import (
	"context"
	"strings"
	"testing"
)

func TestShadowCompilerProducesDeterministicBudgetedFingerprint(t *testing.T) {
	input := WorkerContextInput{
		TaskGoal: "Implement context tracing", RawSTM: "# 發現\n- context_shadow.go changed\n",
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
