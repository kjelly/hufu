//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json/v2"
	"errors"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/waitbudget"
)

type humanVerificationKey struct{}

// WaitForHumanVerification requires a real answer. Auto-approval and unattended
// defaults cannot certify that an operator completed an external verification.
// It uses the shared CLI/TUI prompt, including its interactive timeout accounting.
func WaitForHumanVerification(ctx context.Context, question string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if IsUnattended(ctx) || (!IsInteractiveEnvironment() && onAskUserTUI == nil) {
		return errors.New("browser verification needs a human; unavailable in unattended mode")
	}
	ctx = context.WithValue(ctx, AutoApproveKey, false)
	ctx = context.WithValue(ctx, humanVerificationKey{}, true)
	input, err := json.Marshal(map[string]any{
		"question": question, "type": "single_choice",
		"options": []map[string]string{
			{"label": "Verification complete — continue", "value": "continue"},
			{"label": "Unable to verify — cancel", "value": "cancel"},
		},
	})
	if err != nil {
		return err
	}
	resume := waitbudget.PauseRunIdle(ctx)
	defer resume()
	response, err := NewAskUserTool().Run(ctx, fantasy.ToolCall{Name: "ask_user", Input: string(input)})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if response.IsError {
		return errors.New(response.Content)
	}
	var answer struct {
		Answers []string `json:"answers"`
	}
	if err := json.Unmarshal([]byte(response.Content), &answer); err != nil {
		return err
	}
	if len(answer.Answers) != 1 || answer.Answers[0] != "continue" {
		return errors.New("browser verification was not confirmed by the operator")
	}
	return nil
}
