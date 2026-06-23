package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestIsUnattended(t *testing.T) {
	if IsUnattended(context.Background()) {
		t.Error("plain context should not be unattended")
	}
	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	if !IsUnattended(ctx) {
		t.Error("context with UnattendedKey=true should be unattended")
	}
}

// Unattended permission semantics, independent of ambient TTY state: the
// allowlist is honoured without prompting (deny-by-default for the rest).
func TestCheckToolPermission_UnattendedFallthrough(t *testing.T) {
	allow := []string{"bash"}
	ctxUnattended := context.WithValue(
		context.WithValue(context.Background(), AgentToolsAllowedKey, allow),
		UnattendedKey, true,
	)

	if allowed, _, _ := CheckToolPermission(ctxUnattended, "bash"); !allowed {
		t.Error("unattended should allow an allowlisted tool")
	}
	// Not in allowlist → still denied even when unattended (deny-by-default).
	if allowed, _, _ := CheckToolPermission(ctxUnattended, "sudo"); allowed {
		t.Error("unattended must still deny non-allowlisted tools")
	}
	// ask_user is always allowed regardless.
	if allowed, _, _ := CheckToolPermission(ctxUnattended, "ask_user"); !allowed {
		t.Error("ask_user must always be allowed")
	}
}

func TestUnattendedAskUserResponse_ChoicePicksFirst(t *testing.T) {
	called := false
	SetOnNeedsHuman(func(string) { called = true })
	defer SetOnNeedsHuman(nil)

	args := askUserArgs{
		Question: "pick one",
		Options:  []askOption{{Label: "Yes", Value: "y"}, {Label: "No", Value: "n"}},
	}
	resp, err := unattendedAskUserResponse(args, "single_choice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("choice question should return an answer, got error: %s", resp.Content)
	}
	var parsed askResponseType
	if err := json.Unmarshal([]byte(resp.Content), &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(parsed.Answers) != 1 || parsed.Answers[0] != "y" {
		t.Errorf("expected first option value 'y', got %v", parsed.Answers)
	}
	if !called {
		t.Error("needs-human hook should have fired")
	}
}

func TestUnattendedAskUserResponse_FreeTextErrors(t *testing.T) {
	SetOnNeedsHuman(nil)
	args := askUserArgs{Question: "describe the fix"}
	resp, err := unattendedAskUserResponse(args, "free_text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !resp.IsError {
		t.Error("free-text with no human should return an error response")
	}
	if !strings.Contains(resp.Content, "unattended") {
		t.Errorf("error should explain unattended mode, got %q", resp.Content)
	}
}

func TestExecuteAskUser_UnattendedDoesNotBlock(t *testing.T) {
	SetOnNeedsHuman(nil)
	// Isolate from any TUI hook leaked by other tests so we exercise the
	// unattended CLI path rather than a stubbed TUI dialog.
	SetOnAskUserTUI(nil)
	defer SetOnAskUserTUI(nil)
	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	input, _ := json.Marshal(map[string]any{
		"question": "continue?",
		"type":     "single_choice",
		"options":  []map[string]string{{"label": "go", "value": "go"}},
	})
	// Must return promptly without reading stdin.
	resp, err := executeAskUser(ctx, fantasy.ToolCall{ID: "1", Name: "ask_user", Input: string(input)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("expected an auto-answer, got error: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "go") {
		t.Errorf("expected default answer 'go', got %q", resp.Content)
	}
}
