package conversation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Controller serializes one chat session: at most one turn or control
// command runs at a time, later prompts wait in a bounded queue, and every
// Submit is answered with an explicit Ack. Submit never blocks on the runtime.
type Controller struct {
	port RuntimePort
	cfg  Config

	mu          sync.Mutex
	sessionCtx  context.Context
	started     bool
	closing     bool
	active      *work // admitted command currently owning the actor
	activeTaken bool  // the actor has picked up active
	queue       []*work
	dropped     []string
	acks        *ackWindow

	wake      chan struct{}
	events    chan Event
	done      chan struct{}
	stopEmit  chan struct{}
	closeOnce sync.Once
	report    CloseReport
}

// New creates a controller bound to port. Call Start before Submit.
func New(port RuntimePort, cfg Config) *Controller {
	cfg = cfg.normalized()
	return &Controller{
		port:     port,
		cfg:      cfg,
		acks:     newAckWindow(cfg.DedupeWindow),
		wake:     make(chan struct{}, 1),
		events:   make(chan Event, eventBuffer),
		done:     make(chan struct{}),
		stopEmit: make(chan struct{}),
	}
}

// Events returns the ordered event stream. It is closed after Closed.
func (c *Controller) Events() <-chan Event { return c.events }

// Start launches the actor. Cancelling sessionCtx closes the session.
func (c *Controller) Start(sessionCtx context.Context) {
	c.mu.Lock()
	if c.started || c.closing {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.sessionCtx = sessionCtx
	c.mu.Unlock()
	go c.loop()
}

// Submit admits one command and reports the decision without blocking.
func (c *Controller) Submit(cmd Command) Ack {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(cmd.ID) == "" {
		return reject(cmd, "command id is required")
	}
	if prior, ok := c.acks.get(cmd.ID); ok {
		return prior
	}
	ack := c.admitLocked(cmd)
	c.acks.put(cmd.ID, ack)
	return ack
}

func reject(cmd Command, reason string) Ack {
	return Ack{CommandID: cmd.ID, Status: AckRejected, Reason: reason}
}

func (c *Controller) admitLocked(cmd Command) Ack {
	if c.closing {
		return reject(cmd, "session closing")
	}
	if !c.started {
		return reject(cmd, "session not started")
	}
	switch cmd.Kind {
	case CommandSubmitPrompt:
		return c.admitPromptLocked(cmd)
	case CommandCancelTurn:
		if !c.turnActiveLocked() {
			return reject(cmd, "no active turn")
		}
		c.active.cancel()
		return Ack{CommandID: cmd.ID, TurnID: c.active.turnID, Status: AckAccepted}
	case CommandWrapUp:
		if !c.turnActiveLocked() {
			return reject(cmd, "no active turn")
		}
		c.port.RequestWrapUp()
		return Ack{CommandID: cmd.ID, TurnID: c.active.turnID, Status: AckAccepted}
	case CommandResetContext, CommandSwitchTeam:
		if c.active != nil || len(c.queue) > 0 {
			return reject(cmd, "busy: finish or cancel the active turn first")
		}
		if cmd.Kind == CommandSwitchTeam && strings.TrimSpace(cmd.Target) == "" {
			return reject(cmd, "team name is required")
		}
		c.activateLocked(&work{cmd: cmd})
		return Ack{CommandID: cmd.ID, Status: AckAccepted}
	default:
		return reject(cmd, fmt.Sprintf("unknown command kind %q", cmd.Kind))
	}
}

func (c *Controller) admitPromptLocked(cmd Command) Ack {
	switch {
	case !utf8.ValidString(cmd.Text):
		return reject(cmd, "prompt is not valid UTF-8")
	case strings.TrimSpace(cmd.Text) == "":
		return reject(cmd, "prompt is empty")
	case len(cmd.Text) > c.cfg.MaxPromptBytes:
		return reject(cmd, fmt.Sprintf("prompt is %d bytes; the limit is %d bytes", len(cmd.Text), c.cfg.MaxPromptBytes))
	}
	if c.active == nil && len(c.queue) == 0 {
		w := &work{cmd: cmd, turnID: c.cfg.NewTurnID()}
		c.activateLocked(w)
		return Ack{CommandID: cmd.ID, TurnID: w.turnID, Status: AckAccepted}
	}
	if len(c.queue) >= c.cfg.QueueCapacity {
		return Ack{CommandID: cmd.ID, Status: AckBusy, Reason: fmt.Sprintf("prompt queue is full (%d waiting)", len(c.queue))}
	}
	w := &work{cmd: cmd, turnID: c.cfg.NewTurnID()}
	c.queue = append(c.queue, w)
	return Ack{CommandID: cmd.ID, TurnID: w.turnID, Status: AckQueued, Position: len(c.queue)}
}

func (c *Controller) turnActiveLocked() bool {
	return c.active != nil && c.active.isPrompt()
}

// activateLocked makes w the active command and wakes the actor. A prompt
// gets its own context so cancelling it never touches the session.
func (c *Controller) activateLocked(w *work) {
	if w.isPrompt() {
		w.ctx, w.cancel = context.WithCancel(c.sessionCtx)
	}
	c.active = w
	c.activeTaken = false
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Controller) loop() {
	defer close(c.done)
	for {
		w, ok := c.next()
		if !ok {
			break
		}
		result := c.execute(w)
		// Release the actor before announcing the result, so a command the
		// front end sends in reaction to the event is admitted as idle.
		c.mu.Lock()
		c.active = nil
		if !c.closing && len(c.queue) > 0 {
			next := c.queue[0]
			c.queue = c.queue[1:]
			c.activateLocked(next)
		}
		c.mu.Unlock()
		c.emit(result)
	}
	c.mu.Lock()
	dropped := c.dropped
	c.mu.Unlock()
	if len(dropped) > 0 {
		c.emit(QueueDropped{TurnIDs: dropped})
	}
	c.emit(Closed{})
	close(c.events)
}

// next blocks until the actor has work or the session is closing.
func (c *Controller) next() (*work, bool) {
	for {
		c.mu.Lock()
		if c.closing && (c.active == nil || c.activeTaken) {
			c.mu.Unlock()
			return nil, false
		}
		if c.active != nil && !c.activeTaken {
			c.activeTaken = true
			w := c.active
			c.mu.Unlock()
			return w, true
		}
		sessionDone := c.sessionCtx.Done()
		c.mu.Unlock()
		select {
		case <-c.wake:
		case <-sessionDone:
			c.beginClose()
		}
	}
}

// execute runs w on the actor and returns the event that reports it.
func (c *Controller) execute(w *work) Event {
	switch w.cmd.Kind {
	case CommandSubmitPrompt:
		defer w.cancel()
		c.emit(TurnStarted{TurnID: w.turnID, CommandID: w.cmd.ID, Prompt: w.cmd.Text})
		result := c.port.RunTurn(w.ctx, w.turnID, w.cmd.Text)
		result.TurnID = w.turnID
		return TurnSettled{Result: result}
	case CommandResetContext:
		err := c.port.ResetContext(c.sessionCtx)
		return ControlDone{CommandID: w.cmd.ID, Kind: w.cmd.Kind, Err: err}
	default: // CommandSwitchTeam; admission rejects every other kind
		err := c.port.SwitchTeam(c.sessionCtx, w.cmd.Target)
		return ControlDone{CommandID: w.cmd.ID, Kind: w.cmd.Kind, Target: w.cmd.Target, Err: err}
	}
}

func (c *Controller) emit(ev Event) {
	select {
	case c.events <- ev:
	case <-c.stopEmit:
	}
}

// beginClose stops admission, drops queued prompts, and cancels the active
// turn. It is idempotent.
func (c *Controller) beginClose() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return append([]string(nil), c.dropped...)
	}
	c.closing = true
	for _, w := range c.queue {
		c.dropped = append(c.dropped, w.turnID)
	}
	c.queue = nil
	if c.active != nil && c.active.cancel != nil {
		c.active.cancel()
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return append([]string(nil), c.dropped...)
}

// Close stops the session and waits up to timeout for the active turn to
// settle. Queued prompts are discarded and reported, never executed.
func (c *Controller) Close(timeout time.Duration) CloseReport {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		started := c.started
		c.mu.Unlock()
		c.report.DroppedTurnIDs = c.beginClose()
		if !started {
			close(c.done)
			close(c.events)
			return
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-c.done:
		case <-timer.C:
			c.report.TimedOut = true
			close(c.stopEmit)
		}
	})
	return c.report
}
