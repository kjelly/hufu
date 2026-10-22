package improver

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

type controllerFixture struct {
	controller *Controller
	journal    StoreJournal
	objects    *ObjectStore
	clock      *testClock
	plan       paired.ExperimentPlan
	scope      LedgerScope
	workspace  string
}

func newControllerFixture(t *testing.T, refine bool, pipelineLimit ...int64) controllerFixture {
	t.Helper()
	workspace := t.TempDir()
	objects, err := OpenObjects(workspace, "controlled", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Close() })
	put := func(kind string) paired.ArtifactRef {
		ref, err := objects.PutJSON(kind, map[string]string{"kind": kind})
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	gain := int64(1)
	plan := paired.ExperimentPlan{
		Kind: "paired_experiment_plan", SchemaVersion: 1, ExperimentID: "controlled",
		ParentWorkspaceID: "parent-workspace", ParentBranchID: "parent-branch",
		Mode: "manual", ExecutionMode: "fixture", Profile: "structured_only",
		BinaryDigest: paired.BytesDigest([]byte("build")), Config: put("config"), Fixture: put("fixture"),
		BaselineSnapshot: put("team_snapshot"), ComparisonContextDigest: paired.BytesDigest([]byte("context")),
		PolicyProjectionVersion: 1,
		Models: map[string]paired.ResolvedModel{
			"executor": {Backend: "fixture", Model: "scripted", Generation: map[string]string{}, PolicyDigest: paired.BytesDigest([]byte("policy"))},
		},
		Limits:    paired.Limits{PerStageTokens: 100, PerStageSeconds: 60, PipelineTokens: 1000, PipelineSeconds: 600, MaxProbeRuns: 8, StageRetries: 1},
		Objective: paired.Objective{Kind: "quality_gain", MinPassGain: &gain},
		Slots:     []paired.PairSlot{{Variant: "baseline", Case: "case_a", Replicate: 1}, {Variant: "candidate_1", Case: "case_a", Replicate: 1}},
	}
	if refine {
		plan.Mode = "refine"
		plan.Slots = plan.Slots[:1]
		for _, name := range []string{"procedure", "domain", "composition"} {
			plan.Slots = append(plan.Slots, paired.PairSlot{Variant: name, Case: "case_a", Replicate: 1})
		}
		for _, role := range []string{"proposer", "composer", "reflector"} {
			plan.Models[role] = plan.Models["executor"]
		}
	}
	if len(pipelineLimit) > 0 {
		plan.Limits.PipelineTokens = pipelineLimit[0]
	}
	scope := LedgerScope{ExperimentID: plan.ExperimentID, WorkspaceID: plan.ParentWorkspaceID, BranchID: plan.ParentBranchID, RunID: "parent-run", SessionID: "parent-session"}
	clock := &testClock{now: time.Now().UTC()}
	store, err := team.NewEventStore(workspace, scope.RunID, scope.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	journal := StoreJournal{Store: store}
	controller, err := NewController(journal, objects, scope, plan, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Create(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, slot := range plan.Slots {
		if refine && slot.Variant != "baseline" {
			continue
		}
		binding := VariantBinding{Slot: slot.Variant, Snapshot: plan.BaselineSnapshot, Digest: paired.BytesDigest([]byte(slot.Variant)), Origin: "frozen_input"}
		if err := controller.BindVariant(t.Context(), binding); err != nil {
			t.Fatal(err)
		}
	}
	return controllerFixture{controller, journal, objects, clock, plan, scope, workspace}
}

func (f controllerFixture) read(t *testing.T) LedgerState {
	t.Helper()
	state, err := f.controller.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return state
}
func (f controllerFixture) bound(t *testing.T, stage string, start bool) Lease {
	t.Helper()
	lease, err := f.controller.Admit(t.Context(), stage)
	if err != nil {
		t.Fatal(err)
	}
	id := paired.RuntimeIdentity{InvocationID: lease.ChildWorkspaceID + "-inv", RunID: lease.ChildWorkspaceID + "-run", BranchID: "child-branch"}
	if err := f.controller.Bind(t.Context(), lease, id); err != nil {
		t.Fatal(err)
	}
	if start {
		if err := f.controller.Start(t.Context(), lease); err != nil {
			t.Fatal(err)
		}
	}
	return lease
}
func (f controllerFixture) settlement(t *testing.T, lease Lease, outcome string, usage bool, tokens int64) Settlement {
	t.Helper()
	id := paired.RuntimeIdentity{InvocationID: lease.ChildWorkspaceID + "-inv", RunID: lease.ChildWorkspaceID + "-run", BranchID: "child-branch"}
	ref, err := f.objects.PutJSON("stage_evidence", map[string]any{"identity": id, "outcome": outcome, "usage_known": usage, "tokens_used": tokens})
	if err != nil {
		t.Fatal(err)
	}
	return Settlement{Identity: id, Evidence: ref, Outcome: outcome, UsageKnown: usage, Tokens: tokens, Stopped: true}
}

func TestControllerEnvelopeSettlementAndIdempotency(t *testing.T) {
	f := newControllerFixture(t, false)
	if got := f.read(t).Budget.ReservedTokens; got != 200 {
		t.Fatalf("initial reserved=%d", got)
	}
	first := f.bound(t, "probe_0001", true)
	if got := f.read(t).Budget.ReservedTokens; got != 200 {
		t.Fatalf("allocation double counted: %d", got)
	}
	if _, err := f.controller.Admit(t.Context(), "probe_0002"); err == nil {
		t.Fatal("parallel probe admitted")
	}
	result := f.settlement(t, first, "failed", true, 60)
	if err := f.controller.Complete(t.Context(), first, result); err != nil {
		t.Fatal(err)
	}
	before := f.read(t)
	if err := f.controller.Complete(t.Context(), first, result); err != nil {
		t.Fatal(err)
	}
	after := f.read(t)
	if !reflect.DeepEqual(before, after) || after.Budget.SpentTokens != 60 || after.Budget.ReservedTokens != 100 {
		t.Fatalf("duplicate settlement: %+v", after.Budget)
	}
	if _, err := f.controller.Admit(t.Context(), first.StageID); !errors.Is(err, ErrAlreadyAdmitted) {
		t.Fatalf("functional failure retried: %v", err)
	}
	second := f.bound(t, "probe_0002", true)
	if err := f.controller.Complete(t.Context(), second, f.settlement(t, second, "passed", true, 40)); err != nil {
		t.Fatal(err)
	}
	got := f.read(t)
	if got.Budget.SpentTokens != 100 || got.Budget.ReservedTokens != 0 || got.Budget.ActiveStages != 0 {
		t.Fatalf("settlement=%+v", got.Budget)
	}
}

func TestControllerUnknownOverspendAndRetry(t *testing.T) {
	t.Run("unknown retains then reconciles", func(t *testing.T) {
		f := newControllerFixture(t, false)
		lease := f.bound(t, "probe_0001", true)
		unknown := f.settlement(t, lease, "passed", false, 12)
		if err := f.controller.Complete(t.Context(), lease, unknown); err != nil {
			t.Fatal(err)
		}
		if got := f.read(t).Budget; got.ReservedTokens != 200 || got.SpentTokens != 12 || got.UnknownStages != 1 {
			t.Fatalf("%+v", got)
		}
		if _, err := f.controller.Admit(t.Context(), "probe_0002"); err == nil {
			t.Fatal("unknown usage admitted more work")
		}
		known := f.settlement(t, lease, "passed", true, 45)
		if err := f.controller.Complete(t.Context(), lease, known); err != nil {
			t.Fatal(err)
		}
		if got := f.read(t).Budget; got.SpentTokens != 45 || got.ReservedTokens != 100 || got.UnknownStages != 0 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("overspend recorded", func(t *testing.T) {
		f := newControllerFixture(t, false)
		lease := f.bound(t, "probe_0001", true)
		if err := f.controller.Complete(t.Context(), lease, f.settlement(t, lease, "passed", true, 1100)); err != nil {
			t.Fatal(err)
		}
		state := f.read(t)
		if state.Budget.SpentTokens != 1100 || !state.ResourceOver {
			t.Fatalf("%+v", state)
		}
		if _, err := f.controller.Admit(t.Context(), "probe_0002"); err == nil {
			t.Fatal("overspend admitted")
		}
	})
	t.Run("transport only bounded retry", func(t *testing.T) {
		f := newControllerFixture(t, false)
		first := f.bound(t, "probe_0001", true)
		if err := f.controller.Complete(t.Context(), first, f.settlement(t, first, "transient_transport", true, 20)); err != nil {
			t.Fatal(err)
		}
		second := f.bound(t, "probe_0001", true)
		if second.Generation != 2 || second.ChildWorkspaceID == first.ChildWorkspaceID {
			t.Fatal("retry identity reused")
		}
		if err := f.controller.Complete(t.Context(), first, f.settlement(t, first, "passed", true, 21)); err == nil {
			t.Fatal("stale completion accepted")
		}
		if err := f.controller.Complete(t.Context(), second, f.settlement(t, second, "transient_transport", true, 30)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.controller.Admit(t.Context(), "probe_0001"); err == nil {
			t.Fatal("third attempt admitted")
		}
		if got := f.read(t); got.Budget.SpentTokens != 50 || got.ProbeRuns != 2 {
			t.Fatalf("%+v", got)
		}
	})
}

func TestControllerCompletionConflictAndIdentity(t *testing.T) {
	f := newControllerFixture(t, false)
	lease := f.bound(t, "probe_0001", true)
	result := f.settlement(t, lease, "passed", true, 20)
	wrong := result
	wrong.Identity.RunID = "unrelated-run"
	if err := f.controller.Complete(t.Context(), lease, wrong); err == nil {
		t.Fatal("wrong runtime identity accepted")
	}
	if err := f.controller.Complete(t.Context(), lease, result); err != nil {
		t.Fatal(err)
	}
	conflict := f.settlement(t, lease, "failed", true, 20)
	if err := f.controller.Complete(t.Context(), lease, conflict); !errors.Is(err, ErrCompletionConflict) {
		t.Fatalf("conflict=%v", err)
	}
	state := f.read(t)
	if state.Stages[lease.StageID].State != "blocked" || state.Budget.SpentTokens != 20 || state.Budget.UnknownStages != 1 {
		t.Fatalf("%+v", state)
	}
	if _, err := f.controller.Admit(t.Context(), "probe_0002"); err == nil {
		t.Fatal("conflict admitted further work")
	}
}

func TestControllerClockAndResume(t *testing.T) {
	for _, delta := range []time.Duration{-time.Minute, 11 * time.Minute} {
		t.Run(delta.String(), func(t *testing.T) {
			f := newControllerFixture(t, false)
			before := f.read(t)
			f.clock.advance(delta)
			resumed, err := NewController(f.journal, f.objects, f.scope, f.plan, f.clock)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resumed.Admit(t.Context(), "probe_0001"); err == nil {
				t.Fatal("invalid clock admitted")
			}
			after, err := resumed.Read(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := "deadline_exceeded"
			if delta < 0 {
				want = "clock_rollback"
			}
			if after.StopReason != want || after.Deadline != before.Deadline || after.Budget != before.Budget {
				t.Fatalf("%+v", after)
			}
		})
	}
}

type fakeExecutor struct {
	execute   func(context.Context, StageRequest, func(paired.RuntimeIdentity) error) (Settlement, error)
	reconcile func(context.Context, StageRequest) (Reconciliation, error)
	calls     atomic.Int64
}

func (f *fakeExecutor) Execute(ctx context.Context, request StageRequest, bind func(paired.RuntimeIdentity) error) (Settlement, error) {
	f.calls.Add(1)
	return f.execute(ctx, request, bind)
}
func (f *fakeExecutor) Reconcile(ctx context.Context, request StageRequest) (Reconciliation, error) {
	return f.reconcile(ctx, request)
}

func TestControllerCrashReconciliationNeverRedispatches(t *testing.T) {
	for _, point := range []string{"admitted", "bound", "running", "child_complete"} {
		t.Run(point, func(t *testing.T) {
			f := newControllerFixture(t, false)
			var lease Lease
			var err error
			if point == "admitted" {
				lease, err = f.controller.Admit(t.Context(), "probe_0001")
			} else {
				lease = f.bound(t, "probe_0001", point != "bound")
			}
			if err != nil {
				t.Fatal(err)
			}
			executor := &fakeExecutor{reconcile: func(_ context.Context, request StageRequest) (Reconciliation, error) {
				if request.Lease != lease {
					t.Fatal("reconcile changed identity")
				}
				if point == "child_complete" {
					return Reconciliation{State: "completed", Settlement: f.settlement(t, lease, "passed", true, 35)}, nil
				}
				return Reconciliation{State: "unknown"}, nil
			}}
			if err := f.controller.Reconcile(t.Context(), executor); (err == nil) != (point == "child_complete") {
				t.Fatalf("reconcile=%v", err)
			}
			if point == "child_complete" {
				if err := f.controller.Reconcile(t.Context(), executor); err != nil {
					t.Fatal(err)
				}
				if got := f.read(t); got.Budget.SpentTokens != 35 || got.Budget.ReservedTokens != 100 {
					t.Fatalf("%+v", got.Budget)
				}
			} else if got := f.read(t); got.Budget.ReservedTokens != 200 || got.Stages["probe_0001"].State != "blocked" {
				t.Fatalf("%+v", got)
			}
			events, err := f.journal.ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReplayLedger(events, f.scope, f.plan); err != nil {
				t.Fatal(err)
			}
			if executor.calls.Load() != 0 {
				t.Fatal("reconcile/replay dispatched")
			}
		})
	}
}

type faultJournal struct {
	StoreJournal
	kind   string
	after  bool
	failed bool
}

func (j *faultJournal) AppendAtHead(ctx context.Context, event team.RunEvent, head string) (team.RunEvent, error) {
	if event.Type != j.kind || j.failed {
		return j.StoreJournal.AppendAtHead(ctx, event, head)
	}
	j.failed = true
	if j.after {
		if _, err := j.StoreJournal.AppendAtHead(ctx, event, head); err != nil {
			return team.RunEvent{}, err
		}
	}
	return team.RunEvent{}, errors.New("injected acknowledgement failure")
}

func TestControllerAmbiguousCommitAndOrphan(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			f := newControllerFixture(t, false)
			journal := &faultJournal{StoreJournal: f.journal, kind: eventAdmitted, after: after}
			c, err := NewController(journal, f.objects, f.scope, f.plan, f.clock)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Admit(t.Context(), "probe_0001"); err == nil {
				t.Fatal("injected failure hidden")
			}
			state := f.read(t)
			want := "planned"
			if after {
				want = "admitted"
			}
			if state.Stages["probe_0001"].State != want {
				t.Fatalf("%+v", state)
			}
			if after {
				if _, err := c.Admit(t.Context(), "probe_0001"); !errors.Is(err, ErrAlreadyAdmitted) {
					t.Fatalf("lost ack redispatch: %v", err)
				}
			}
			// An artifact without a committed event cannot contribute evidence.
			if _, err := f.objects.PutJSON("stage_evidence", map[string]string{"outcome": "passed"}); err != nil {
				t.Fatal(err)
			}
			if got := f.read(t); got.Budget.SpentTokens != 0 {
				t.Fatal("orphan counted")
			}
		})
	}
}

func TestControllerConcurrentProcesses(t *testing.T) {
	if workspace := os.Getenv("HUFU_PAIRED_TEST_WORKSPACE"); workspace != "" {
		raw, err := os.ReadFile(filepath.Join(workspace, "fixture.json"))
		if err != nil {
			t.Fatal(err)
		}
		var fixture struct {
			Plan  paired.ExperimentPlan
			Scope LedgerScope
		}
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatal(err)
		}
		objects, err := OpenObjects(workspace, fixture.Scope.ExperimentID, false)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = objects.Close() }()
		store, err := team.NewEventStore(workspace, fixture.Scope.RunID, fixture.Scope.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		controller, err := NewController(StoreJournal{store}, objects, fixture.Scope, fixture.Plan, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = controller.Admit(t.Context(), "probe_0001")
		if err != nil && !errors.Is(err, ErrAlreadyAdmitted) {
			t.Fatal(err)
		}
		return
	}
	f := newControllerFixture(t, false)
	raw, err := paired.CanonicalJSON(struct {
		Plan  paired.ExperimentPlan
		Scope LedgerScope
	}{f.plan, f.scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workspace, "fixture.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	commands := make([]*exec.Cmd, 2)
	outputs := make([]bytes.Buffer, 2)
	for i := range commands {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestControllerConcurrentProcesses$")
		command.Env = append(os.Environ(), "HUFU_PAIRED_TEST_WORKSPACE="+f.workspace)
		command.Stdout, command.Stderr = &outputs[i], &outputs[i]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands[i] = command
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("process %d: %v\n%s", i, err, outputs[i].String())
		}
	}
	state := f.read(t)
	if len(state.Stages["probe_0001"].Attempts) != 1 || state.Budget.ReservedTokens != 200 || state.ProbeRuns != 1 {
		t.Fatalf("%+v", state)
	}
}
