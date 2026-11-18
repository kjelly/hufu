package team

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/waitbudget"
)

func TestInvocationWatchdogExcludesHumanWaitAndResumes(t *testing.T) {
	for _, scenario := range []string{"continue", "decline", "cancel", "wall_deadline", "generic_prompt"} {
		t.Run(scenario, func(t *testing.T) {
			c := newTestCoordinator(t)
			synctest.Test(t, func(t *testing.T) {
				parent, endParent := context.WithCancel(t.Context())
				defer endParent()
				if scenario == "wall_deadline" {
					var end context.CancelFunc
					parent, end = context.WithTimeout(parent, 5*time.Second)
					defer end()
				}
				ctx, cancel := context.WithCancelCause(parent)
				defer cancel(nil)
				c.stallThreshold, c.stallPollInterval = time.Second, 100*time.Millisecond
				w := c.newInvocationWatchdog(ctx, cancel)
				w.start()
				defer w.wait()
				defer cancel(nil)
				tools.SetOnAskUserTUI(func(ctx context.Context, question, _ string, _ []tools.AskUserTUIOption, _ bool) (string, bool) {
					if scenario == "wall_deadline" {
						<-ctx.Done()
					} else {
						time.Sleep(time.Minute)
						if ctx.Err() != nil || w.stalled.Load() {
							t.Fatal("human wait was treated as a stall")
						}
						if scenario == "cancel" {
							endParent()
						}
					}
					if scenario == "continue" {
						// An actual fresh operator code, not a default choice.
						_, code, _ := strings.Cut(question, "\nConfirmation code: ")
						return `{"free_text":"` + code + `"}`, true
					}
					return `{"free_text":""}`, true
				})
				defer tools.SetOnAskUserTUI(nil)
				toolCtx := c.runIdle.Context(ctx)
				var err error
				if scenario == "generic_prompt" {
					_, err = tools.NewAskUserTool().Run(toolCtx, fantasy.ToolCall{Input: `{"question":"Clarify this task","type":"free_text"}`})
				} else {
					err = tools.WaitForHumanVerification(toolCtx, "Verify public source")
				}
				switch scenario {
				case "continue", "generic_prompt":
					if err != nil {
						t.Fatal(err)
					}
				case "decline":
					if !errors.Is(err, tools.ErrHumanVerificationNotConfirmed) {
						t.Fatal(err)
					}
				default:
					if ctx.Err() == nil || errors.Is(context.Cause(ctx), ErrInvocationStalled) {
						t.Fatalf("outer cancellation lost: %v", err)
					}
					return
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if !errors.Is(context.Cause(ctx), ErrInvocationStalled) {
					t.Fatal("watchdog did not resume after the human answer")
				}
			})
		})
	}
}

func TestInvocationWatchdogExcludesWaitsBetweenPolls(t *testing.T) {
	c := newTestCoordinator(t)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		w := c.newInvocationWatchdog(ctx, cancel)
		time.Sleep(time.Second)
		resume := waitbudget.PauseRunIdle(c.runIdle.Context(ctx))
		time.Sleep(time.Hour)
		resume()
		if idle := time.Duration(w.workingTime() - w.activityAt.Load()); idle != time.Second {
			t.Fatalf("completed wait charged to silence: %v", idle)
		}
	})
}
