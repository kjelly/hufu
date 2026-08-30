package team

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
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

func TestWithEffectiveMaxOutputTokens(t *testing.T) {
	spec := ModelContextSpec{ModelID: "gemma4:31b", ContextWindow: 128000, MaxOutputTokens: 4096, SafetyMarginTokens: 2000, IsEstimated: true}

	overridden := spec.WithEffectiveMaxOutputTokens(16384)
	if overridden.MaxOutputTokens != 16384 {
		t.Errorf("MaxOutputTokens = %d, want 16384 (effective max-tokens must win over the registry guess)", overridden.MaxOutputTokens)
	}
	// Everything else is untouched.
	if overridden.ContextWindow != 128000 || overridden.SafetyMarginTokens != 2000 {
		t.Errorf("unrelated fields changed: %+v", overridden)
	}

	unchanged := spec.WithEffectiveMaxOutputTokens(0)
	if unchanged.MaxOutputTokens != 4096 {
		t.Errorf("MaxOutputTokens = %d, want unchanged 4096 when effective <= 0", unchanged.MaxOutputTokens)
	}
}

func TestRegisterConfiguredContextWindowPreservesSafetyBudgets(t *testing.T) {
	modelID := "operator-context-window-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 128000, MaxOutputTokens: 8192,
		SafetyMarginTokens: 1500, Estimator: "estimated", IsEstimated: true,
	})

	RegisterConfiguredContextWindow([]string{modelID}, 32768)
	spec := GlobalModelSpecRegistry().GetSpec(modelID)
	if spec.ContextWindow != 32768 || spec.ContextWindowSource != "operator" || spec.IsEstimated {
		t.Fatalf("configured context spec = %#v, want operator exact capacity", spec)
	}
	if spec.MaxOutputTokens != 8192 || spec.SafetyMarginTokens != 1500 || spec.Estimator != "estimated" {
		t.Fatalf("configured context changed output/safety budgets: %#v", spec)
	}
}

func TestUnknownModelRemainsEstimatedWithoutConfiguredContextWindow(t *testing.T) {
	spec := GlobalModelSpecRegistry().GetSpec("unknown-unconfigured-context-model")
	if !spec.IsEstimated {
		t.Fatalf("unknown model spec = %#v, want fail-closed estimated metadata", spec)
	}
}

func TestResolveAgentMaxOutputTokens(t *testing.T) {
	c := &Coordinator{session: &TeamSession{
		Config: agent.TeamConfig{Generation: agent.GenerationParams{MaxTokens: "8192"}},
	}}

	if got := c.resolveAgentMaxOutputTokens(&agent.AgentDef{Generation: agent.GenerationParams{MaxTokens: "4096"}}); got != 4096 {
		t.Errorf("agent-configured max-tokens = %d, want 4096 (agent must win over team)", got)
	}
	if got := c.resolveAgentMaxOutputTokens(&agent.AgentDef{}); got != 8192 {
		t.Errorf("unset agent max-tokens = %d, want team fallback 8192", got)
	}
	if got := c.resolveAgentMaxOutputTokens(nil); got != 8192 {
		t.Errorf("nil agent = %d, want team fallback 8192", got)
	}

	empty := &Coordinator{session: &TeamSession{}}
	if got := empty.resolveAgentMaxOutputTokens(nil); got != 0 {
		t.Errorf("no config anywhere = %d, want 0 (let caller fall back to registry)", got)
	}
}

func TestDetectAndCacheOllamaContextLengths(t *testing.T) {
	srv := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gemma4:31b", "max_context_window": 131072}},
		})
	}))
	defer srv.Close()

	modelID := "ollama/gemma4:31b"
	DetectAndCacheOllamaContextLengths(context.Background(), srv.URL+"/v1", "", []string{modelID})

	spec := globalRegistry.GetSpec(modelID)
	if spec.ContextWindow != 131072 {
		t.Errorf("ContextWindow = %d, want 131072 (detected from OpenAI-compatible /models)", spec.ContextWindow)
	}
	if spec.IsEstimated {
		t.Error("provider metadata context window should not remain marked as estimated")
	}
}

func TestDetectAndCacheOllamaContextLengths_SkipsKnownModels(t *testing.T) {
	var called bool
	srv := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// gpt-4o has an exact hardcoded registry entry (not estimated), so it must
	// not be probed over the network at all.
	DetectAndCacheOllamaContextLengths(context.Background(), srv.URL+"/v1", "", []string{"gpt-4o"})

	if called {
		t.Error("probed a model with an exact (non-estimated) registry entry; should have been skipped")
	}
}

func TestParseObservedContextWindow(t *testing.T) {
	tests := []struct {
		name string
		err  string
		want int
		ok   bool
	}{
		{name: "provider available context", err: "request (40428 tokens) exceeds the available context size (39936 tokens)", want: 39936, ok: true},
		{name: "generic context window", err: "context window 8192 exceeded", want: 8192, ok: true},
		{name: "unrelated error", err: "failed to initialize samplers", want: 0, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseObservedContextWindow(errors.New(tt.err))
			if got != tt.want || ok != tt.ok {
				t.Fatalf("ParseObservedContextWindow() = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func newIPv4TestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listener unavailable in this environment: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func TestTruncateToTokenBudget(t *testing.T) {
	t.Run("under budget is unchanged", func(t *testing.T) {
		text := "short text"
		if got := TruncateToTokenBudget(text, "estimated", 1000); got != text {
			t.Errorf("got %q, want unchanged %q", got, text)
		}
	})

	t.Run("zero budget is unchanged", func(t *testing.T) {
		text := strings.Repeat("word ", 5000)
		if got := TruncateToTokenBudget(text, "estimated", 0); got != text {
			t.Error("maxTokens <= 0 must leave text unchanged")
		}
	})

	t.Run("over budget is cut and stays within it", func(t *testing.T) {
		text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 500)
		budget := 200
		got := TruncateToTokenBudget(text, "estimated", budget)
		if len(got) >= len(text) {
			t.Fatalf("expected truncation, got len %d >= original len %d", len(got), len(text))
		}
		if !strings.HasSuffix(got, "... [truncated]") {
			t.Errorf("expected truncation marker, got suffix %q", got[max(0, len(got)-30):])
		}
		prefix := strings.TrimSuffix(got, "\n\n... [truncated]")
		if tokens := estimateTextTokens(prefix, "estimated"); tokens > budget {
			t.Errorf("truncated prefix estimates to %d tokens, want <= %d", tokens, budget)
		}
	})
}

func TestGetWorkerContextSizeIsTokenBudget(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{}}}
	if got := c.getWorkerContextSize(); got != defaultWorkerContextSize {
		t.Errorf("getWorkerContextSize() = %d, want default %d", got, defaultWorkerContextSize)
	}

	configured := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{WorkerContextSize: 500}}}
	if got := configured.getWorkerContextSize(); got != 500 {
		t.Errorf("getWorkerContextSize() = %d, want configured 500", got)
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

func TestCapStepMessagesWithCounterDoesNotSqueezeVerifiedHistory(t *testing.T) {
	ctx := context.Background()
	counter := NewDefaultTokenCounter(globalRegistry)
	verified := verifiedHistoryPrefix + strings.Repeat("verified evidence with E1234 and exit status 1\n", 800)
	bulky := strings.Repeat("ordinary old message ", 2_000)
	msgs := []fantasy.Message{
		fantasy.NewSystemMessage("System prompt"),
		fantasy.NewUserMessage("Original goal"),
		fantasy.NewUserMessage(verified),
		fantasy.NewUserMessage(bulky),
	}
	for range recentMessagesProtected {
		msgs = append(msgs, fantasy.NewUserMessage("recent exchange"))
	}
	got := CapStepMessagesWithCounter(ctx, counter, "qwen3", msgs, 1_000)
	if got == nil {
		t.Fatal("expected budget shaper to process bulky history")
	}
	verifiedPart, ok := fantasy.AsMessagePart[fantasy.TextPart](got[2].Content[0])
	if !ok || verifiedPart.Text != verified {
		t.Fatalf("verified history was altered: %#v", got[2])
	}
	if messageTextSize(got[3]) >= messageTextSize(msgs[3]) {
		t.Fatal("ordinary old message was not squeezed")
	}
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
