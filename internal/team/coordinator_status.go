package team

// Current-activity snapshot used by the TUI status line.

import (
	"fmt"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/tools"
	"github.com/anomalyco/hufu/internal/utils"
)

func (c *Coordinator) getSnapshotField(getter func(*currentSnapshot) string) string {
	s := c.current.Load()
	if s == nil {
		return ""
	}
	return getter(s)
}

func (c *Coordinator) updateSnapshot(updater func(*currentSnapshot)) {
	old := c.current.Load()
	newS := &currentSnapshot{}
	if old != nil {
		*newS = *old
	}
	updater(newS)
	c.current.Store(newS)
}

// currentSnapshot is an atomic snapshot of the coordinator's active state.
// Replaces 8 separate Sync.RWMutex-guarded fields with a single atomic.Pointer
// for lock-free, contention-free reads by the SIGINT handler and status reporter.
type currentSnapshot struct {
	Agent  string
	Task   string
	TodoID string
	Stage  string // "model", "tool", "wrapping_up", "idle"
	Step   int
	Tool   string
	Model  string
	// stageStart is NOT in the snapshot; it is only written/read within the
	// SetCurrentStage method which still uses its own lightweight mutex.
}

// SetCurrentStage records the high-level stage the coordinator is in:
// "model" while an LLM call is in flight, "tool" while a tool is being
// executed, "wrapping_up" during wrap-up, "idle" otherwise.
func (c *Coordinator) SetCurrentStage(stage string) {
	old := c.current.Load()
	newS := &currentSnapshot{}
	if old != nil {
		*newS = *old
	}
	newS.Stage = stage
	c.current.Store(newS)
	c.currentStageStartMu.Lock()
	defer c.currentStageStartMu.Unlock()
	if stage == "idle" {
		c.currentStageStart = time.Time{}
		return
	}
	c.currentStageStart = time.Now()
}

// SetCurrentStep records the current step number in the fantasy agent's step loop.
func (c *Coordinator) SetCurrentStep(n int) {
	old := c.current.Load()
	newS := &currentSnapshot{}
	if old != nil {
		*newS = *old
	}
	newS.Step = n
	c.current.Store(newS)
}

// SetCurrentTool records the tool name currently being executed.
func (c *Coordinator) SetCurrentTool(name string) {
	old := c.current.Load()
	newS := &currentSnapshot{}
	if old != nil {
		*newS = *old
	}
	newS.Tool = name
	c.current.Store(newS)
}

// GetCurrentTool returns the last recorded tool name, even if the current
// stage is no longer "tool". This is useful for failure summaries.
func (c *Coordinator) GetCurrentTool() string {
	s := c.current.Load()
	if s == nil {
		return ""
	}
	return s.Tool
}

// SetCurrentModel records the model ID currently being used for an LLM call.
func (c *Coordinator) SetCurrentModel(modelID string) {
	old := c.current.Load()
	newS := &currentSnapshot{}
	if old != nil {
		*newS = *old
	}
	newS.Model = modelID
	c.current.Store(newS)
}

// GetCurrentStatus returns a human-readable snapshot of the coordinator's
// current state. Designed for SIGINT/watchdog diagnostics.
func (c *Coordinator) GetCurrentStatus() string {
	s := c.current.Load()
	c.currentStageStartMu.RLock()
	stageStart := c.currentStageStart
	c.currentStageStartMu.RUnlock()

	if s == nil || s.Stage == "" || s.Stage == "idle" {
		return "idle"
	}

	elapsed := ""
	if !stageStart.IsZero() {
		elapsed = fmt.Sprintf(" (%.0fs elapsed)", time.Since(stageStart).Seconds())
	}

	parts := []string{s.Stage}
	if s.Agent != "" {
		parts = append(parts, "agent="+s.Agent)
	}
	if s.Model != "" && (s.Stage == "model" || s.Stage == "wrapping_up") {
		parts = append(parts, "model="+s.Model)
	}
	if s.Tool != "" && s.Stage == "tool" {
		parts = append(parts, "tool="+s.Tool)
	}
	if s.Step > 0 {
		parts = append(parts, fmt.Sprintf("step=%d", s.Step))
	}
	if s.Task != "" {
		parts = append(parts, "task="+utils.TruncateString(s.Task, 60))
	}
	return strings.Join(parts, " ") + elapsed
}

func (c *Coordinator) GetCurrentAgentInfo() tools.AgentInfo {
	s := c.current.Load()
	if s == nil {
		return tools.AgentInfo{}
	}
	return tools.AgentInfo{Name: s.Agent, Task: s.Task}
}
