package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ergoreadline "github.com/ergochat/readline"

	"github.com/anomalyco/hufu/internal/readline"
	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
)

type idleWarningTimer struct {
	w        *lineWriter
	interval time.Duration
	timer    *time.Timer
	mu       sync.Mutex
	warned   bool
	deadline time.Time
	model    string
}

func newIdleWarningTimer(w *lineWriter, interval time.Duration) *idleWarningTimer {
	t := &idleWarningTimer{
		w:        w,
		interval: interval,
	}
	t.timer = time.AfterFunc(interval, t.warn)
	t.deadline = time.Now().Add(t.interval)
	return t
}

func (t *idleWarningTimer) SetModel(model string) {
	t.mu.Lock()
	t.model = model
	t.mu.Unlock()
}

func (t *idleWarningTimer) warn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer == nil {
		return
	}
	elapsed := time.Since(t.deadline.Add(-t.interval))
	modelStr := ""
	if t.model != "" {
		modelStr = " [" + t.model + "]"
	}
	t.w.write(fmt.Sprintf("\n%s Waiting for LLM response%s... (%v elapsed)\n",
		stepStyle.Render("⏳"),
		modelStr,
		elapsed.Truncate(time.Second),
	))
	t.warned = true
}

func (t *idleWarningTimer) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer == nil {
		return
	}
	t.timer.Stop()
	if t.warned {
		idleDuration := time.Since(t.deadline.Add(-t.interval))
		t.w.write(fmt.Sprintf("\n%s Activity resumed (idle %v)\n", doneStyle.Render("↻"), idleDuration.Truncate(time.Second)))
		t.warned = false
	}
	t.timer = time.AfterFunc(t.interval, t.warn)
	t.deadline = time.Now().Add(t.interval)
}

func (t *idleWarningTimer) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
}

type activeCoordinator struct {
	mu          sync.Mutex
	coordinator *team.Coordinator
}

func (a *activeCoordinator) Set(c *team.Coordinator) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.coordinator = c
}

func (a *activeCoordinator) Get() *team.Coordinator {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.coordinator
}

func (a *activeCoordinator) Clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.coordinator = nil
}

type promptInjector struct {
	ch              chan string
	wrapUpCh        chan struct{}
	wrapUpRequested atomic.Bool
	promptReader    *readline.PromptReader
}

func newPromptInjector(pr *readline.PromptReader) *promptInjector {
	return &promptInjector{
		ch:           make(chan string, 16),
		wrapUpCh:     make(chan struct{}, 1),
		promptReader: pr,
	}
}

func (p *promptInjector) enqueue(prompt string) {
	select {
	case p.ch <- prompt:
	default:
	}
}

func (p *promptInjector) injectWrapUp() {
	p.wrapUpRequested.Store(true)
	select {
	case p.wrapUpCh <- struct{}{}:
	default:
	}
}

func (p *promptInjector) IsWrapUpRequested() bool {
	return p.wrapUpRequested.Load()
}

func (p *promptInjector) promptAndEnqueue() {
	tools.StdinMu.Lock()
	defer tools.StdinMu.Unlock()

	if p.promptReader == nil {
		return
	}

	stderrLog("\n%s\n", boldStyle.Render("─── Additional Prompt ───"))
	line, err := p.promptReader.ReadLine(boldStyle.Render("> "))
	if err != nil {
		if err == ergoreadline.ErrInterrupt || err == io.EOF {
			return
		}
		return
	}
	prompt := strings.TrimSpace(line)
	if prompt == "" {
		return
	}
	p.enqueue(prompt)
	stderrLog("%s Prompt enqueued, will be processed after current task completes.\n", doneStyle.Render("✓"))
}
