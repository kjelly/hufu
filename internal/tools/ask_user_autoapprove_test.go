package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
)

func TestChooseAutoApproveAskUserResponse_PicksSafeOption(t *testing.T) {
	args := askUserArgs{
		Question: "remove the file?",
		Options: []askOption{
			{Label: "Delete", Value: "delete"},
			{Label: "No", Value: "no"},
		},
	}

	resp, ok := chooseAutoApproveAskUserResponse(args)
	if !ok {
		t.Fatal("expected a safe option to be auto-selected")
	}
	if len(resp.Answers) != 1 || resp.Answers[0] != "no" {
		t.Fatalf("expected the safe refusal option, got %+v", resp.Answers)
	}
}

func TestChooseAutoApproveAskUserResponse_RejectsDangerousOnly(t *testing.T) {
	args := askUserArgs{
		Question: "which destructive action should we take?",
		Options: []askOption{
			{Label: "Delete", Value: "delete"},
			{Label: "Overwrite", Value: "overwrite"},
		},
	}

	if resp, ok := chooseAutoApproveAskUserResponse(args); ok {
		t.Fatalf("expected no auto-selection, got %+v", resp)
	}
}

func TestExecuteAskUser_AutoApproveSkipsPromptForSafeOptions(t *testing.T) {
	SetOnAskUserTUI(func(context.Context, string, string, []AskUserTUIOption, bool) (string, bool) {
		t.Fatal("TUI should not be invoked when a safe option can be auto-approved")
		return "", false
	})
	defer SetOnAskUserTUI(nil)
	SetOnNeedsHuman(nil)

	ctx := context.WithValue(context.Background(), AutoApproveKey, true)
	input, _ := json.Marshal(map[string]any{
		"question": "remove the file?",
		"type":     "single_choice",
		"options": []map[string]string{
			{"label": "Delete", "value": "delete"},
			{"label": "No", "value": "no"},
		},
	})

	resp, err := executeAskUser(ctx, fantasy.ToolCall{ID: "1", Name: "ask_user", Input: string(input)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("expected auto-approved response, got error: %s", resp.Content)
	}

	var parsed AskUserResponse
	if err := json.Unmarshal([]byte(resp.Content), &parsed); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(parsed.Answers) != 1 || parsed.Answers[0] != "no" {
		t.Fatalf("expected safe answer 'no', got %+v", parsed.Answers)
	}
}
