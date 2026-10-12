package conversation

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePort blocks each turn until released (or cancelled) when block is set.
type fakePort struct {
	mu         sync.Mutex
	block      bool
	ignoreCtx  bool
	prompts    []string
	turnIDs    []string
	wrapUps    int
	resets     int
	switches   []string
	switchErr  error
	started    chan string
	release    chan struct{}
	turnCtxErr []error
}

func newFakePort(block bool) *fakePort {
	return &fakePort{block: block, started: make(chan string, 256), release: make(chan struct{}, 256)}
}

func (f *fakePort) RunTurn(ctx context.Context, turnID, prompt string) TurnResult {
	f.mu.Lock()
	f.prompts = append(f.prompts, prompt)
	f.turnIDs = append(f.turnIDs, turnID)
	block, ignoreCtx := f.block, f.ignoreCtx
	f.mu.Unlock()
	f.started <- turnID
	if block {
		if ignoreCtx {
			<-f.release
		} else {
			select {
			case <-f.release:
			case <-ctx.Done():
				f.mu.Lock()
				f.turnCtxErr = append(f.turnCtxErr, ctx.Err())
				f.mu.Unlock()
				return TurnResult{Err: ctx.Err(), Cancelled: true}
			}
		}
	}
	return TurnResult{Answer: "answer:" + prompt}
}

func (f *fakePort) RequestWrapUp() {
	f.mu.Lock()
	f.wrapUps++
	f.mu.Unlock()
}

func (f *fakePort) ResetContext(context.Context) error {
	f.mu.Lock()
	f.resets++
	f.mu.Unlock()
	return nil
}

func (f *fakePort) SwitchTeam(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switches = append(f.switches, name)
	return f.switchErr
}

func (f *fakePort) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

func newTestController(t *testing.T, port *fakePort, cfg Config) (*Controller, context.CancelFunc) {
	t.Helper()
	var seq int
	var seqMu sync.Mutex
	cfg.NewTurnID = func() string {
		seqMu.Lock()
		defer seqMu.Unlock()
		seq++
		return fmt.Sprintf("turn-%d", seq)
	}
	ctrl := New(port, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	ctrl.Start(ctx)
	t.Cleanup(func() {
		ctrl.Close(time.Second)
		cancel()
	})
	return ctrl, cancel
}

func waitStarted(t *testing.T, port *fakePort) string {
	t.Helper()
	select {
	case id := <-port.started:
		return id
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
		return ""
	}
}

// collect drains events until want events arrived or the stream closed.
func collect(t *testing.T, ctrl *Controller, want int) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(2 * time.Second)
	for len(got) < want {
		select {
		case ev, ok := <-ctrl.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("collected %d events, want %d: %#v", len(got), want, got)
		}
	}
	return got
}

func prompt(id, text string) Command {
	return Command{ID: id, Kind: CommandSubmitPrompt, Text: text}
}

func TestSubmitSinglePromptRunsOnce(t *testing.T) { // C01
	port := newFakePort(false)
	ctrl, _ := newTestController(t, port, Config{})
	ack := ctrl.Submit(prompt("c1", "hello"))
	if ack.Status != AckAccepted || ack.TurnID != "turn-1" {
		t.Fatalf("ack = %+v, want accepted turn-1", ack)
	}
	events := collect(t, ctrl, 2)
	started, ok := events[0].(TurnStarted)
	if !ok || started.TurnID != "turn-1" || started.CommandID != "c1" || started.Prompt != "hello" {
		t.Fatalf("first event = %#v, want TurnStarted", events[0])
	}
	settled, ok := events[1].(TurnSettled)
	if !ok || settled.Result.TurnID != "turn-1" || settled.Result.Answer != "answer:hello" {
		t.Fatalf("second event = %#v, want TurnSettled", events[1])
	}
	if port.runCount() != 1 {
		t.Fatalf("RunTurn calls = %d, want 1", port.runCount())
	}
}

func TestSubmitDuplicateIDReturnsOriginalAck(t *testing.T) { // C02
	port := newFakePort(true)
	ctrl, _ := newTestController(t, port, Config{})
	first := ctrl.Submit(prompt("same", "hello"))
	waitStarted(t, port)
	second := ctrl.Submit(prompt("same", "hello again"))
	if second != first {
		t.Fatalf("duplicate ack = %+v, want original %+v", second, first)
	}
	port.release <- struct{}{}
	collect(t, ctrl, 2)
	if port.runCount() != 1 {
		t.Fatalf("RunTurn calls = %d, want 1", port.runCount())
	}
}

func TestSubmitQueueCapacity(t *testing.T) { // C03
	port := newFakePort(true)
	ctrl, _ := newTestController(t, port, Config{QueueCapacity: 1})
	if ack := ctrl.Submit(prompt("a", "one")); ack.Status != AckAccepted {
		t.Fatalf("first ack = %+v", ack)
	}
	waitStarted(t, port)
	if ack := ctrl.Submit(prompt("b", "two")); ack.Status != AckQueued || ack.Position != 1 {
		t.Fatalf("second ack = %+v, want queued at 1", ack)
	}
	busy := ctrl.Submit(prompt("c", "three"))
	if busy.Status != AckBusy || busy.Reason == "" {
		t.Fatalf("third ack = %+v, want busy with reason", busy)
	}
}

func TestSubmitBurstAccountsForEveryCommand(t *testing.T) { // C04
	port := newFakePort(true)
	ctrl, _ := newTestController(t, port, Config{QueueCapacity: 16})
	counts := map[AckStatus]int{}
	for i := range 100 {
		counts[ctrl.Submit(prompt(fmt.Sprintf("burst-%d", i), "work")).Status]++
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if total != 100 || counts[AckAccepted] != 1 || counts[AckQueued] != 16 || counts[AckBusy] != 83 {
		t.Fatalf("ack counts = %v, want 1 accepted, 16 queued, 83 busy", counts)
	}
	for range counts[AckAccepted] + counts[AckQueued] {
		waitStarted(t, port)
		port.release <- struct{}{}
	}
	collect(t, ctrl, 2*(counts[AckAccepted]+counts[AckQueued]))
	if got := port.runCount(); got != counts[AckAccepted]+counts[AckQueued] {
		t.Fatalf("RunTurn calls = %d, want %d", got, counts[AckAccepted]+counts[AckQueued])
	}
}

func TestSubmitRejectsInvalidCommands(t *testing.T) { // C05
	tests := []struct {
		name   string
		cmd    Command
		reason string
	}{
		{name: "missing id", cmd: Command{Kind: CommandSubmitPrompt, Text: "x"}, reason: "command id"},
		{name: "empty prompt", cmd: prompt("e1", ""), reason: "empty"},
		{name: "whitespace prompt", cmd: prompt("e2", " \n\t"), reason: "empty"},
		{name: "invalid utf8", cmd: prompt("e3", "bad\xff"), reason: "UTF-8"},
		{name: "too large", cmd: prompt("e4", strings.Repeat("x", 11)), reason: "limit is 10 bytes"},
		{name: "unknown kind", cmd: Command{ID: "e5", Kind: "launch"}, reason: "unknown command kind"},
		{name: "switch without target", cmd: Command{ID: "e6", Kind: CommandSwitchTeam}, reason: "team name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl, _ := newTestController(t, newFakePort(false), Config{MaxPromptBytes: 10})
			ack := ctrl.Submit(tt.cmd)
			if ack.Status != AckRejected || !strings.Contains(ack.Reason, tt.reason) {
				t.Fatalf("ack = %+v, want rejected containing %q", ack, tt.reason)
			}
		})
	}
}

func TestCancelTurnKeepsSessionAndRunsNextPrompt(t *testing.T) { // C06
	port := newFakePort(true)
	ctrl, _ := newTestController(t, port, Config{})
	ctrl.Submit(prompt("a", "one"))
	waitStarted(t, port)
	ctrl.Submit(prompt("b", "two"))
	cancelAck := ctrl.Submit(Command{ID: "cancel-1", Kind: CommandCancelTurn})
	if cancelAck.Status != AckAccepted || cancelAck.TurnID != "turn-1" {
		t.Fatalf("cancel ack = %+v", cancelAck)
	}
	if second := waitStarted(t, port); second != "turn-2" {
		t.Fatalf("next turn = %q, want turn-2", second)
	}
	port.release <- struct{}{}
	events := collect(t, ctrl, 4)
	first := events[1].(TurnSettled).Result
	if !first.Cancelled || !errors.Is(first.Err, context.Canceled) {
		t.Fatalf("first result = %+v, want cancelled", first)
	}
	if second := events[3].(TurnSettled).Result; second.Cancelled || second.Answer != "answer:two" {
		t.Fatalf("second result = %+v, want completed", second)
	}
	if ack := ctrl.Submit(prompt("c", "three")); ack.Status != AckAccepted {
		t.Fatalf("session unusable after cancel: %+v", ack)
	}
}

func TestControlCommandsRequireActiveTurn(t *testing.T) { // C07
	ctrl, _ := newTestController(t, newFakePort(false), Config{})
	for _, kind := range []CommandKind{CommandCancelTurn, CommandWrapUp} {
		ack := ctrl.Submit(Command{ID: "idle-" + string(kind), Kind: kind})
		if ack.Status != AckRejected || ack.Reason != "no active turn" {
			t.Fatalf("%s ack = %+v, want rejected", kind, ack)
		}
	}
}

func TestWrapUpDelegatesWithoutCancelling(t *testing.T) { // C08
	port := newFakePort(true)
	ctrl, _ := newTestController(t, port, Config{})
	ctrl.Submit(prompt("a", "one"))
	waitStarted(t, port)
	if ack := ctrl.Submit(Command{ID: "wrap", Kind: CommandWrapUp}); ack.Status != AckAccepted {
		t.Fatalf("wrap ack = %+v", ack)
	}
	port.release <- struct{}{}
	events := collect(t, ctrl, 2)
	if result := events[1].(TurnSettled).Result; result.Cancelled {
		t.Fatalf("wrap-up cancelled the turn: %+v", result)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.wrapUps != 1 {
		t.Fatalf("RequestWrapUp calls = %d, want 1", port.wrapUps)
	}
}

func TestCloseDropsQueuedAndCancelsActive(t *testing.T) { // C09
	port := newFakePort(true)
	ctrl, _ := newTestController(t, port, Config{})
	ctrl.Submit(prompt("a", "one"))
	waitStarted(t, port)
	ctrl.Submit(prompt("b", "two"))
	ctrl.Submit(prompt("c", "three"))
	report := ctrl.Close(2 * time.Second)
	if report.TimedOut || len(report.DroppedTurnIDs) != 2 {
		t.Fatalf("report = %+v, want 2 dropped and no timeout", report)
	}
	var events []Event
	for ev := range ctrl.Events() {
		events = append(events, ev)
	}
	if _, ok := events[len(events)-1].(Closed); !ok {
		t.Fatalf("last event = %#v, want Closed", events[len(events)-1])
	}
	dropped, ok := events[len(events)-2].(QueueDropped)
	if !ok || strings.Join(dropped.TurnIDs, ",") != "turn-2,turn-3" {
		t.Fatalf("QueueDropped = %#v", events[len(events)-2])
	}
	if settled := events[1].(TurnSettled).Result; !settled.Cancelled {
		t.Fatalf("active turn not cancelled: %+v", settled)
	}
	if port.runCount() != 1 {
		t.Fatalf("queued prompts executed: %d runs", port.runCount())
	}
}

func TestCloseTimeoutReportsAndDoesNotLeak(t *testing.T) { // C10
	baseline := runtime.NumGoroutine()
	port := newFakePort(true)
	port.ignoreCtx = true
	ctrl := New(port, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl.Start(ctx)
	ctrl.Submit(prompt("a", "stuck"))
	waitStarted(t, port)
	report := ctrl.Close(50 * time.Millisecond)
	if !report.TimedOut {
		t.Fatalf("report = %+v, want timeout", report)
	}
	port.release <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("goroutines = %d, baseline %d: actor leaked", got, baseline)
	}
}

func TestSubmitAfterCloseIsRejected(t *testing.T) { // C11
	ctrl, _ := newTestController(t, newFakePort(false), Config{})
	ctrl.Close(time.Second)
	ctrl.Close(time.Second) // idempotent
	if ack := ctrl.Submit(prompt("late", "hello")); ack.Status != AckRejected || ack.Reason != "session closing" {
		t.Fatalf("ack = %+v, want rejected session closing", ack)
	}
}

func TestSubmitIsSafeConcurrently(t *testing.T) { // C12
	port := newFakePort(false)
	ctrl, _ := newTestController(t, port, Config{QueueCapacity: 64})
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ack := ctrl.Submit(prompt(fmt.Sprintf("p-%d", i), "work"))
			if ack.Status == AckAccepted || ack.Status == AckQueued {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	collect(t, ctrl, 2*admitted)
	if got := port.runCount(); got != admitted {
		t.Fatalf("RunTurn calls = %d, want %d", got, admitted)
	}
}

func TestResetAndSwitchRunOnlyWhenIdle(t *testing.T) {
	port := newFakePort(true)
	port.switchErr = errors.New("team missing")
	ctrl, _ := newTestController(t, port, Config{})
	ctrl.Submit(prompt("a", "one"))
	waitStarted(t, port)
	busy := ctrl.Submit(Command{ID: "reset-busy", Kind: CommandResetContext})
	if busy.Status != AckRejected || !strings.HasPrefix(busy.Reason, "busy") {
		t.Fatalf("reset while running = %+v, want busy rejection", busy)
	}
	port.release <- struct{}{}
	collect(t, ctrl, 2)

	if ack := ctrl.Submit(Command{ID: "reset", Kind: CommandResetContext}); ack.Status != AckAccepted {
		t.Fatalf("reset ack = %+v", ack)
	}
	done := collect(t, ctrl, 1)[0].(ControlDone)
	if done.Kind != CommandResetContext || done.Err != nil {
		t.Fatalf("reset done = %+v", done)
	}
	if ack := ctrl.Submit(Command{ID: "switch", Kind: CommandSwitchTeam, Target: "ops"}); ack.Status != AckAccepted {
		t.Fatalf("switch ack = %+v", ack)
	}
	switched := collect(t, ctrl, 1)[0].(ControlDone)
	if switched.Target != "ops" || switched.Err == nil {
		t.Fatalf("switch done = %+v, want error for missing team", switched)
	}
}

func TestSessionContextCancellationClosesController(t *testing.T) {
	port := newFakePort(true)
	ctrl, cancel := newTestController(t, port, Config{})
	ctrl.Submit(prompt("a", "one"))
	waitStarted(t, port)
	ctrl.Submit(prompt("b", "two"))
	cancel()
	var last Event
	for ev := range ctrl.Events() {
		last = ev
	}
	if _, ok := last.(Closed); !ok {
		t.Fatalf("last event = %#v, want Closed", last)
	}
	if ack := ctrl.Submit(prompt("c", "three")); ack.Status != AckRejected {
		t.Fatalf("ack after session cancel = %+v", ack)
	}
}
