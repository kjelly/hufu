package tools

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

type resourceCapacityProbe struct {
	waited chan struct{}
	resume func() error
}

func (p *resourceCapacityProbe) EnterTool(context.Context) (func(), error) { return func() {}, nil }
func (p *resourceCapacityProbe) Wait(context.Context) (func() error, error) {
	close(p.waited)
	return p.resume, nil
}

func TestTaskResourceCapacityWait(t *testing.T) {
	for _, scenario := range []string{"available", "contended", "cancelled", "reentry_failed"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				gate := make(chan struct{}, 1)
				if scenario != "available" {
					gate <- struct{}{}
				}
				resumes := 0
				denied := errors.New("task admission cancelled")
				probe := &resourceCapacityProbe{waited: make(chan struct{}), resume: func() error {
					resumes++
					if scenario == "reentry_failed" {
						return denied
					}
					return nil
				}}
				done := make(chan error, 1)
				go func() { done <- AcquireTaskResource(WithTaskCapacity(ctx, probe), gate) }()
				synctest.Wait()
				if scenario != "available" {
					select {
					case <-probe.waited:
					default:
						t.Fatal("tab waiter did not yield task capacity")
					}
					if resumes != 0 {
						t.Fatal("resumed before acquiring the tab")
					}
					if scenario == "cancelled" {
						cancel()
					} else {
						<-gate
					}
				}
				err := <-done
				switch scenario {
				case "cancelled":
					if !errors.Is(err, context.Canceled) || resumes != 1 {
						t.Fatalf("cancelled wait: %v, resumes=%d", err, resumes)
					}
				case "reentry_failed":
					if !errors.Is(err, denied) || len(gate) != 0 {
						t.Fatalf("failed re-entry leaked the tab: %v", err)
					}
				default:
					if err != nil || len(gate) != 1 {
						t.Fatalf("tab acquisition: %v", err)
					}
				}
			})
		})
	}
}
