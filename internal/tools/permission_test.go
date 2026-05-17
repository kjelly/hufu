package tools

import (
	"context"
	"testing"
)

// ============== GetToolLevel Tests ==============

func TestGetToolLevel_HighRisk(t *testing.T) {
	highRiskTools := []string{"golang", "lua", "bash", "mcp"}

	for _, tool := range highRiskTools {
		t.Run(tool, func(t *testing.T) {
			level := GetToolLevel(tool)
			if level != ToolLevelHigh {
				t.Errorf("GetToolLevel(%q) = %q, want %q", tool, level, ToolLevelHigh)
			}
		})
	}
}

func TestGetToolLevel_MediumRisk(t *testing.T) {
	mediumRiskTools := []string{"download", "fetch", "agentic_fetch"}

	for _, tool := range mediumRiskTools {
		t.Run(tool, func(t *testing.T) {
			level := GetToolLevel(tool)
			if level != ToolLevelMedium {
				t.Errorf("GetToolLevel(%q) = %q, want %q", tool, level, ToolLevelMedium)
			}
		})
	}
}

func TestGetToolLevel_LowRisk(t *testing.T) {
	lowRiskTools := []string{"view", "write", "edit", "grep", "glob", "ls", "ask_user", "random", "sudo", "ssh"}

	for _, tool := range lowRiskTools {
		t.Run(tool, func(t *testing.T) {
			level := GetToolLevel(tool)
			if level != ToolLevelLow {
				t.Errorf("GetToolLevel(%q) = %q, want %q", tool, level, ToolLevelLow)
			}
		})
	}
}

func TestGetToolLevel_Unknown(t *testing.T) {
	level := GetToolLevel("unknown_tool")
	if level != ToolLevelLow {
		t.Errorf("GetToolLevel(%q) = %q, want %q (default)", "unknown_tool", level, ToolLevelLow)
	}
}

func TestGetToolLevel_Empty(t *testing.T) {
	level := GetToolLevel("")
	if level != ToolLevelLow {
		t.Errorf("GetToolLevel(%q) = %q, want %q (default)", "", level, ToolLevelLow)
	}
}

// ============== CheckToolPermission Tests ==============

func TestCheckToolPermission_NoConfig_Allowed(t *testing.T) {
	ctx := context.Background()

	// High-risk tool without config - should ASK (security first)
	allowed, askUser, err := CheckToolPermission(ctx, "bash")
	if err != nil {
		t.Errorf("CheckToolPermission() unexpected error = %v", err)
	}
	if allowed {
		t.Errorf("CheckToolPermission(bash) = %v, want false (security first)", allowed)
	}
	if !askUser {
		t.Errorf("CheckToolPermission(bash) askUser = %v, want true", askUser)
	}

	// Medium-risk tool without config - should ASK
	allowed, askUser, err = CheckToolPermission(ctx, "download")
	if err != nil {
		t.Errorf("CheckToolPermission() unexpected error = %v", err)
	}
	if allowed {
		t.Errorf("CheckToolPermission(download) = %v, want false (security first)", allowed)
	}
	if !askUser {
		t.Errorf("CheckToolPermission(download) askUser = %v, want true", askUser)
	}

	// Low-risk tool without config - should be allowed automatically
	allowed, askUser, err = CheckToolPermission(ctx, "view")
	if err != nil {
		t.Errorf("CheckToolPermission() unexpected error = %v", err)
	}
	if !allowed {
		t.Errorf("CheckToolPermission(view) = %v, want true", allowed)
	}
	if askUser {
		t.Errorf("CheckToolPermission(view) askUser = %v, want false", askUser)
	}
}

func TestCheckToolPermission_HighRisk_Denied(t *testing.T) {
	ctx := context.Background()
	ctx = SetToolsAllowed(ctx, []string{"view", "write"})

	// High-risk tool not in allowed list - should be denied
	allowed, askUser, err := CheckToolPermission(ctx, "bash")
	if err != nil {
		t.Errorf("CheckToolPermission() unexpected error = %v", err)
	}
	if allowed {
		t.Errorf("CheckToolPermission(bash) = %v, want false (not in allowed list)", allowed)
	}
	if askUser {
		t.Errorf("CheckToolPermission(bash) askUser = %v, want false", askUser)
	}
}

func TestCheckToolPermission_HighRisk_Allowed(t *testing.T) {
	ctx := context.Background()
	ctx = SetToolsAllowed(ctx, []string{"view", "bash", "write"})

	// High-risk tool in allowed list - should be allowed
	allowed, askUser, err := CheckToolPermission(ctx, "bash")
	if err != nil {
		t.Errorf("CheckToolPermission() unexpected error = %v", err)
	}
	if !allowed {
		t.Errorf("CheckToolPermission(bash) = %v, want true (in allowed list)", allowed)
	}
	if askUser {
		t.Errorf("CheckToolPermission(bash) askUser = %v, want false", askUser)
	}
}

func TestCheckToolPermission_MediumRisk_NotInList_AskUser(t *testing.T) {
	ctx := context.Background()
	ctx = SetToolsAllowed(ctx, []string{"view", "write"})

	// Medium-risk tool not in allowed list - should ask user
	allowed, askUser, err := CheckToolPermission(ctx, "download")
	if err != nil {
		t.Errorf("CheckToolPermission() unexpected error = %v", err)
	}
	if allowed {
		t.Errorf("CheckToolPermission(download) = %v, want false (not in allowed list)", allowed)
	}
	if !askUser {
		t.Errorf("CheckToolPermission(download) askUser = %v, want true", askUser)
	}
}

func TestCheckToolPermission_MediumRisk_InList_Allowed(t *testing.T) {
	ctx := context.Background()
	ctx = SetToolsAllowed(ctx, []string{"view", "download", "write"})

	// Medium-risk tool in allowed list - should be allowed without asking
	allowed, askUser, err := CheckToolPermission(ctx, "download")
	if err != nil {
		t.Errorf("CheckToolPermission() unexpected error = %v", err)
	}
	if !allowed {
		t.Errorf("CheckToolPermission(download) = %v, want true (in allowed list)", allowed)
	}
	if askUser {
		t.Errorf("CheckToolPermission(download) askUser = %v, want false", askUser)
	}
}

func TestCheckToolPermission_LowRisk_AlwaysAllowed(t *testing.T) {
	ctx := context.Background()
	ctx = SetToolsAllowed(ctx, []string{"view"})

	// Low-risk tool - always allowed regardless of config
	allowed, askUser, err := CheckToolPermission(ctx, "write")
	if err != nil {
		t.Errorf("CheckToolPermission() unexpected error = %v", err)
	}
	if !allowed {
		t.Errorf("CheckToolPermission(write) = %v, want true (low-risk always allowed)", allowed)
	}
	if askUser {
		t.Errorf("CheckToolPermission(write) askUser = %v, want false", askUser)
	}
}

func TestCheckToolPermission_EmptyAllowedList(t *testing.T) {
	ctx := context.Background()
	ctx = SetToolsAllowed(ctx, []string{})

	// High-risk tool with empty allowed list - should be denied
	allowed, askUser, err := CheckToolPermission(ctx, "bash")
	if err != nil {
		t.Errorf("CheckToolPermission() unexpected error = %v", err)
	}
	if allowed {
		t.Errorf("CheckToolPermission(bash) = %v, want false (empty allowed list)", allowed)
	}
	if askUser {
		t.Errorf("CheckToolPermission(bash) askUser = %v, want false", askUser)
	}
}

func TestCheckToolPermission_AllTools(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		allowed   []string
		wantAllow bool
		wantAsk   bool
	}{
		// High-risk tools
		{"bash denied", "bash", []string{"view"}, false, false},
		{"bash allowed", "bash", []string{"view", "bash"}, true, false},
		{"golang denied", "golang", []string{"view"}, false, false},
		{"golang allowed", "golang", []string{"view", "golang"}, true, false},
		{"lua denied", "lua", []string{"view"}, false, false},
		{"lua allowed", "lua", []string{"view", "lua"}, true, false},
		{"mcp denied", "mcp", []string{"view"}, false, false},
		{"mcp allowed", "mcp", []string{"view", "mcp"}, true, false},

		// Medium-risk tools
		{"download ask", "download", []string{"view"}, false, true},
		{"download allowed", "download", []string{"view", "download"}, true, false},
		{"fetch ask", "fetch", []string{"view"}, false, true},
		{"fetch allowed", "fetch", []string{"view", "fetch"}, true, false},
		{"agentic_fetch ask", "agentic_fetch", []string{"view"}, false, true},
		{"agentic_fetch allowed", "agentic_fetch", []string{"view", "agentic_fetch"}, true, false},

		// Low-risk tools
		{"view always allowed", "view", []string{}, true, false},
		{"write always allowed", "write", []string{}, true, false},
		{"edit always allowed", "edit", []string{}, true, false},
		{"grep always allowed", "grep", []string{}, true, false},
		{"glob always allowed", "glob", []string{}, true, false},
		{"ls always allowed", "ls", []string{}, true, false},
		{"ask_user always allowed", "ask_user", []string{}, true, false},
		{"random always allowed", "random", []string{}, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			ctx = SetToolsAllowed(ctx, tt.allowed)

			allowed, askUser, err := CheckToolPermission(ctx, tt.tool)
			if err != nil {
				t.Errorf("CheckToolPermission() unexpected error = %v", err)
			}
			if allowed != tt.wantAllow {
				t.Errorf("CheckToolPermission(%q) allowed = %v, want %v", tt.tool, allowed, tt.wantAllow)
			}
			if askUser != tt.wantAsk {
				t.Errorf("CheckToolPermission(%q) askUser = %v, want %v", tt.tool, askUser, tt.wantAsk)
			}
		})
	}
}

// ============== AgentToolsAllowedKey Context Helpers Tests ==============

func TestSetToolsAllowed_GetToolsAllowed(t *testing.T) {
	ctx := context.Background()

	// Test with nil
	allowed := GetToolsAllowed(ctx)
	if allowed != nil {
		t.Errorf("GetToolsAllowed() = %v, want nil", allowed)
	}

	// Test with empty list
	tools := []string{"view", "write"}
	ctx = SetToolsAllowed(ctx, tools)
	allowed = GetToolsAllowed(ctx)
	if len(allowed) != 2 || allowed[0] != "view" || allowed[1] != "write" {
		t.Errorf("GetToolsAllowed() = %v, want %v", allowed, tools)
	}

	// Test retrieval returns the same slice reference (by design)
	allowed[0] = "modified"
	allowed2 := GetToolsAllowed(ctx)
	if allowed2[0] != "modified" {
		t.Errorf("GetToolsAllowed() should return same slice reference, got %q want %q", allowed2[0], "modified")
	}
}

func TestGetToolsAllowed_WrongType(t *testing.T) {
	// Test with wrong type in context
	ctx := context.WithValue(context.Background(), AgentToolsAllowedKey, "not-a-slice")
	allowed := GetToolsAllowed(ctx)
	if allowed != nil {
		t.Errorf("GetToolsAllowed() with wrong type = %v, want nil", allowed)
	}
}

func TestGetToolsAllowed_NilSlice(t *testing.T) {
	ctx := SetToolsAllowed(context.Background(), nil)
	allowed := GetToolsAllowed(ctx)
	if allowed != nil {
		t.Errorf("GetToolsAllowed() with nil slice = %v, want nil", allowed)
	}
}

// ============== AskUser Active State Tests ==============

func TestSetAskUserActive_IsAskUserActive(t *testing.T) {
	// Initial state should be inactive
	if IsAskUserActive() {
		t.Errorf("IsAskUserActive() = true, want false (initial state)")
	}

	// Set active
	SetAskUserActive(true)
	if !IsAskUserActive() {
		t.Errorf("IsAskUserActive() = false, want true (after SetAskUserActive(true))")
	}

	// Set inactive
	SetAskUserActive(false)
	if IsAskUserActive() {
		t.Errorf("IsAskUserActive() = true, want false (after SetAskUserActive(false))")
	}
}

func TestSetAskUserActive_MultipleTimes(t *testing.T) {
	SetAskUserActive(true)
	SetAskUserActive(true) // Should remain active
	if !IsAskUserActive() {
		t.Errorf("IsAskUserActive() = false, want true (after multiple SetAskUserActive(true))")
	}

	SetAskUserActive(false)
	SetAskUserActive(false) // Should remain inactive
	if IsAskUserActive() {
		t.Errorf("IsAskUserActive() = true, want false (after multiple SetAskUserActive(false))")
	}
}

// ============== Callback Hook Tests ==============

func TestSetOnAskUserStart_NotifyAskUserStart(t *testing.T) {
	called := false
	SetOnAskUserStart(func() {
		called = true
	})

	NotifyAskUserStart()
	if !called {
		t.Errorf("NotifyAskUserStart() did not call registered callback")
	}
}

func TestSetOnAskUserDone_NotifyAskUserDone(t *testing.T) {
	called := false
	SetOnAskUserDone(func() {
		called = true
	})

	NotifyAskUserDone()
	if !called {
		t.Errorf("NotifyAskUserDone() did not call registered callback")
	}
}

func TestNotifyAskUserStart_NoCallback(t *testing.T) {
	// Reset callbacks
	SetOnAskUserStart(nil)
	SetOnAskUserDone(nil)

	// Should not panic
	NotifyAskUserStart()
	NotifyAskUserDone()
}

func TestSetOnAskUserTUI_TryAskUserTUI(t *testing.T) {
	called := false
	expectedQuestion := "Test question?"
	expectedQtype := "single_choice"
	expectedOpts := []AskUserTUIOption{{Label: "Yes", Value: "y"}}
	expectedAllowAny := false

	SetOnAskUserTUI(func(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (string, bool) {
		called = true
		if question != expectedQuestion {
			t.Errorf("callback question = %q, want %q", question, expectedQuestion)
		}
		if qtype != expectedQtype {
			t.Errorf("callback qtype = %q, want %q", qtype, expectedQtype)
		}
		if len(opts) != len(expectedOpts) || opts[0].Label != expectedOpts[0].Label {
			t.Errorf("callback opts = %v, want %v", opts, expectedOpts)
		}
		if allowAny != expectedAllowAny {
			t.Errorf("callback allowAny = %v, want %v", allowAny, expectedAllowAny)
		}
		return `{"answers":["y"]}`, true
	})

	ctx := context.Background()
	result, ok := TryAskUserTUI(ctx, expectedQuestion, expectedQtype, expectedOpts, expectedAllowAny)
	if !called {
		t.Errorf("TryAskUserTUI() did not call registered callback")
	}
	if !ok {
		t.Errorf("TryAskUserTUI() ok = false, want true")
	}
	if result != `{"answers":["y"]}` {
		t.Errorf("TryAskUserTUI() result = %q, want %q", result, `{"answers":["y"]}`)
	}
}

func TestTryAskUserTUI_NoCallback(t *testing.T) {
	SetOnAskUserTUI(nil)

	ctx := context.Background()
	_, ok := TryAskUserTUI(ctx, "question", "type", nil, false)
	if ok {
		t.Errorf("TryAskUserTUI() ok = true, want false (no callback registered)")
	}
}

func TestTryAskUserTUI_ContextCancelled(t *testing.T) {
	called := false
	SetOnAskUserTUI(func(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (string, bool) {
		called = true
		// Check if context is cancelled
		select {
		case <-ctx.Done():
			return "", false
		default:
			return `{"answers":["y"]}`, true
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, ok := TryAskUserTUI(ctx, "question", "type", nil, false)
	if !called {
		t.Errorf("TryAskUserTUI() did not call registered callback")
	}
	if ok {
		t.Errorf("TryAskUserTUI() with cancelled context ok = true, want false")
	}
}

// ============== normalizeWorkspacePath Tests ==============

func TestNormalizeWorkspacePath(t *testing.T) {
	tests := []struct {
		name          string
		path          string
		workspaceName string
		want          string
	}{
		{"empty path", "", "workspace", ""},
		{"empty workspace", "/workspace/test", "", "/workspace/test"},
		{"exact match", "/workspace", "workspace", "./workspace"},
		{"prefix match", "/workspace/test/file.txt", "workspace", "./workspace/test/file.txt"},
		{"no match", "/other/path", "workspace", "/other/path"},
		{"partial match not at start", "/test/workspace/file", "workspace", "/test/workspace/file"},
		{"different workspace name", "/workspace/test", "work", "/workspace/test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeWorkspacePath(tt.path, tt.workspaceName)
			if got != tt.want {
				t.Errorf("normalizeWorkspacePath(%q, %q) = %q, want %q", tt.path, tt.workspaceName, got, tt.want)
			}
		})
	}
}

// ============== safeTruncateString Tests ==============

func TestSafeTruncateString_EmptyString(t *testing.T) {
	result := safeTruncateString("", 10)
	if result != "" {
		t.Errorf("safeTruncateString(\"\", 10) = %q, want \"\"", result)
	}
}

func TestSafeTruncateString_ShorterThanMaxLen(t *testing.T) {
	result := safeTruncateString("hello", 10)
	if result != "hello" {
		t.Errorf("safeTruncateString(\"hello\", 10) = %q, want \"hello\"", result)
	}
}

func TestSafeTruncateString_ExactlyMaxLen(t *testing.T) {
	result := safeTruncateString("hello", 5)
	if result != "hello" {
		t.Errorf("safeTruncateString(\"hello\", 5) = %q, want \"hello\"", result)
	}
}

func TestSafeTruncateString_LongerThanMaxLen(t *testing.T) {
	result := safeTruncateString("hello world", 5)
	if result != "hello" {
		t.Errorf("safeTruncateString(\"hello world\", 5) = %q, want \"hello\"", result)
	}
}

func TestSafeTruncateString_MaxLenZero(t *testing.T) {
	result := safeTruncateString("hello", 0)
	if result != "" {
		t.Errorf("safeTruncateString(\"hello\", 0) = %q, want \"\"", result)
	}
}

func TestSafeTruncateString_MaxLenOne(t *testing.T) {
	result := safeTruncateString("hello", 1)
	if result != "h" {
		t.Errorf("safeTruncateString(\"hello\", 1) = %q, want \"h\"", result)
	}
}

func TestSafeTruncateString_Unicode(t *testing.T) {
	// Test with multi-byte Unicode characters
	result := safeTruncateString("你好世界", 2)
	if result != "你好" {
		t.Errorf("safeTruncateString(\"你好世界\", 2) = %q, want \"你好\"", result)
	}
}

func TestSafeTruncateString_UnicodeTruncated(t *testing.T) {
	// Ensure we don't split multi-byte characters
	result := safeTruncateString("你好世界", 3)
	if result != "你好世" {
		t.Errorf("safeTruncateString(\"你好世界\", 3) = %q, want \"你好世\"", result)
	}
}

// ============== truncBy Tests ==============

func TestTruncBy_NoTruncation(t *testing.T) {
	result := truncBy(10, 100, 500, 1000)
	if result != "" {
		t.Errorf("truncBy(10, 100, 500, 1000) = %q, want \"\"", result)
	}
}

func TestTruncBy_LinesOnly(t *testing.T) {
	result := truncBy(100, 10, 500, 1000)
	if result != "lines" {
		t.Errorf("truncBy(100, 10, 500, 1000) = %q, want \"lines\"", result)
	}
}

func TestTruncBy_BytesOnly(t *testing.T) {
	result := truncBy(10, 100, 2000, 1000)
	if result != "bytes" {
		t.Errorf("truncBy(10, 100, 2000, 1000) = %q, want \"bytes\"", result)
	}
}

func TestTruncBy_LinesAndBytes(t *testing.T) {
	result := truncBy(100, 10, 2000, 1000)
	if result != "lines+bytes" {
		t.Errorf("truncBy(100, 10, 2000, 1000) = %q, want \"lines+bytes\"", result)
	}
}

func TestTruncBy_NZero(t *testing.T) {
	// When n=0, string should remain unchanged (tested via truncBy with equal values)
	result := truncBy(10, 10, 500, 500)
	if result != "" {
		t.Errorf("truncBy(10, 10, 500, 500) = %q, want \"\" (no truncation)", result)
	}
}

func TestTruncBy_NGreaterThanStringLength(t *testing.T) {
	// When max is greater than total, no truncation
	result := truncBy(10, 100, 500, 10000)
	if result != "" {
		t.Errorf("truncBy(10, 100, 500, 10000) = %q, want \"\" (max > total)", result)
	}
}

func TestTruncBy_EdgeCaseMaxZero(t *testing.T) {
	// When max is 0 and total > 0, should indicate truncation
	result := truncBy(10, 0, 500, 0)
	if result != "lines+bytes" {
		t.Errorf("truncBy(10, 0, 500, 0) = %q, want \"lines+bytes\"", result)
	}
}
