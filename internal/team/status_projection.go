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

	"github.com/kjelly/hufu/internal/utils"
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
//
// An agent's todo items accumulate for the life of a run, so an agent that is
// actively working its second task still has its first task's TodoItem
// sitting in canonical state too. Only the single item chosen by
// governingTodoItemForAgent contributes a task-derived status — an old,
// already-superseded error must not keep outranking current work just
// because "error" sorts above "working" (see mergeAgentStatus).
// Terminal-session state is different: an unresolved or unknown-state
// session is a live process risk right now, not a historical outcome, so
// every session for the agent still contributes via the existing
// worst-wins merge.
func ProjectAgentStatuses(items []*TodoItem, sessions []TerminalSession) map[string]AgentStatus {
	byTask := make(map[string]*TodoItem, len(items))
	agentNames := make(map[string]struct{})
	for _, item := range items {
		if item == nil || strings.TrimSpace(item.Agent) == "" {
			continue
		}
		byTask[item.ID] = item
		agentNames[strings.ToLower(strings.TrimSpace(item.Agent))] = struct{}{}
	}
	result := make(map[string]AgentStatus, len(agentNames))
	for name := range agentNames {
		if governing := governingTodoItemForAgent(items, name); governing != nil {
			result[name] = taskAgentStatus(governing.Status)
		}
	}

	for _, session := range sessions {
		name := strings.ToLower(strings.TrimSpace(session.Agent))
		if name == "" {
			if item := byTask[terminalControllerTaskID(session)]; item != nil {
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
	// Completed cleanup is containment evidence, not a live-session leak. It
	// must not keep an otherwise finished task in working/error indefinitely.
	if session.CleanupState == TerminalCleanupCompleted && !session.Running {
		return AgentStatusIdle
	}
	if session.CleanupState == TerminalCleanupManual {
		return AgentStatusError
	}
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
	return ReconcileAgentStatusesForRun(workspace, items, sessions, "", "")
}

// ReconcileAgentStatusesForRun writes the ordinary per-agent projection plus
// the run identity and, once known, one canonical terminal aggregate status.
// The latter prevents a terminal session from presenting coordinator and
// workers as unrelated outcomes after a restart.
func ReconcileAgentStatusesForRun(workspace string, items []*TodoItem, sessions []TerminalSession, runID, terminalStatus string) error {
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
		record := projectedStatusRecord{Status: status, RunID: runID, TerminalStatus: terminalStatus}
		if governing := governingTodoItemForAgent(items, name); governing != nil {
			record.Detail = utils.RedactSecrets(governing.Detail)
			// An idle projection is a current success/absence state. Do not
			// carry an old failure event from a prior attempt into it.
			if status != AgentStatusIdle {
				record.FailureEvent = RedactedFailureEvent(governing.FailureEvent)
			}
			// The governing item is the most recent one, but an older,
			// still-unresolved failure elsewhere for this agent (e.g. a task
			// that errored and was replanned to a new task, rather than
			// retried) would otherwise vanish the moment work moves on. Carry
			// it as separate evidence instead of losing it or letting it
			// masquerade as the agent's current status.
			if unresolved := mostRecentUnresolvedFailureItem(items, name, governing.ID); unresolved != nil {
				record.UnresolvedFailure = RedactedFailureEvent(unresolved.FailureEvent)
			}
		}
		if terminalDetail := terminalStatusDetail(name, sessions); terminalDetail != "" {
			if record.Detail == "" {
				record.Detail = terminalDetail
			} else {
				record.Detail += "; " + terminalDetail
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

// governingTodoItemForAgent picks the single TodoItem that determines an
// agent's projected status, so status and its detail/failure_event always
// come from the same item — selecting them independently (e.g. by
// re-matching on an already-decided status) can combine an old success
// message with a newer failure, or an old failure with newer work, yielding
// a self-contradictory status file.
//
// An item still being worked always governs over any settled outcome,
// however severe: active work is more current than history, and an agent's
// todo items accumulate for the life of a run, so an old, already-replanned
// error must not keep outranking the task the agent is on now. With no
// active item, the *worst* settled outcome governs — mirroring
// taskAgentStatus's severity order — so a real error is not lost among other
// tasks the agent has since finished or skipped in the same batch (e.g. a
// run-teardown that finalizes several tasks at once, where wall-clock order
// among them carries no meaning). Recency (EndedAt, then StartedAt, then ID)
// only breaks ties within whichever group (active, or worst-settled) governs.
func governingTodoItemForAgent(items []*TodoItem, agent string) *TodoItem {
	var mine []*TodoItem
	for _, item := range items {
		if item != nil && strings.EqualFold(strings.TrimSpace(item.Agent), agent) {
			mine = append(mine, item)
		}
	}
	var active []*TodoItem
	for _, item := range mine {
		if isActiveTodoStatus(item.Status) {
			active = append(active, item)
		}
	}
	if len(active) > 0 {
		return mostRecentItem(active)
	}
	worstRank := -1
	for _, item := range mine {
		if r := settledTodoStatusRank(item.Status); r > worstRank {
			worstRank = r
		}
	}
	var worst []*TodoItem
	for _, item := range mine {
		if settledTodoStatusRank(item.Status) == worstRank {
			worst = append(worst, item)
		}
	}
	return mostRecentItem(worst)
}

// mostRecentUnresolvedFailureItem finds the newest still-unresolved failure
// for the agent other than excludeID (the item already governing the
// projected status). It lets an old failure that a newer task has moved past
// stay visible as durable evidence instead of disappearing the moment it
// stops dictating the agent's headline status.
func mostRecentUnresolvedFailureItem(items []*TodoItem, agent, excludeID string) *TodoItem {
	var filtered []*TodoItem
	for _, item := range items {
		if item == nil || item.ID == excludeID || !strings.EqualFold(strings.TrimSpace(item.Agent), agent) {
			continue
		}
		switch item.Status {
		case TaskError, TaskBlocked, TaskProtocolIncomplete:
			filtered = append(filtered, item)
		}
	}
	return mostRecentItem(filtered)
}

// isActiveTodoStatus reports whether status represents ongoing work rather
// than a settled outcome.
func isActiveTodoStatus(status TaskStatus) bool {
	switch status {
	case TaskInProgress, TaskPlanned, TaskVerifying, TaskPaused:
		return true
	default:
		return false
	}
}

// settledTodoStatusRank orders non-active outcomes by severity, matching
// taskAgentStatus: an error-like status must outrank done/skipped/pending
// regardless of which happened later, since finalizing several unrelated
// tasks in the same instant carries no meaningful time ordering between them.
func settledTodoStatusRank(status TaskStatus) int {
	switch status {
	case TaskError, TaskBlocked, TaskProtocolIncomplete:
		return 1
	default:
		return 0
	}
}

func mostRecentItem(items []*TodoItem) *TodoItem {
	var latest *TodoItem
	for _, item := range items {
		if item == nil {
			continue
		}
		if latest == nil || itemNewer(item, latest) {
			latest = item
		}
	}
	return latest
}

func itemNewer(candidate, current *TodoItem) bool {
	if candidate.EndedAt != current.EndedAt {
		return candidate.EndedAt.After(current.EndedAt)
	}
	if candidate.StartedAt != current.StartedAt {
		return candidate.StartedAt.After(current.StartedAt)
	}
	return candidate.ID > current.ID
}

// terminalStatusDetail exposes containment state without terminal output.
func terminalStatusDetail(agent string, sessions []TerminalSession) string {
	completedID := ""
	for _, session := range sessions {
		if !strings.EqualFold(strings.TrimSpace(session.Agent), agent) {
			continue
		}
		switch session.CleanupState {
		case TerminalCleanupManual:
			return "terminal " + session.ID + " requires manual intervention; do not retry until reconciled"
		case TerminalCleanupCompleted:
			// Keep looking: a manual-intervention session for this same agent
			// takes precedence over an earlier contained one. Returning safe
			// retry guidance in that case would contradict the error status.
			if completedID == "" {
				completedID = session.ID
			}
		}
	}
	if completedID != "" {
		return "terminal " + completedID + " was automatically contained; safe to retry"
	}
	return ""
}

func validateProjectedStatusName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) || filepath.Base(name) != name || filepath.Clean(name) != name || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, '\x00') {
		return fmt.Errorf("unsafe projected status filename %q", name)
	}
	return nil
}

type projectedStatusRecord struct {
	Status         AgentStatus          `yaml:"status"`
	RunID          string               `yaml:"run_id,omitempty"`
	TerminalStatus string               `yaml:"terminal_status,omitempty"`
	Detail         string               `yaml:"detail,omitempty"`
	FailureEvent   *FailureEventPayload `yaml:"failure_event,omitempty"`
	// UnresolvedFailure is an older failure for this agent that a newer task
	// has moved past (e.g. replanned rather than retried), kept visible as
	// durable evidence without letting it override the agent's current
	// status the way FailureEvent — which always matches Status — does.
	UnresolvedFailure *FailureEventPayload `yaml:"unresolved_failure,omitempty"`
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
	return c.reconcileProjectedItemsForRun(items, "")
}

func (c *Coordinator) reconcileProjectedItemsForRun(items []*TodoItem, terminalStatus string) error {
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
	runID := c.executionRunID
	if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		runID = c.taskTracker.TodoList().RunID()
	}
	return ReconcileAgentStatusesForRun(c.session.Workspace, items, sessions, runID, terminalStatus)
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

// reconcileTerminalStatusProjection is called only after FinalizeRun has
// established the canonical terminal outcome. It atomically makes every
// coordinator/worker status file identify that same run and aggregate state.
func (c *Coordinator) reconcileTerminalStatusProjection(result *RunResult) {
	if c == nil || result == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return
	}
	// Terminal status and completed workset files are projections of the
	// confirmed immutable run_finished snapshot. Never advance either one for
	// an append failure or a recovery-required lifecycle.
	if !c.TerminalLifecycleConfirmed() {
		return
	}
	items := append([]*TodoItem(nil), c.taskTracker.TodoList().Items()...)
	coordinatorStatus := TaskDone
	if result.Outcome == RunOutcomeBlocked || result.Outcome == RunOutcomeFailed || result.Outcome == RunOutcomePartial || result.Outcome == RunOutcomeCancelled || result.Outcome == RunOutcomeStalled {
		coordinatorStatus = TaskError
	}
	items = append(items, &TodoItem{ID: CoordTodoID, Agent: "coordinator", Status: coordinatorStatus, Detail: FormatCanonicalStatus(result)})
	if err := c.reconcileProjectedItemsForRun(items, string(result.Outcome)); err != nil {
		log.Printf("warning: terminal status projection failed: %v", err)
	}
	if err := c.publishCompletedRuntimeWorksetProjection(result); err != nil {
		log.Printf("warning: completed workset projection failed: %v", err)
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
