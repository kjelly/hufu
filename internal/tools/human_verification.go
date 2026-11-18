//go:build linux || darwin

package tools

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/waitbudget"
)

type humanVerificationKey struct{}

// ErrHumanVerificationDeclined marks an explicit operator cancellation. It must
// not be turned into another automatic or model-triggered prompt.
var ErrHumanVerificationDeclined = errors.New("browser verification cancelled by the operator")

// ErrHumanVerificationNotConfirmed is a negative answer, not cancellation or
// failed I/O. Browser adapters expose it as an unavailable, non-citable source.
var ErrHumanVerificationNotConfirmed = errors.New("browser verification was not confirmed")

// ErrHumanVerificationUnavailable reports the absence of an interactive operator.
// It does not imply a policy denial or an explicit operator cancellation.
var ErrHumanVerificationUnavailable = errors.New("browser verification needs an interactive human; no answer was supplied")

// WaitForHumanVerification accepts only a fresh, explicitly typed code. Neither
// a default option, a stale answer nor auto-approval can certify verification.
func WaitForHumanVerification(ctx context.Context, question string) (resultErr error) {
	if request, _ := ctx.Value(humanVerificationRequestKey{}).(HumanVerificationObservation); request.RequestID == "" {
		ctx = WithHumanVerificationRequest(ctx, HumanVerificationObservation{RequestID: rand.Text(), Attempt: 1, Trigger: "explicit"})
	}
	defer func() {
		if resultErr != nil {
			status := "failed"
			if resultErr == ErrHumanVerificationNotConfirmed {
				status = "declined"
			}
			if errors.Is(resultErr, ErrHumanVerificationDeclined) || errors.Is(resultErr, context.Canceled) {
				status = "cancelled"
			}
			recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := ObserveHumanVerification(recordCtx, status); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if IsUnattended(ctx) || (!IsInteractiveEnvironment() && onAskUserTUI == nil) {
		return ErrHumanVerificationUnavailable
	}
	ctx = context.WithValue(ctx, AutoApproveKey, false)
	ctx = context.WithValue(ctx, humanVerificationKey{}, true)
	code := rand.Text()[:8]
	instruction := "After completing verification in Chrome, type the confirmation code below. Enter or any other answer declines verification of this source. Type cancel to stop."
	if HumanVerificationRequiresAuthentication(ctx) {
		instruction = "Would you like to log in to read this source? If so, log in yourself in Chrome and return to the source you want Hufu to read, then type the confirmation code below to hand control back. Never enter credentials here. Press Enter or give any other answer to skip login and use another source. Type cancel to stop."
	}
	prompt := fmt.Sprintf("%s\n%s\nConfirmation code: %s", question, instruction, code)
	resume := waitbudget.PauseRunIdle(ctx)
	defer resume()
	if err := ObserveHumanVerification(ctx, "prompt_requested"); err != nil {
		return err
	}
	input, err := json.Marshal(map[string]any{"question": prompt, "type": "free_text"})
	if err != nil {
		return err
	}
	response, err := NewAskUserTool().Run(ctx, fantasy.ToolCall{Name: "ask_user", Input: string(input)})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if IsInteractiveAbortRequested() {
		return ErrHumanVerificationDeclined
	}
	if response.IsError {
		return errors.New(response.Content)
	}
	var answer AskUserResponse
	if err := json.Unmarshal([]byte(response.Content), &answer); err != nil {
		return err
	}
	if answer.Cancelled {
		return ErrHumanVerificationDeclined
	}
	if len(answer.Answers) == 0 && strings.TrimSpace(answer.Free) == "cancel" {
		return ErrHumanVerificationDeclined
	}
	if !answer.Declined && len(answer.Answers) == 0 && strings.TrimSpace(answer.Free) == code {
		return ObserveHumanVerification(ctx, "confirmed")
	}
	return ErrHumanVerificationNotConfirmed
}
