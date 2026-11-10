package team

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/waitbudget"
)

func TestTaskCapacityHumanWait(t *testing.T) {
	for _, scenario := range []string{"resume", "cancel", "parallel_tool", "overlapping_waits", "unlimited"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				sem := make(chan struct{}, 1)
				if scenario == "unlimited" {
					sem = nil
				}
				slot, err := acquireSem(ctx, sem)
				if err != nil {
					t.Fatal(err)
				}
				capacity := &taskCapacity{slot: slot, wake: make(chan struct{}, 1)}
				defer capacity.release()
				toolCtx, leave, err := tools.EnterCapacityTool(tools.WithTaskCapacity(ctx, capacity))
				if err != nil {
					t.Fatal(err)
				}
				defer leave()
				_, leaveNested, err := tools.EnterCapacityTool(toolCtx)
				if err != nil {
					t.Fatal(err)
				}
				leaveNested() // gateway nesting must not prevent suspension
				var parallelLeave func()
				if scenario == "parallel_tool" {
					parallelLeave, err = capacity.EnterTool(ctx)
					if err != nil {
						t.Fatal(err)
					}
				}
				resume, err := capacity.Wait(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if parallelLeave != nil {
					if len(sem) != 1 {
						t.Fatal("yielded while another tool was executing")
					}
					parallelLeave()
				}
				if len(sem) != 0 {
					t.Fatal("human wait retained capacity")
				}
				if scenario == "overlapping_waits" {
					anotherResume, err := capacity.Wait(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if err := anotherResume(); err != nil {
						t.Fatal(err)
					}
					if len(sem) != 1 {
						t.Fatal("first resumed operation lacks capacity")
					}
				}
				var occupying semSlot
				if scenario != "overlapping_waits" {
					occupying, err = acquireSem(ctx, sem)
					if err != nil {
						t.Fatal(err)
					}
				}
				defer occupying.release()
				resumed := make(chan error, 1)
				go func() { resumed <- resume() }()
				synctest.Wait()
				if scenario == "resume" || scenario == "cancel" || scenario == "parallel_tool" {
					select {
					case <-resumed:
						t.Fatal("resumed without reacquiring capacity")
					default:
					}
				}
				if scenario == "cancel" {
					cancel()
				}
				occupying.release()
				err = <-resumed
				if (scenario == "cancel") != errors.Is(err, context.Canceled) {
					t.Fatalf("resume error: %v", err)
				}
				if again := resume(); !errors.Is(again, err) {
					t.Fatalf("resume is not idempotent: %v / %v", again, err)
				}
			})
		})
	}
}

func TestTaskCapacityNewToolDuringHumanWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, stop := waitbudget.WithTimeout(t.Context(), time.Second)
		defer stop()
		sem := make(chan struct{}, 1)
		slot, err := acquireSem(ctx, sem)
		if err != nil {
			t.Fatal(err)
		}
		capacity := &taskCapacity{slot: slot, wake: make(chan struct{}, 1)}
		defer capacity.release()
		resume, err := capacity.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		other, err := acquireSem(ctx, sem)
		if err != nil {
			t.Fatal(err)
		}
		defer other.release()
		entered := make(chan struct{})
		toolDone := make(chan struct{})
		go func() {
			leave, err := capacity.EnterTool(ctx)
			if err != nil {
				t.Error(err)
				close(toolDone)
				return
			}
			close(entered)
			leave()
			close(toolDone)
		}()
		synctest.Wait()
		time.Sleep(time.Hour)
		if ctx.Err() != nil {
			t.Fatal("capacity queue consumed working timeout")
		}
		select {
		case <-entered:
			t.Fatal("new tool ran without capacity")
		default:
		}
		other.release()
		<-toolDone
		if err := resume(); err != nil {
			t.Fatal(err)
		}
		if len(sem) != 1 {
			t.Fatal("human continuation lost its capacity")
		}
	})
}

func TestTaskCapacityNativeWaitPreservesParallelWork(t *testing.T) {
	ctx := t.Context()
	sem := make(chan struct{}, 1)
	slot, err := acquireSem(ctx, sem)
	if err != nil {
		t.Fatal(err)
	}
	capacity := &taskCapacity{parent: ctx, slot: slot}
	defer capacity.release()
	leave, err := capacity.EnterTool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer leave()
	resume, err := tools.BeginCapacityWait(tools.WithTaskCapacity(ctx, capacity))
	if err != nil {
		t.Fatal(err)
	}
	if len(sem) != 1 {
		t.Fatal("native wait hid a working parallel tool")
	}
	leave()
	if len(sem) != 0 {
		t.Fatal("native wait did not yield after parallel work stopped")
	}
	if err := resume(); err != nil {
		t.Fatal(err)
	}
	if err := resume(); err != nil {
		t.Fatal(err)
	}
	if len(sem) != 1 {
		t.Fatal("native continuation lacks capacity")
	}
}

func TestTaskCapacityHumanWaitPreservesProviderInvocation(t *testing.T) {
	c := newTestCoordinator(t)
	sem := make(chan struct{}, 1)
	slot, err := acquireSem(t.Context(), sem)
	if err != nil {
		t.Fatal(err)
	}
	capacity := &taskCapacity{parent: t.Context(), slot: slot}
	defer capacity.release()
	ctx := tools.WithTaskCapacity(t.Context(), capacity)
	release, err := (providerRequestAdmission{c: c}).AcquireProviderInvocation(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	resume, err := tools.BeginCapacityWait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sem) != 1 {
		t.Fatal("yielded while a parallel model request was active")
	}
	release()
	if len(sem) != 0 {
		t.Fatal("human wait retained capacity after the model request ended")
	}
	if err := resume(); err != nil {
		t.Fatal(err)
	}
	if len(sem) != 1 {
		t.Fatal("human continuation lacks capacity")
	}
}

func TestTaskCapacityToolCancellationRequiresReentry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent := t.Context()
		toolCtx, cancel := context.WithCancel(parent)
		defer cancel()
		sem := make(chan struct{}, 1)
		slot, err := acquireSem(parent, sem)
		if err != nil {
			t.Fatal(err)
		}
		capacity := &taskCapacity{parent: parent, slot: slot}
		defer capacity.release()
		resume, err := capacity.Wait(toolCtx)
		if err != nil {
			t.Fatal(err)
		}
		other, err := acquireSem(parent, sem)
		if err != nil {
			t.Fatal(err)
		}
		defer other.release()
		cancel()
		done := make(chan error, 1)
		go func() { done <- resume() }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("cancelled tool returned to a live worker without capacity")
		default:
		}
		other.release()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("lost tool cancellation: %v", err)
		}
		if len(sem) != 1 {
			t.Fatal("live worker lacks capacity after tool cancellation")
		}
	})
}

func TestDAGSchedulerHumanWaitRunsIndependentWork(t *testing.T) {
	for _, cancelWaiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "cancelled"}[cancelWaiting], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			prompt := make(chan struct{})
			answer := make(chan struct{})
			otherStarted := make(chan struct{})
			finishOther := make(chan struct{})
			resumed := make(chan struct{})
			finish := sync.OnceFunc(func() { close(finishOther) })
			respond := sync.OnceFunc(func() { close(answer) })
			tools.SetOnAskUserTUI(func(ctx context.Context, _, _ string, _ []tools.AskUserTUIOption, _ bool) (string, bool) {
				close(prompt)
				select {
				case <-answer:
				case <-ctx.Done():
				}
				return `{"answers":["continue"]}`, true
			})
			defer tools.SetOnAskUserTUI(nil)
			var humanID, otherID string
			worker := scriptWorker(scriptedTurn{stream: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
				id, _ := ctx.Value(todoIDKey{}).(string)
				if id == humanID {
					if err := tools.WaitForHumanVerification(ctx, "Verify page"); err != nil {
						return nil, err
					}
					close(resumed)
				} else if id == otherID {
					close(otherStarted)
					select {
					case <-finishOther:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				} else {
					select {
					case <-resumed:
					default:
						return nil, errors.New("conflicting or dependent task started during human wait")
					}
				}
				return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}}, nil
			}})
			c := newTestCoordinator(t, withWorker(worker), withMaxRetries(0))
			c.maxConcurrent = 1
			items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
				{Agent: "worker", Desc: "human verification"},
				{Agent: "worker", Desc: "same browser tab"},
				{Agent: "worker", Desc: "independent source"},
			})
			humanID, otherID = items[0].ID, items[2].ID
			items = append(items, c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "dependent source", DependsOn: []string{humanID}}})...)
			s := mustNewDAGScheduler(t, c, []TaskDef{
				{Agent: "worker", Goal: "human verification", Resources: []ResourceClaim{{Resource: "browser:verification", Mode: ResourceExclusive}}},
				{Agent: "worker", Goal: "same browser tab", Resources: []ResourceClaim{{Resource: "browser:verification", Mode: ResourceExclusive}}},
				{Agent: "worker", Goal: "independent source"},
				{Agent: "worker", Goal: "dependent source", DependsOn: []int{0}},
			}, items, nil)
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() { defer close(finished); _, err := s.run(ctx); done <- err }()
			t.Cleanup(func() {
				cancel()
				respond()
				finish()
				select {
				case <-finished:
				case <-time.After(20 * time.Second):
					t.Error("scheduler cleanup did not drain")
				}
			})
			await := func(ch <-chan struct{}) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(20 * time.Second):
					t.Fatal("scheduler did not progress")
				}
			}
			await(prompt)
			await(otherStarted) // max-concurrent=1: this requires releasing the human slot
			respond()
			select {
			case <-resumed:
				t.Fatal("human task resumed while independent task occupied the only slot")
			case <-time.After(20 * time.Millisecond):
			}
			if cancelWaiting {
				cancel()
			}
			finish()
			select {
			case err := <-done:
				if cancelWaiting != errors.Is(err, context.Canceled) {
					t.Fatalf("scheduler error = %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("scheduler did not drain")
			}
			if len(s.sem) != 0 || len(s.teamSlots) != 0 || s.inProgress != 0 || len(s.activeResources) != 0 {
				t.Fatal("scheduler leaked capacity or resources")
			}
			if cancelWaiting {
				if s.states[3] != TaskPending || s.states[1] != TaskPending {
					t.Fatal("cancelled scheduler launched dependent")
				}
			} else if s.states[0] != TaskDone || s.states[3] != TaskDone || s.states[1] != TaskDone {
				t.Fatalf("tasks not completed: %v", s.states)
			}
		})
	}
}

func TestDAGSchedulerHumanWaitWithSharedBackend(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	prompt, answer := make(chan struct{}), make(chan struct{})
	queued, resumed := make(chan struct{}), make(chan struct{})
	backend := make(chan struct{}, 1)
	respond := sync.OnceFunc(func() { close(answer) })
	tools.SetOnAskUserTUI(func(ctx context.Context, _, _ string, _ []tools.AskUserTUIOption, _ bool) (string, bool) {
		close(prompt)
		select {
		case <-answer:
		case <-ctx.Done():
		}
		return `{"answers":["continue"]}`, true
	})
	defer tools.SetOnAskUserTUI(nil)
	var humanID string
	worker := scriptWorker(scriptedTurn{stream: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		id, _ := ctx.Value(todoIDKey{}).(string)
		if id != humanID {
			close(queued)
		}
		slot, err := acquireSem(ctx, backend)
		if err != nil {
			return nil, err
		}
		defer slot.release()
		if id == humanID {
			if err := tools.WaitForHumanVerification(ctx, "Verify page"); err != nil {
				return nil, err
			}
			close(resumed)
		}
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}}, nil
	}})
	c := newTestCoordinator(t, withWorker(worker), withMaxRetries(0))
	c.maxConcurrent = 1
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "human holds backend"},
		{Agent: "worker", Desc: "queued behind backend"},
	})
	humanID = items[0].ID
	s := mustNewDAGScheduler(t, c, []TaskDef{
		{Agent: "worker", Goal: "human holds backend"},
		{Agent: "worker", Goal: "queued behind backend"},
	}, items, nil)
	finished := make(chan struct{})
	var runErr error
	go func() { defer close(finished); _, runErr = s.run(ctx) }()
	t.Cleanup(func() {
		cancel()
		respond()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Error("scheduler did not drain")
		}
	})
	for _, ready := range []chan struct{}{prompt, queued} {
		select {
		case <-ready:
		case <-time.After(20 * time.Second):
			t.Fatal("task did not reach shared-backend wait")
		}
	}
	// The second task already owns the sole team slot, while the first owns
	// the backend. Its resource wait must yield before the human can resume.
	respond()
	select {
	case <-resumed:
	case <-time.After(20 * time.Second):
		t.Fatal("team/backend capacity cycle deadlocked human continuation")
	}
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("scheduler did not finish")
	}
	if runErr != nil || s.states[0] != TaskDone || s.states[1] != TaskDone {
		t.Fatalf("results: %v, %v", runErr, s.states)
	}
	if len(s.sem) != 0 || len(backend) != 0 {
		t.Fatal("capacity leaked")
	}
}
