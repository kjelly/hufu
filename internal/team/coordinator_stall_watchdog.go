package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sync/atomic"
	"time"
)

// stallLogsDir holds goroutine dumps taken when the run appears stalled.
const stallLogsDir = "logs/stall"

// runStallEventType is reported and journaled when the coordinator observes
// no forward-progress signal for longer than the configured threshold.
const runStallEventType = "run_stall_detected"

const (
	defaultStallThreshold    = 5 * time.Minute
	defaultStallPollInterval = 30 * time.Second
	defaultStallMaxDumps     = 6
)

// SetStallWatchdog configures the silent-stall watchdog. threshold is how
// long the run may go without any forward-progress signal (a reported event:
// step, tool_call, tool_result, text, done, error, ...) before it is
// considered stalled; maxDumps caps how many goroutine dumps a single run
// will write, regardless of how many separate stall episodes occur. Zero
// values keep the built-in defaults. Call before Run.
func (c *Coordinator) SetStallWatchdog(threshold time.Duration, maxDumps int) {
	if c == nil {
		return
	}
	if threshold > 0 {
		c.stallThreshold = threshold
	}
	if maxDumps > 0 {
		c.stallMaxDumps = int32(maxDumps)
	}
}

// invocationWatchdog owns all mutable watchdog state for one public
// coordinator invocation. The coordinator only holds a pointer to route
// status events to the currently running invocation; counters and cancellation
// belong to this value and are discarded with it.
type invocationWatchdog struct {
	coordinator *Coordinator
	ctx         context.Context
	cancel      context.CancelCauseFunc
	owner       *invocationOwner
	threshold   time.Duration
	maxDumps    int32
	poll        time.Duration
	activityAt  atomic.Int64
	lastDumpAt  atomic.Int64
	dumps       atomic.Int32
	stalled     atomic.Bool
	done        chan struct{}
}

func (c *Coordinator) newInvocationWatchdog(ctx context.Context, cancel context.CancelCauseFunc) *invocationWatchdog {
	threshold := c.stallThreshold
	if threshold <= 0 {
		threshold = defaultStallThreshold
	}
	maxDumps := c.stallMaxDumps
	if maxDumps <= 0 {
		maxDumps = defaultStallMaxDumps
	}
	poll := c.stallPollInterval
	if poll <= 0 {
		poll = defaultStallPollInterval
	}
	w := &invocationWatchdog{coordinator: c, ctx: ctx, cancel: cancel, threshold: threshold, maxDumps: maxDumps, poll: poll, done: make(chan struct{})}
	w.touch()
	return w
}

func (c *Coordinator) setInvocationWatchdog(w *invocationWatchdog) {
	if c != nil {
		c.invocationWatchdog.Store(w)
	}
}

// touchActivity records that a forward-progress signal was just observed. It
// is called from the single StatusEvent delivery path (see
// SetStatusReporter), so it covers every reporting call site — c.report,
// direct c.reportStatus calls, and captured reportFn closures alike —
// without requiring each call site to remember to touch it individually.
func (c *Coordinator) touchActivity() {
	if c == nil {
		return
	}
	if w := c.invocationWatchdog.Load(); w != nil {
		w.touch()
	}
}

func (w *invocationWatchdog) touch() {
	w.activityAt.Store(time.Now().UnixNano())
}

func (w *invocationWatchdog) start() {
	go func() {
		defer close(w.done)
		w.run()
	}()
}

func (w *invocationWatchdog) wait() {
	if w == nil || w.done == nil {
		return
	}
	<-w.done
}

func (w *invocationWatchdog) run() {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			idle := now.Sub(time.Unix(0, w.activityAt.Load()))
			if idle < w.threshold {
				// Activity resumed; a future stall should be reported
				// immediately rather than waiting out a stale dump timer.
				w.lastDumpAt.Store(0)
				continue
			}
			if w.dumps.Load() >= w.maxDumps {
				continue
			}
			lastDump := w.lastDumpAt.Load()
			if lastDump != 0 && now.Sub(time.Unix(0, lastDump)) < w.threshold {
				continue
			}
			w.lastDumpAt.Store(now.UnixNano())
			n := w.dumps.Add(1)
			w.coordinator.reportStall(idle, n, w.maxDumps)
			// Context cancellation alone is not a hard abort for Fantasy. Kill
			// and reap the Hufu-owned provider proxy first; closing its socket is
			// what releases a provider call that ignores context.
			if w.stalled.CompareAndSwap(false, true) {
				if err := w.owner.abortProviderBoundary(); err != nil {
					w.coordinator.report(w.coordinator.newEvent("error").withMessage("provider hard-abort cleanup failed: " + err.Error()))
				}
				w.cancel(ErrInvocationStalled)
			}
		}
	}
}

func (c *Coordinator) cancelStalledRound() {
	if c == nil {
		return
	}
	todoID := ""
	if current := c.current.Load(); current != nil {
		todoID = current.TodoID
	}
	if todoID == "" {
		return
	}
	c.terminalControlMu.Lock()
	cancel := c.terminalRoundCancels[todoID]
	c.terminalControlMu.Unlock()
	if cancel != nil {
		cancel()
		c.report(c.newEvent("step").withTodoID(todoID).withMessage("silent model round cancelled by stall watchdog; preserving evidence for recovery"))
	}
}

// reportStall surfaces a stall both as a live StatusEvent (for the running
// CLI/UI) and as a durable event_store entry, and writes a full goroutine
// dump to the workspace so a human can see exactly what every goroutine was
// blocked on.
func (c *Coordinator) reportStall(idle time.Duration, dumpNumber, maxDumps int32) {
	agent, task, todoID, stage, tool := "", "", "", "", ""
	if s := c.current.Load(); s != nil {
		agent, task, todoID, stage, tool = s.Agent, s.Task, s.TodoID, s.Stage, s.Tool
	}
	dumpPath, dumpErr := c.writeStallDump(idle)
	msg := fmt.Sprintf("no forward progress observed for %s (dump %d/%d)", idle.Round(time.Second), dumpNumber, maxDumps)
	if dumpErr != nil {
		msg += fmt.Sprintf("; goroutine dump failed: %v", dumpErr)
	} else if dumpPath != "" {
		msg += "; goroutine dump: " + dumpPath
	}
	data := map[string]any{
		"idle_seconds": idle.Round(time.Second).Seconds(),
		"dump_number":  dumpNumber,
		"max_dumps":    maxDumps,
		"dump_path":    dumpPath,
		"last_stage":   stage,
		"last_tool":    tool,
		"last_task":    task,
	}
	c.report(c.newEvent(runStallEventType).withAgent(agent).withTodoID(todoID).withMessage(msg).withData(data))
	_ = c.emitEvent(runStallEventType, "coordinator", todoID, data)
}

// writeStallDump captures every goroutine's stack to a timestamped file under
// the workspace so a stalled run leaves durable, human-readable evidence of
// exactly what it was blocked on (a syscall, a channel, a lock, ...) instead
// of only the fact that time passed with no events.
func (c *Coordinator) writeStallDump(idle time.Duration) (string, error) {
	if c.session == nil || c.session.Workspace == "" {
		return "", nil
	}
	dir := filepath.Join(c.session.Workspace, stallLogsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("stall-%s.txt", time.Now().UTC().Format("20060102T150405.000Z"))
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(f, "idle for %s as of %s\n\n", idle.Round(time.Second), time.Now().UTC().Format(time.RFC3339)); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := pprof.Lookup("goroutine").WriteTo(f, 2); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, nil
}
