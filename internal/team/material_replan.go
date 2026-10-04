package team

import (
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"

	"github.com/kjelly/hufu/internal/agent"
)

// EventStrategyChangeEvaluated records how a task dispatched in place of one
// that failed with replan_required compares with the failed strategy: once
// at dispatch (phase planned) and once after its first finished attempt
// (phase executed, which also compares the tool sequences).
const EventStrategyChangeEvaluated EventType = "strategy_change_evaluated"

// EventStrategyChangeRejected records a dispatch refused under
// reliability.material-replan: enforce because it repeated a failed strategy.
const EventStrategyChangeRejected EventType = "strategy_change_rejected"

// StrategyChangeSchemaVersion is the payload schema of both strategy events.
const StrategyChangeSchemaVersion = 1

const (
	strategyPhasePlanned  = "planned"
	strategyPhaseExecuted = "executed"
	// strategyLinkVerification links tasks that prove success with the same
	// verification; strategyLinkCriterion links a task that advances the
	// criterion another failed.
	strategyLinkVerification = "verification"
	strategyLinkCriterion    = "criterion"
	// reasonReplanNotMateriallyDifferent marks a replacement that changes
	// none of the structural dimensions of the failed strategy.
	reasonReplanNotMateriallyDifferent = "replan_not_materially_different"
)

// StrategyChangePayload is content-free: identities, dimension names, and
// digests, never goal text, arguments, or output.
type StrategyChangePayload struct {
	SchemaVersion       int                 `json:"schema_version"`
	Phase               string              `json:"phase"`
	Mode                string              `json:"mode"`
	RunID               string              `json:"run_id,omitempty"`
	TaskID              string              `json:"task_id,omitempty"`
	Attempt             int                 `json:"attempt,omitempty"`
	Agent               string              `json:"agent"`
	PreviousTaskID      string              `json:"previous_task_id"`
	PreviousAttempt     int                 `json:"previous_attempt,omitempty"`
	Link                string              `json:"link"`
	PreviousDigest      string              `json:"previous_digest"`
	CandidateDigest     string              `json:"candidate_digest"`
	ChangedDimensions   []StrategyDimension `json:"changed_dimensions,omitempty"`
	UnknownDimensions   []StrategyDimension `json:"unknown_dimensions,omitempty"`
	MateriallyDifferent bool                `json:"materially_different"`
	ReasonCode          string              `json:"reason_code,omitempty"`
	PreviousStrategy    RecoveryStrategy    `json:"previous_strategy,omitempty"`
	CandidateStrategy   RecoveryStrategy    `json:"candidate_strategy,omitempty"`
}

// strategyChangeLog remembers which failed tasks each dispatched replacement
// was compared with, which executed comparisons were recorded, and the
// per-run counts. It is rebuilt from the event log on first use.
type strategyChangeLog struct {
	mu       sync.Mutex
	loaded   bool
	planned  map[string][]StrategyChangePayload
	executed map[string]bool
	counts   map[string]*replanStrategyCounts
}

type replanStrategyCounts struct {
	changed, unchanged, rejected int
}

func (c *Coordinator) materialReplanMode() string {
	mode, ok := agent.NormalizeMaterialReplanMode(c.reliabilityConfig().MaterialReplan)
	if !ok {
		return agent.MaterialReplanWarn
	}
	return mode
}

type replanLink struct {
	previous *TodoItem
	link     string
}

// replanPredecessors returns the failed tasks candidate would attempt again:
// unresolved tasks that failed with replan_required and either prove success
// with the same verification as candidate or failed a criterion candidate
// advances. A failed task with neither cannot be linked without reading goal
// text, so it is not compared.
func (c *Coordinator) replanPredecessors(candidate *TodoItem, items []*TodoItem) []replanLink {
	operation := stableOperation(candidate)
	verified := !strings.HasPrefix(operation, "task:")
	var links []replanLink
	for _, item := range items {
		if item == nil || item.ID == candidate.ID || (item.Status != TaskError && item.Status != TaskBlocked) {
			continue
		}
		if item.FailureEvent == nil || item.FailureEvent.RetryDisposition != ReplanRequired {
			continue
		}
		if item.Resolution != nil && item.Resolution.Status != "" && item.Resolution.Status != "unresolved" {
			continue
		}
		criterion := c.failedCriterionForTask(item)
		switch {
		case verified && stableOperation(item) == operation:
			links = append(links, replanLink{previous: item, link: strategyLinkVerification})
		case criterion != "" && slices.Contains(candidate.Advances, criterion):
			links = append(links, replanLink{previous: item, link: strategyLinkCriterion})
		}
	}
	return links
}

func lastFinishedReceipt(item *TodoItem) *ExecutionReceipt {
	for i := len(item.ExecutionReceipts) - 1; i >= 0; i-- {
		if !item.ExecutionReceipts[i].FinishedAt.IsZero() {
			return &item.ExecutionReceipts[i]
		}
	}
	return nil
}

func strategyChangePayload(phase, mode string, candidate *TodoItem, link replanLink, previous, current StrategyExecutionFingerprint) StrategyChangePayload {
	changed, unknown := compareStrategyFingerprints(previous, current)
	payload := StrategyChangePayload{
		SchemaVersion: StrategyChangeSchemaVersion, Phase: phase, Mode: mode,
		Agent: strings.ToLower(strings.TrimSpace(candidate.Agent)), PreviousTaskID: link.previous.ID, Link: link.link,
		PreviousDigest: previous.Digest, CandidateDigest: current.Digest,
		ChangedDimensions: changed, UnknownDimensions: unknown, MateriallyDifferent: len(changed) > 0,
		PreviousStrategy: previous.RecoveryStrategy, CandidateStrategy: current.RecoveryStrategy,
	}
	if !payload.MateriallyDifferent {
		payload.ReasonCode = reasonReplanNotMateriallyDifferent
	}
	return payload
}

// evaluateReplanBatch compares every task of a batch about to be created
// with the failed strategies it would attempt again. The tool sequence is
// not known before a task runs, so this planned comparison leaves it out
// on both sides. Under material-replan: enforce a task that changes nothing
// structural refuses the whole batch; otherwise the comparisons are returned
// for recording once the batch exists.
func (c *Coordinator) evaluateReplanBatch(batch []TodoSpec, ids []string) ([]StrategyChangePayload, error) {
	mode := c.materialReplanMode()
	if mode == agent.MaterialReplanOff || c.taskTracker == nil || len(batch) != len(ids) {
		return nil, nil
	}
	items := c.taskTracker.TodoList().Items()
	batchAgents := make(map[string]string, len(ids))
	for i, id := range ids {
		batchAgents[id] = batch[i].Agent
	}
	agentOf := func(id string) string {
		if agent, ok := batchAgents[id]; ok {
			return agent
		}
		if item := c.todoItemByID(id); item != nil {
			return item.Agent
		}
		return ""
	}
	var evaluations, refused []StrategyChangePayload
	for i := range batch {
		candidate := todoItemFromSpec(batch[i], ids[i])
		for _, link := range c.replanPredecessors(candidate, items) {
			payload := strategyChangePayload(strategyPhasePlanned, mode, candidate, link,
				strategyFingerprint(link.previous, nil, agentOf), strategyFingerprint(candidate, nil, agentOf))
			payload.RunID, payload.TaskID = c.executionRunID, ids[i]
			if !payload.MateriallyDifferent && mode == agent.MaterialReplanEnforce {
				refused = append(refused, payload)
				continue
			}
			evaluations = append(evaluations, payload)
		}
	}
	if len(refused) == 0 {
		return evaluations, nil
	}
	var reasons []string
	for _, payload := range refused {
		// The refused task never exists, so the event names the failed task.
		payload.TaskID = ""
		c.recordStrategyChange(EventStrategyChangeRejected, "", "", payload)
		reasons = append(reasons, fmt.Sprintf("a %s task repeats the strategy of failed task %s", payload.Agent, payload.PreviousTaskID))
	}
	return nil, c.rejectDelegationPolicy(reasonReplanNotMateriallyDifferent + ": " + strings.Join(reasons, "; ") +
		". Those tasks failed with replan_required, and the replacement keeps the same agent and task contract, execution target, dependencies, and verification evidence; a new goal text or recovery strategy name is not a different strategy." +
		" Change at least one of these (for example another agent or model, a diagnostic task it depends on, or different files or evidence to work from), or resolve the failed task with reconcile_task if it no longer needs doing.")
}

// recordPlannedStrategyChanges records the comparisons of a batch that was
// created. A comparison that found no material change also warns.
func (c *Coordinator) recordPlannedStrategyChanges(evaluations []StrategyChangePayload) {
	for _, payload := range evaluations {
		key := "strategy:planned:" + payload.TaskID + "/" + payload.PreviousTaskID
		c.recordStrategyChange(EventStrategyChangeEvaluated, key, payload.TaskID, payload)
		if !payload.MateriallyDifferent {
			c.report(c.newEvent("loop_warning").withTodoID(payload.TaskID).withMessage(fmt.Sprintf(
				"task %s repeats the strategy of failed task %s (%s); it changes none of the agent, execution target, dependencies, or evidence", payload.TaskID, payload.PreviousTaskID, ReplanRequired)))
		}
	}
}

// observeExecutedStrategyChange compares a replacement's first finished
// attempt with the last finished attempt of each failed task it was
// compared with at dispatch, now including the tool sequences. It is a
// diagnostic and best effort.
func (c *Coordinator) observeExecutedStrategyChange(todoID string, receipt ExecutionReceipt) {
	if c == nil || !c.hasDurableEventJournal() || receipt.FinishedAt.IsZero() {
		return
	}
	c.strategyChanges.mu.Lock()
	c.loadStrategyChangesLocked()
	planned := slices.Clone(c.strategyChanges.planned[todoID])
	c.strategyChanges.mu.Unlock()
	if len(planned) == 0 {
		return
	}
	candidate := c.todoItemByID(todoID)
	if candidate == nil {
		return
	}
	agentOf := func(id string) string {
		if item := c.todoItemByID(id); item != nil {
			return item.Agent
		}
		return ""
	}
	for _, plan := range planned {
		key := "strategy:executed:" + todoID + "/" + plan.PreviousTaskID
		c.strategyChanges.mu.Lock()
		done := c.strategyChanges.executed[key]
		c.strategyChanges.mu.Unlock()
		previous := c.todoItemByID(plan.PreviousTaskID)
		if done || previous == nil {
			continue
		}
		previousReceipt := lastFinishedReceipt(previous)
		payload := strategyChangePayload(strategyPhaseExecuted, plan.Mode, candidate, replanLink{previous: previous, link: plan.Link},
			strategyFingerprint(previous, previousReceipt, agentOf), strategyFingerprint(candidate, &receipt, agentOf))
		payload.RunID, payload.TaskID, payload.Attempt = receipt.RunID, todoID, receipt.Attempt
		if previousReceipt != nil {
			payload.PreviousAttempt = previousReceipt.Attempt
		}
		c.recordStrategyChange(EventStrategyChangeEvaluated, key, todoID, payload)
	}
}

// recordStrategyChange appends one strategy event and updates the log. An
// empty key appends unconditionally.
func (c *Coordinator) recordStrategyChange(eventType EventType, key, taskID string, payload StrategyChangePayload) {
	// Load the log before appending, or the first load would read this event
	// back and count it twice.
	c.strategyChanges.mu.Lock()
	c.loadStrategyChangesLocked()
	c.strategyChanges.mu.Unlock()
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("warning: encode %s: %v", eventType, err)
		return
	}
	if key == "" {
		err = c.emitEvent(string(eventType), "hufu", taskID, payload)
	} else {
		_, err = c.emitEventOnce(key, RunEvent{Type: string(eventType), Actor: "hufu", TaskID: taskID, Attempt: payload.Attempt, Payload: raw})
	}
	if err != nil {
		log.Printf("warning: persist %s: %v", eventType, err)
		return
	}
	c.strategyChanges.mu.Lock()
	defer c.strategyChanges.mu.Unlock()
	c.strategyChanges.record(eventType, payload)
}

func (l *strategyChangeLog) record(eventType EventType, payload StrategyChangePayload) {
	counts := l.counts[payload.RunID]
	if counts == nil {
		counts = &replanStrategyCounts{}
		l.counts[payload.RunID] = counts
	}
	switch {
	case eventType == EventStrategyChangeRejected:
		counts.rejected++
	case payload.Phase == strategyPhaseExecuted:
		l.executed["strategy:executed:"+payload.TaskID+"/"+payload.PreviousTaskID] = true
	default:
		for _, existing := range l.planned[payload.TaskID] {
			if existing.PreviousTaskID == payload.PreviousTaskID {
				return
			}
		}
		l.planned[payload.TaskID] = append(l.planned[payload.TaskID], payload)
		if payload.MateriallyDifferent {
			counts.changed++
		} else {
			counts.unchanged++
		}
	}
}

func (c *Coordinator) loadStrategyChangesLocked() {
	if c.strategyChanges.loaded {
		return
	}
	c.strategyChanges.loaded = true
	c.strategyChanges.planned = map[string][]StrategyChangePayload{}
	c.strategyChanges.executed = map[string]bool{}
	c.strategyChanges.counts = map[string]*replanStrategyCounts{}
	if c.eventStore == nil {
		return
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		log.Printf("warning: read strategy change events: %v", err)
		return
	}
	for _, event := range events {
		if payload, ok := decodeStrategyChange(event); ok {
			c.strategyChanges.record(EventType(event.Type), payload)
		}
	}
}

// replanStrategyCounts returns this run's dispatched replacements that
// changed the failed strategy, those that did not, and refused dispatches.
func (c *Coordinator) replanStrategyCounts() replanStrategyCounts {
	if c == nil {
		return replanStrategyCounts{}
	}
	c.strategyChanges.mu.Lock()
	defer c.strategyChanges.mu.Unlock()
	c.loadStrategyChangesLocked()
	if counts := c.strategyChanges.counts[c.executionRunID]; counts != nil {
		return *counts
	}
	return replanStrategyCounts{}
}

func decodeStrategyChange(event RunEvent) (StrategyChangePayload, bool) {
	if event.Type != string(EventStrategyChangeEvaluated) && event.Type != string(EventStrategyChangeRejected) {
		return StrategyChangePayload{}, false
	}
	var payload StrategyChangePayload
	if json.Unmarshal(event.Payload, &payload) != nil || payload.SchemaVersion != StrategyChangeSchemaVersion {
		return StrategyChangePayload{}, false
	}
	return payload, true
}

// StrategyChangeObservations decodes every strategy event in log order,
// skipping payloads it cannot read.
func StrategyChangeObservations(events []RunEvent) []StrategyChangePayload {
	var out []StrategyChangePayload
	for _, event := range events {
		if payload, ok := decodeStrategyChange(event); ok {
			out = append(out, payload)
		}
	}
	return out
}

func validateStrategyChangeEvent(event RunEvent) error {
	var payload StrategyChangePayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode %s payload: %w", event.Type, err)
	}
	if payload.SchemaVersion != StrategyChangeSchemaVersion {
		return fmt.Errorf("%s has unsupported schema_version %d", event.Type, payload.SchemaVersion)
	}
	if payload.Phase != strategyPhasePlanned && payload.Phase != strategyPhaseExecuted {
		return fmt.Errorf("%s has invalid phase %q", event.Type, payload.Phase)
	}
	if strings.TrimSpace(payload.PreviousTaskID) == "" || payload.PreviousDigest == "" || payload.CandidateDigest == "" {
		return fmt.Errorf("%s requires the failed task and both digests", event.Type)
	}
	if event.Type == string(EventStrategyChangeEvaluated) && strings.TrimSpace(payload.TaskID) == "" {
		return fmt.Errorf("%s requires the dispatched task", event.Type)
	}
	return nil
}
