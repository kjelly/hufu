package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"
)

// stallLogsDir holds goroutine dumps taken when the run appears stalled.
const stallLogsDir = "logs/stall"

// runStallEventType is reported and journaled when the coordinator observes
// no forward-progress signal for longer than the configured threshold. It is
// diagnostic only: nothing is aborted or retried because of it.
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

// touchActivity records that a forward-progress signal was just observed. It
// is called from the single StatusEvent delivery path (see
// SetStatusReporter), so it covers every reporting call site — c.report,
// direct c.reportStatus calls, and captured reportFn closures alike —
// without requiring each call site to remember to touch it individually.
func (c *Coordinator) touchActivity() {
	if c == nil {
		return
	}
	c.stallActivityAt.Store(time.Now().UnixNano())
}

// startStallWatchdog begins polling for silent stalls: periods with no
// forward-progress signal at all, as opposed to a single slow tool call
// (which itself reports a tool_call event before it blocks). It self-starts
// at most once per coordinator and stops when ctx is done.
func (c *Coordinator) startStallWatchdog(ctx context.Context) {
	if c == nil {
		return
	}
	c.stallWatchdogOnce.Do(func() {
		c.touchActivity()
		go c.runStallWatchdog(ctx)
	})
}

func (c *Coordinator) runStallWatchdog(ctx context.Context) {
	threshold := c.stallThreshold
	if threshold <= 0 {
		threshold = defaultStallThreshold
	}
	maxDumps := c.stallMaxDumps
	if maxDumps <= 0 {
		maxDumps = defaultStallMaxDumps
	}
	ticker := time.NewTicker(defaultStallPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			idle := now.Sub(time.Unix(0, c.stallActivityAt.Load()))
			if idle < threshold {
				// Activity resumed; a future stall should be reported
				// immediately rather than waiting out a stale dump timer.
				c.stallLastDumpAt.Store(0)
				continue
			}
			if c.stallDumps.Load() >= maxDumps {
				continue
			}
			lastDump := c.stallLastDumpAt.Load()
			if lastDump != 0 && now.Sub(time.Unix(0, lastDump)) < threshold {
				continue
			}
			c.stallLastDumpAt.Store(now.UnixNano())
			n := c.stallDumps.Add(1)
			c.reportStall(idle, n, maxDumps)
		}
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
