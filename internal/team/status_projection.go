package team

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/anomalyco/hufu/internal/utils"
)

// AgentStatus is the small, user-facing status vocabulary projected from the
// canonical task and terminal-session state.
type AgentStatus string

// statusProjectionMu serializes complete projection snapshots. Atomic rename
// protects an individual file, but without this lock two snapshots could
// interleave their writes and stale-file cleanup.
var statusProjectionMu sync.Mutex

const (
	AgentStatusIdle    AgentStatus = "idle"
	AgentStatusWorking AgentStatus = "working"
	AgentStatusPaused  AgentStatus = "paused"
	AgentStatusError   AgentStatus = "error"
)

// ProjectAgentStatuses derives status exclusively from canonical task and
// terminal state. It does not read status files and therefore cannot preserve
// a stale status across a restart or a partial run.
func ProjectAgentStatuses(items []*TodoItem, sessions []TerminalSession) map[string]AgentStatus {
	byTask := make(map[string]*TodoItem, len(items))
	result := make(map[string]AgentStatus)
	for _, item := range items {
		if item == nil || strings.TrimSpace(item.Agent) == "" {
			continue
		}
		byTask[item.ID] = item
		name := strings.ToLower(strings.TrimSpace(item.Agent))
		status := taskAgentStatus(item.Status)
		result[name] = mergeAgentStatus(result[name], status)
	}

	for _, session := range sessions {
		name := strings.ToLower(strings.TrimSpace(session.Agent))
		if name == "" {
			if item := byTask[session.OwnerTaskID]; item != nil {
				name = strings.ToLower(strings.TrimSpace(item.Agent))
			}
		}
		if name == "" {
			continue
		}
		result[name] = mergeAgentStatus(result[name], terminalAgentStatus(session))
	}
	return result
}

func taskAgentStatus(status TaskStatus) AgentStatus {
	switch status {
	case TaskInProgress, TaskPlanned, TaskVerifying:
		return AgentStatusWorking
	case TaskPaused:
		return AgentStatusPaused
	case TaskError, TaskBlocked, TaskProtocolIncomplete:
		return AgentStatusError
	default:
		return AgentStatusIdle
	}
}

func terminalAgentStatus(session TerminalSession) AgentStatus {
	if session.State == TerminalSessionUnknown {
		// Unknown is fail-closed after a restart: the process cannot be called
		// idle or successful until reconciliation establishes its fate.
		return AgentStatusError
	}
	if session.Controller == TerminalControllerUser && (session.Running || session.State == TerminalSessionRunning) {
		return AgentStatusPaused
	}
	if session.Running || session.State == TerminalSessionRunning {
		return AgentStatusWorking
	}
	return AgentStatusIdle
}

func mergeAgentStatus(current, next AgentStatus) AgentStatus {
	priority := func(status AgentStatus) int {
		switch status {
		case AgentStatusError:
			return 4
		case AgentStatusPaused:
			return 3
		case AgentStatusWorking:
			return 2
		default:
			return 1
		}
	}
	if priority(next) > priority(current) {
		return next
	}
	if current == "" {
		return next
	}
	return current
}

// ReconcileAgentStatuses atomically projects canonical state into status
// files. Existing files for agents no longer present in canonical state are
// removed, so cancellation and crash-resume cannot leave stale workers.
func ReconcileAgentStatuses(workspace string, items []*TodoItem, sessions []TerminalSession) error {
	statusProjectionMu.Lock()
	defer statusProjectionMu.Unlock()

	statuses := ProjectAgentStatuses(items, sessions)
	for name := range statuses {
		if err := validateProjectedStatusName(name); err != nil {
			return err
		}
	}
	dir := filepath.Join(workspace, statusDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	for name, status := range statuses {
		record := projectedStatusRecord{Status: status}
		for _, item := range items {
			if item == nil || !strings.EqualFold(strings.TrimSpace(item.Agent), name) {
				continue
			}
			if record.Detail == "" && item.Detail != "" {
				record.Detail = utils.RedactSecrets(item.Detail)
			}
			if record.FailureEvent == nil && item.FailureEvent != nil {
				record.FailureEvent = RedactedFailureEvent(item.FailureEvent)
			}
			if record.Detail != "" && record.FailureEvent != nil {
				break
			}
		}
		contentBytes, err := yaml.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal projected status for %s: %w", name, err)
		}
		if err := AtomicWriteFile(filepath.Join(dir, name+".yml"), contentBytes, 0o644); err != nil {
			return err
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	keep := make(map[string]struct{}, len(statuses))
	for name := range statuses {
		keep[name+".yml"] = struct{}{}
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yml" {
			continue
		}
		if _, ok := keep[entry.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func validateProjectedStatusName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) || filepath.Base(name) != name || filepath.Clean(name) != name || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, '\x00') {
		return fmt.Errorf("unsafe projected status filename %q", name)
	}
	return nil
}

type projectedStatusRecord struct {
	Status       AgentStatus          `yaml:"status"`
	Detail       string               `yaml:"detail,omitempty"`
	FailureEvent *FailureEventPayload `yaml:"failure_event,omitempty"`
}

// ReconcileAgentStatusesFromSource keeps terminal-session acquisition outside
// the file projection. If canonical session state cannot be read, it returns
// the error without replacing existing status files (fail closed).
func ReconcileAgentStatusesFromSource(ctx context.Context, workspace string, items []*TodoItem, source TerminalSessionSource) error {
	if source == nil {
		return ReconcileAgentStatuses(workspace, items, nil)
	}
	sessions, err := source.List(ctx, "")
	if err != nil {
		return fmt.Errorf("list terminal sessions for status projection: %w", err)
	}
	return ReconcileAgentStatuses(workspace, items, sessions)
}

func (c *Coordinator) reconcileProjectedStatuses(coordinatorStatus AgentStatus) {
	if err := c.reconcileProjectedStatusesWithDetail(coordinatorStatus, ""); err != nil {
		log.Printf("warning: status projection failed: %v", err)
	}
}

func (c *Coordinator) reconcileProjectedStatusesWithDetail(coordinatorStatus AgentStatus, detail string) error {
	if c == nil || c.session == nil || c.session.Workspace == "" || c.taskTracker == nil {
		return nil
	}
	items := c.taskTracker.TodoList().Items()
	items = append(items, &TodoItem{ID: CoordTodoID, Agent: "coordinator", Status: mapAgentStatusToTaskStatus(coordinatorStatus), Detail: detail})
	return c.reconcileProjectedItems(items)
}

func (c *Coordinator) reconcileProjectedItems(items []*TodoItem) error {
	if c == nil || c.session == nil || c.session.Workspace == "" {
		return nil
	}
	items = append([]*TodoItem(nil), items...)
	// A configured worker with no current todo still has a canonical idle
	// state. Include those workers as synthetic, read-only projection inputs;
	// this does not mutate the TodoList.
	for _, def := range c.uniqueWorkerDefs() {
		if def == nil || strings.TrimSpace(def.Name) == "" {
			continue
		}
		found := false
		for _, item := range items {
			if item != nil && strings.EqualFold(strings.TrimSpace(item.Agent), strings.TrimSpace(def.Name)) {
				found = true
				break
			}
		}
		if !found {
			items = append(items, &TodoItem{ID: "projection:" + strings.ToLower(strings.TrimSpace(def.Name)), Agent: def.Name, Status: TaskDone})
		}
	}
	var sessions []TerminalSession
	if c.terminalSessionMgr != nil {
		var err error
		sessions, err = c.terminalSessionMgr.List(context.Background(), "")
		if err != nil {
			return fmt.Errorf("list terminal sessions for status projection: %w", err)
		}
	}
	return ReconcileAgentStatuses(c.session.Workspace, items, sessions)
}

// reconcileTaskStatusProjection is the best-effort lifecycle hook used after
// canonical TodoList transitions. Status files are projections only, so a
// projection failure must not change task execution semantics.
func (c *Coordinator) reconcileTaskStatusProjection() {
	if c == nil || c.taskTracker == nil {
		return
	}
	if err := c.reconcileProjectedItems(c.taskTracker.TodoList().Items()); err != nil {
		log.Printf("warning: task status projection failed: %v", err)
	}
}

func mapAgentStatusToTaskStatus(status AgentStatus) TaskStatus {
	switch status {
	case AgentStatusWorking:
		return TaskInProgress
	case AgentStatusPaused:
		return TaskPaused
	case AgentStatusError:
		return TaskError
	default:
		return TaskDone
	}
}
