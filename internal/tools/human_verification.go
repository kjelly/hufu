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

// WaitForHumanVerification accepts only a fresh, explicitly typed code. Neither
// a default option, a stale answer nor auto-approval can certify verification.
func WaitForHumanVerification(ctx context.Context, question string) (resultErr error) {
	if request, _ := ctx.Value(humanVerificationRequestKey{}).(HumanVerificationObservation); request.RequestID == "" {
		ctx = WithHumanVerificationRequest(ctx, HumanVerificationObservation{RequestID: rand.Text(), Attempt: 1, Trigger: "explicit"})
	}
	defer func() {
		if resultErr != nil {
			status := "failed"
			if errors.Is(resultErr, ErrHumanVerificationDeclined) || errors.Is(resultErr, context.Canceled) {
				status = "cancelled"
			}
			recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			resultErr = errors.Join(resultErr, ObserveHumanVerification(recordCtx, status))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if IsUnattended(ctx) || (!IsInteractiveEnvironment() && onAskUserTUI == nil) {
		return errors.New("browser verification needs an interactive human; no answer was supplied")
	}
	ctx = context.WithValue(ctx, AutoApproveKey, false)
	ctx = context.WithValue(ctx, humanVerificationKey{}, true)
	code := rand.Text()[:8]
	prompt := fmt.Sprintf("%s\nAfter completing verification in Chrome, type the confirmation code below. Enter alone does not confirm. Type cancel to stop.\nConfirmation code: %s", question, code)
	resume := waitbudget.PauseRunIdle(ctx)
	defer resume()
	for range 3 {
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
		if len(answer.Answers) == 0 && strings.TrimSpace(answer.Free) == code {
			return ObserveHumanVerification(ctx, "confirmed")
		}
		if err := ObserveHumanVerification(ctx, "answer_rejected"); err != nil {
			return err
		}
		prompt = fmt.Sprintf("Confirmation not accepted. Complete verification in Chrome and enter this request's code, or type cancel.\n%s", prompt)
	}
	return errors.New("browser verification was not confirmed: three invalid answers; no page verification was accepted")
}
