package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestModelSpecRegistry(t *testing.T) {
	reg := NewModelSpecRegistry()

	t.Run("known models", func(t *testing.T) {
		spec := reg.GetSpec("gpt-4o")
		if spec.ContextWindow != 128000 {
			t.Errorf("gpt-4o context window = %d, want 128000", spec.ContextWindow)
		}
		if spec.Estimator != "tiktoken" {
			t.Errorf("gpt-4o estimator = %q, want tiktoken", spec.Estimator)
		}
	})

	t.Run("provider prefix resolution", func(t *testing.T) {
		spec := reg.GetSpec("ollama/qwen3:8b")
		if spec.Estimator != "qwen" {
			t.Errorf("ollama/qwen3:8b estimator = %q, want qwen", spec.Estimator)
		}
		if spec.ContextWindow != 128000 {
			t.Errorf("ollama/qwen3:8b context window = %d, want 128000", spec.ContextWindow)
		}
	})

	t.Run("fallback estimation for unknown models", func(t *testing.T) {
		spec := reg.GetSpec("unknown-provider/custom-model-99b")
		if spec.ContextWindow != 128000 {
			t.Errorf("fallback context window = %d, want 128000", spec.ContextWindow)
		}
		if !spec.IsEstimated {
			t.Error("expected IsEstimated = true for unknown model")
		}
	})

	t.Run("custom registration", func(t *testing.T) {
		reg.RegisterSpec(ModelContextSpec{
			ModelID:            "my-custom-model",
			ContextWindow:      64000,
			MaxOutputTokens:    8192,
			SafetyMarginTokens: 1500,
			Estimator:          "exact",
		})
		spec := reg.GetSpec("my-custom-model")
		if spec.ContextWindow != 64000 || spec.Estimator != "exact" {
			t.Errorf("custom spec mismatch: %+v", spec)
		}
	})
}

func TestEstimatorTokenCounts(t *testing.T) {
	tc := NewDefaultTokenCounter(globalRegistry)
	ctx := context.Background()

	t.Run("english text token estimation", func(t *testing.T) {
		text := "Hello world, this is a test prompt for estimating tokens."
		count, err := tc.CountText(ctx, "gpt-4o", text)
		if err != nil {
			t.Fatalf("CountText error: %v", err)
		}
		if count <= 0 || count > len(text) {
			t.Errorf("CountText = %d, want plausible estimate between 1 and %d", count, len(text))
		}
	})

	t.Run("cjk text has higher token density", func(t *testing.T) {
		cjkText := "這是測試中文 Token 估算器，繁體與簡體中文字元密度與英文字母不同。"
		engText := "This is a test english sentence with the exact same character count 30."

		cjkCount, _ := tc.CountText(ctx, "qwen3", cjkText)
		engCount, _ := tc.CountText(ctx, "qwen3", engText)

		if cjkCount <= engCount {
			t.Errorf("cjk tokens (%d) should be > english tokens (%d) for same length due to char density", cjkCount, engCount)
		}
	})
}

func TestContextBudgetAndReport(t *testing.T) {
	spec := ModelContextSpec{
		ModelID:            "claude-3-5-sonnet",
		ContextWindow:      65536,
		MaxOutputTokens:    12000,
		SafetyMarginTokens: 2000,
		Estimator:          "claude",
	}

	budget := CalculateContextBudget(spec, 8210, 4540)

	if budget.Window != 65536 {
		t.Errorf("Window = %d, want 65536", budget.Window)
	}
	wantAvail := 65536 - 8210 - 4540 - 12000 - 2000
	if budget.Available != wantAvail {
		t.Errorf("Available = %d, want %d", budget.Available, wantAvail)
	}

	usage := ContextUsageBreakdown{
		SystemInstructions:    8210,
		ToolSchemas:           4540,
		RecentConversation:    18200,
		CompactedHistory:      5430,
		ProjectContext:        3210,
		StmLtmRag:             4870,
		TaskDependencyResults: 3360,
		ReplyReserve:          12000,
	}

	report := budget.BreakdownReport(usage)
	if !strings.Contains(report, "Context usage: 59,820 / 65,536") {
		t.Errorf("report missing total header, got:\n%s", report)
	}
	if !strings.Contains(report, "System instructions") || !strings.Contains(report, "Reply reserve") {
		t.Errorf("report missing section breakdown, got:\n%s", report)
	}
}

func TestCapStepMessagesWithCounter(t *testing.T) {
	ctx := context.Background()
	counter := NewDefaultTokenCounter(globalRegistry)
	bigText := strings.Repeat("This is a bulky result content. ", 500)

	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("System prompt"),
		fantasy.NewUserMessage("Original goal"),
		toolResultMsg("c1", bigText),
		toolResultMsg("c2", bigText),
	}
	for range recentMessagesProtected {
		msgs = append(msgs, fantasy.NewUserMessage("recent exchange"))
	}

	t.Run("under budget returns nil", func(t *testing.T) {
		smallMsgs := []fantasy.Message{fantasy.NewUserMessage("goal")}
		if got := CapStepMessagesWithCounter(ctx, counter, "qwen3", smallMsgs, 5000); got != nil {
			t.Fatal("expected nil for under-budget messages")
		}
	})

	t.Run("over token budget squeezes old messages", func(t *testing.T) {
		got := CapStepMessagesWithCounter(ctx, counter, "qwen3", msgs, 2000)
		if got == nil {
			t.Fatal("expected message capping for over-budget messages")
		}
		if len(got) != len(msgs) {
			t.Fatalf("message count changed: %d -> %d", len(msgs), len(got))
		}

		// Ensure system/goal were protected
		if messageTextSize(got[0]) != messageTextSize(msgs[0]) || messageTextSize(got[1]) != messageTextSize(msgs[1]) {
			t.Error("head messages should not be squeezed")
		}

		// Old tool result should be squeezed
		if messageTextSize(got[2]) >= messageTextSize(msgs[2]) {
			t.Error("bulky old tool result should be squeezed")
		}
	})
}

func TestIsContextOverflowError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("generic error"), false},
		{errors.New("400 Bad Request: context length exceeded"), true},
		{errors.New("maximum context length is 128000 tokens"), true},
		{errors.New("prompt is too long for token limit"), true},
		{errors.New("Context overflow in stream call"), true},
	}
	for _, tc := range cases {
		got := IsContextOverflowError(tc.err)
		if got != tc.want {
			t.Errorf("IsContextOverflowError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestWarnEstimatedOnceDedup(t *testing.T) {
	// Reset dedup state for this test by using a fresh model ID unlikely to
	// collide with other tests.
	modelID := "test-dedup-model-unique-xyz"
	var calls int
	orig := estimatedModelLogger
	estimatedModelLogger = func(m, est string) { calls++ }
	t.Cleanup(func() { estimatedModelLogger = orig })
	// Clear any prior entry for this model from the dedup map so the test is
	// deterministic regardless of execution order.
	estimatedWarnSeen.Delete(modelID)

	warnEstimatedOnce(modelID, "estimated")
	warnEstimatedOnce(modelID, "estimated")
	warnEstimatedOnce(modelID, "estimated")
	if calls != 1 {
		t.Errorf("expected exactly 1 warning for repeated model, got %d", calls)
	}
}

func TestCountTokensInTextModelAware(t *testing.T) {
	text := "這是中文與 English 混合的測試字串 with code { }"
	cjkTokens := countTokensInText("qwen3", text)
	defaultTokens := countTokensInText("", text)
	if cjkTokens <= 0 {
		t.Errorf("countTokensInText(qwen3) = %d, want > 0", cjkTokens)
	}
	if defaultTokens <= 0 {
		t.Errorf("countTokensInText(default) = %d, want > 0", defaultTokens)
	}
}

func TestCountTokensInMessagesModelAware(t *testing.T) {
	msgs := []fantasy.Message{
		fantasy.NewUserMessage("Implement the feature and run tests."),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Done."}}},
	}
	n := countTokensInMessages("gpt-4o", msgs)
	if n <= 0 {
		t.Errorf("countTokensInMessages(gpt-4o) = %d, want > 0", n)
	}
}
