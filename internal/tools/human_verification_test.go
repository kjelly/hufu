//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kjelly/hufu/internal/waitbudget"
)

func TestHumanAndProviderWaitsShareWorkingBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetInteractiveWait()
		defer resetInteractiveWait()
		ctx, cancel := WithInteractiveAwareTimeout(t.Context(), 10*time.Second)
		defer cancel()
		resume := waitbudget.Pause(ctx)
		time.Sleep(time.Minute)
		SetAskUserActive(true)
		child, end := WithInteractiveAwareTimeout(ctx, time.Second)
		defer end()
		time.Sleep(time.Minute)
		resume()
		time.Sleep(time.Minute)
		SetAskUserActive(false)
		deadline, _ := ctx.Deadline()
		if remaining := time.Until(deadline); remaining != 10*time.Second {
			t.Fatalf("overlapping waits extended working budget: %v", remaining)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !errors.Is(child.Err(), context.DeadlineExceeded) || ctx.Err() != nil {
			t.Fatal("budget created during wait was not paused")
		}
		time.Sleep(9 * time.Second)
		synctest.Wait()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("working budget failed to resume")
		}
	})
}

func TestHumanVerificationNeverUsesNonInteractiveSelector(t *testing.T) {
	previous := os.Stdin
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = null
	defer func() { os.Stdin = previous; _ = null.Close() }()
	SetOnAskUserTUI(func(context.Context, string, string, []AskUserTUIOption, bool) (string, bool) { return "", false })
	defer SetOnAskUserTUI(nil)
	ctx := context.WithValue(t.Context(), AskUserChoiceSelectorKey, AskUserChoiceSelector(func(context.Context, string, string, []AskUserTUIOption, bool) (AskUserResponse, error) {
		t.Fatal("a model selector cannot confirm manual verification")
		return AskUserResponse{Answers: []string{"continue"}}, nil
	}))
	if err := WaitForHumanVerification(ctx, "Verify"); err == nil {
		t.Fatal("non-interactive verification succeeded")
	}
}

func TestOverlappingTUIWaitsCountOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetInteractiveWait()
		defer resetInteractiveWait()
		first, second := make(chan struct{}), make(chan struct{})
		SetOnAskUserTUI(func(_ context.Context, question, _ string, _ []AskUserTUIOption, _ bool) (string, bool) {
			if question == "first" {
				<-first
			} else {
				<-second
			}
			return `{"answers":["continue"]}`, true
		})
		defer SetOnAskUserTUI(nil)
		ctx, cancel := WithInteractiveAwareTimeout(t.Context(), time.Second)
		defer cancel()
		finished := make(chan struct{}, 2)
		for _, question := range []string{"first", "second"} {
			go func() { _, _ = TryAskUserTUI(ctx, question, "single_choice", nil, false); finished <- struct{}{} }()
		}
		synctest.Wait()
		time.Sleep(time.Hour)
		close(first)
		<-finished
		if !IsAskUserActive() {
			t.Fatal("first completion ended overlapping wait")
		}
		time.Sleep(time.Hour)
		close(second)
		<-finished
		if IsAskUserActive() || InteractiveWaitTotal() != 2*time.Hour || ctx.Err() != nil {
			t.Fatalf("active=%t wait=%v error=%v", IsAskUserActive(), InteractiveWaitTotal(), ctx.Err())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("working budget did not resume")
		}
	})
}

func TestHumanVerificationRequiresActualConfirmation(t *testing.T) {
	for _, scenario := range []string{"continue", "cancel", "unattended", "invalid", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			called := false
			SetOnAskUserTUI(func(ctx context.Context, _, _ string, _ []AskUserTUIOption, _ bool) (string, bool) {
				called = true
				if IsAutoApprove(ctx) {
					t.Error("manual verification can auto-approve")
				}
				if scenario == "invalid" {
					return `{"free_text":"continue"}`, true
				}
				return `{"answers":["` + scenario + `"]}`, true
			})
			defer SetOnAskUserTUI(nil)
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			callCtx := context.WithValue(ctx, AutoApproveKey, true)
			if scenario == "unattended" {
				callCtx = context.WithValue(callCtx, UnattendedKey, true)
			}
			if scenario == "cancelled" {
				stop()
			}
			err := WaitForHumanVerification(callCtx, "Verify in Chrome")
			if (err == nil) != (scenario == "continue") {
				t.Fatalf("error=%v", err)
			}
			if scenario == "unattended" && called {
				t.Fatal("unattended run prompted")
			}
		})
	}
}

func TestHumanVerificationExcludesWaitAndPreservesOuterLimits(t *testing.T) {
	for _, scenario := range []string{"continue", "cancelled", "wall_deadline"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				resetInteractiveWait()
				defer resetInteractiveWait()
				outer, stop := context.WithCancel(t.Context())
				defer stop()
				var parent context.Context = outer
				var idle waitbudget.RunIdleClock
				total, endTotal := idle.WithTimeout(parent, 2*time.Second)
				defer endTotal()
				parent = total
				if scenario == "wall_deadline" {
					wall, cancel := context.WithTimeout(outer, 5*time.Second)
					defer cancel()
					parent = wall
				}
				coordinator, end := WithInteractiveAwareTimeout(parent, 2*time.Second)
				defer end()
				worker, endWorker := WithInteractiveAwareTimeout(coordinator, time.Second)
				defer endWorker()
				SetOnAskUserTUI(func(ctx context.Context, _, _ string, _ []AskUserTUIOption, _ bool) (string, bool) {
					if scenario == "continue" {
						time.Sleep(time.Hour)
						if total.Err() != nil || idle.Elapsed() != time.Hour {
							t.Fatal("human verification consumed total duration budget")
						}
						if worker.Err() != nil || coordinator.Err() != nil {
							t.Fatal("human wait consumed working time")
						}
					} else {
						if scenario == "cancelled" {
							stop()
						}
						<-ctx.Done()
					}
					return `{"answers":["continue"]}`, true
				})
				defer SetOnAskUserTUI(nil)
				err := WaitForHumanVerification(worker, "Verify in Chrome")
				if scenario == "continue" {
					if err != nil {
						t.Fatal(err)
					}
					time.Sleep(time.Second)
					synctest.Wait()
					if !errors.Is(worker.Err(), context.DeadlineExceeded) || coordinator.Err() != nil {
						t.Fatal("working timeout did not resume")
					}
				} else if err == nil {
					t.Fatal("outer limit ignored")
				}
			})
		})
	}
}
