package improver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

func TestControllerExecuteFencesProviderAndDrainsCancellation(t *testing.T) {
	f := newControllerFixture(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bound := make(chan struct{})
	releaseDrain := make(chan struct{})
	done := make(chan error, 1)
	executor := &fakeExecutor{execute: func(ctx context.Context, request StageRequest, bind func(paired.RuntimeIdentity) error) (Settlement, error) {
		result := f.settlement(t, request.Lease, "cancelled", true, 21)
		if err := bind(result.Identity); err != nil {
			return Settlement{}, err
		}
		// This is the simulated first provider call. Parent binding/dispatch
		// authorization must already be durable and readable by another handle.
		state := f.read(t)
		attempt := state.Stages[request.StageID].Attempts[0]
		if attempt.Identity == nil || *attempt.Identity != result.Identity || attempt.State != "running" {
			t.Error("provider reached before durable identity")
		}
		close(bound)
		<-ctx.Done()
		<-releaseDrain
		return result, ctx.Err()
	}}
	go func() { done <- f.controller.Execute(ctx, "probe_0001", executor) }()
	<-bound
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before drain: %v", err)
	default:
	}
	// Wait for the cancellation callback with a bounded test deadline; no
	// scheduling assumptions about which goroutine sees cancellation first.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for f.read(t).StopReason == "" {
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatal("admission was not stopped")
		}
	}
	if _, err := f.controller.Admit(t.Context(), "probe_0002"); err == nil {
		t.Fatal("cancelled run admitted")
	}
	close(releaseDrain)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result: %v", err)
	}
	state := f.read(t)
	if state.Budget.ActiveStages != 0 || state.Budget.SpentTokens != 21 || state.StopReason != "cancel_requested" {
		t.Fatalf("%+v", state)
	}
}

func TestControllerBindingFailureMakesZeroProviderCalls(t *testing.T) {
	f := newControllerFixture(t, false)
	journal := &faultJournal{StoreJournal: f.journal, kind: eventBound}
	controller, err := NewController(journal, f.objects, f.scope, f.plan, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	var providerCalls atomic.Int64
	executor := &fakeExecutor{execute: func(_ context.Context, request StageRequest, bind func(paired.RuntimeIdentity) error) (Settlement, error) {
		result := f.settlement(t, request.Lease, "passed", true, 10)
		if err := bind(result.Identity); err != nil {
			return Settlement{}, err
		}
		providerCalls.Add(1)
		return result, nil
	}}
	if err := controller.Execute(t.Context(), "probe_0001", executor); err == nil {
		t.Fatal("binding failure hidden")
	}
	if providerCalls.Load() != 0 {
		t.Fatal("provider called despite binding failure")
	}
	state := f.read(t)
	if state.Stages["probe_0001"].State != "blocked" || state.Budget.ReservedTokens != 200 {
		t.Fatalf("%+v", state)
	}
}

func TestControllerStartAuthorizationIsSingleUseEvenAtSameClock(t *testing.T) {
	f := newControllerFixture(t, false)
	lease := f.bound(t, "probe_0001", true)
	if err := f.controller.Start(t.Context(), lease); err == nil {
		t.Fatal("repeated dispatch was authorized")
	}
}

type headConflictJournal struct {
	StoreJournal
	conflicts int
}

func (j *headConflictJournal) AppendAtHead(ctx context.Context, event team.RunEvent, head string) (team.RunEvent, error) {
	if j.conflicts == 0 && event.Type == eventAdmitted {
		j.conflicts++
		if _, err := j.StoreJournal.Append(ctx, team.RunEvent{Actor: "other", Type: "unrelated", Payload: []byte(`{}`)}); err != nil {
			return team.RunEvent{}, err
		}
	}
	return j.StoreJournal.AppendAtHead(ctx, event, head)
}

func TestControllerRecomputesAfterHeadConflict(t *testing.T) {
	f := newControllerFixture(t, false)
	journal := &headConflictJournal{StoreJournal: f.journal}
	c, err := NewController(journal, f.objects, f.scope, f.plan, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Admit(t.Context(), "probe_0001"); err != nil {
		t.Fatal(err)
	}
	state := f.read(t)
	if journal.conflicts != 1 || len(state.Stages["probe_0001"].Attempts) != 1 || state.Budget.ReservedTokens != 200 {
		t.Fatalf("%+v", state)
	}
}

func TestControllerFailedCompletionCommitDoesNotSettle(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_commit", true: "lost_ack"}[after], func(t *testing.T) {
			f := newControllerFixture(t, false)
			lease := f.bound(t, "probe_0001", true)
			result := f.settlement(t, lease, "passed", true, 30)
			journal := &faultJournal{StoreJournal: f.journal, kind: eventCompleted, after: after}
			c, err := NewController(journal, f.objects, f.scope, f.plan, f.clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Complete(t.Context(), lease, result); err == nil {
				t.Fatal("commit failure hidden")
			}
			state := f.read(t)
			want := int64(0)
			if after {
				want = 30
			}
			if state.Budget.SpentTokens != want {
				t.Fatalf("spent=%d", state.Budget.SpentTokens)
			}
			if err := c.Complete(t.Context(), lease, result); err != nil {
				t.Fatal(err)
			}
			if got := f.read(t).Budget.SpentTokens; got != 30 {
				t.Fatalf("spent=%d", got)
			}
		})
	}
}

func TestControllerRequiredEnvelopeProtectsBudget(t *testing.T) {
	f := newControllerFixture(t, true)
	if got := f.read(t).Budget.ReservedTokens; got != 300 {
		t.Fatalf("initial required=%d", got)
	}
	lease := f.bound(t, "probe_0001", true)
	if err := f.controller.Complete(t.Context(), lease, f.settlement(t, lease, "passed", true, 100)); err != nil {
		t.Fatal(err)
	}
	if err := f.controller.Reserve(t.Context(), "domain"); err != nil {
		t.Fatal(err)
	}
	if err := f.controller.Reserve(t.Context(), "composition"); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t).Budget.ReservedTokens; got != 600 {
		t.Fatalf("reserved=%d", got)
	}
	if err := f.controller.Reserve(t.Context(), "reflection"); err != nil {
		t.Fatal(err)
	}
	if err := f.controller.Transition(t.Context(), "reflection", 0, "skipped_budget", "optional_budget"); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t).Budget.ReservedTokens; got != 600 {
		t.Fatalf("optional release=%d", got)
	}
	// A failed proposal keeps its required probes reserved, so retry cannot
	// consume their allocation; any retry needs additional free capacity.
	proposal := f.bound(t, "proposal_procedure", true)
	if err := f.controller.Complete(t.Context(), proposal, f.settlement(t, proposal, "transient_transport", true, 100)); err != nil {
		t.Fatal(err)
	}
	if got := f.read(t).Budget; got.SpentTokens != 200 || got.ReservedTokens != 500 {
		t.Fatalf("%+v", got)
	}
}

func TestControllerOptionalAndRetryCannotSpendRequiredEnvelope(t *testing.T) {
	f := newControllerFixture(t, true, 500)
	lease := f.bound(t, "probe_0001", true)
	if err := f.controller.Complete(t.Context(), lease, f.settlement(t, lease, "transient_transport", true, 100)); err != nil {
		t.Fatal(err)
	}
	if err := f.controller.Reserve(t.Context(), "domain"); err != nil {
		t.Fatal(err)
	}
	before := f.read(t)
	for _, group := range []string{"composition", "reflection"} {
		if err := f.controller.Reserve(t.Context(), group); err == nil {
			t.Fatalf("%s stole required budget", group)
		}
	}
	if _, err := f.controller.Admit(t.Context(), "probe_0001"); err == nil {
		t.Fatal("retry stole another variant's required envelope")
	}
	after := f.read(t)
	if before.Head != after.Head || after.Budget.SpentTokens != 100 || after.Budget.ReservedTokens != 400 {
		t.Fatalf("failed admission changed ledger: %+v", after)
	}
}

func TestControllerFinishRequiresSettlementAndSyntheticDecision(t *testing.T) {
	f := newControllerFixture(t, false)
	report, err := f.objects.PutJSON("experiment_report", map[string]string{"decision": "simulation_only"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.controller.Finish(t.Context(), report, "simulation_only"); err == nil {
		t.Fatal("unsettled run finished")
	}
	for _, stage := range []string{"probe_0001", "probe_0002"} {
		lease := f.bound(t, stage, true)
		if err := f.controller.Complete(t.Context(), lease, f.settlement(t, lease, "passed", true, 30)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.controller.Finish(t.Context(), report, "eligible_for_review"); err == nil {
		t.Fatal("fixture acquired eligibility")
	}
	if err := f.controller.Finish(t.Context(), report, "simulation_only"); err != nil {
		t.Fatal(err)
	}
	if err := f.controller.Finish(t.Context(), report, "simulation_only"); err != nil {
		t.Fatal(err)
	}
}
