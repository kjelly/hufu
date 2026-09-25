package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/fantasy"
)

const (
	teamActionDispatchRejectedPrefix = "TEAM ACTION DISPATCH REJECTED:"
	maxTeamActionRejections          = 3
	teamActionDispatchGuidance       = "No task was created. Do not retry the rejected call unchanged. Use team_action_get to check the input schema and proposals, dispatch workers to gather evidence or record a proposal, or continue without this action."

	teamActionCatalogAbsent            = "team_action_catalog_absent"
	teamActionInitialBatchPending      = "team_action_initial_batch_pending"
	teamActionUnknown                  = "team_action_unknown"
	teamActionAgentMismatch            = "team_action_agent_mismatch"
	teamActionPhaseForbidden           = "team_action_phase_forbidden"
	teamActionUnattendedDenied         = "team_action_unattended_denied"
	teamActionDuplicateInBatch         = "team_action_duplicate_in_batch"
	teamActionProposalRequired         = "team_action_proposal_required"
	teamActionBudgetExceeded           = "team_action_invocation_budget_exceeded"
	teamActionDecisionProfileForbidden = "team_action_decision_profile_unsupported"
	teamActionTaskFieldForbidden       = "team_action_task_field_forbidden"
	teamActionTaskInvalid              = "team_action_task_invalid"

	maxLinkedProposals = 32
	// maxCatalogSchemaEnum bounds the catalog_action id enum; a larger
	// catalog is validated at dispatch and listed by team_action_list.
	maxCatalogSchemaEnum = 32
)

// CatalogInvocation is a coordinator's raw catalog_action request. It lives
// only between decoding and compilation; a compiled task carries a
// CatalogActionBinding instead.
type CatalogInvocation struct {
	ID        string
	Arguments json.RawMessage
}

func (i *CatalogInvocation) clone() *CatalogInvocation {
	if i == nil {
		return nil
	}
	clone := *i
	clone.Arguments = append(json.RawMessage(nil), i.Arguments...)
	return &clone
}

// teamActionDispatchError is a rejected catalog dispatch. It is answered with
// a recoverable response instead of the ordinary invalid-arguments error.
type teamActionDispatchError struct {
	Index  int
	Code   string
	Detail string
}

func (e *teamActionDispatchError) Error() string {
	return fmt.Sprintf("%s: tasks[%d]: %s", e.Code, e.Index, e.Detail)
}

func dispatchError(index int, code, format string, args ...any) *teamActionDispatchError {
	return &teamActionDispatchError{Index: index, Code: code, Detail: fmt.Sprintf(format, args...)}
}

// hasCatalogActionField reports whether a task object sets catalog_action,
// matching the key case-insensitively as encoding/json would.
func hasCatalogActionField(fields map[string]json.RawMessage) bool {
	for key := range fields {
		if strings.EqualFold(key, "catalog_action") {
			return true
		}
	}
	return false
}

// uniqueJSONObjectKeys returns an object's keys and rejects a key repeated
// exactly or with different case: both a map decode and Go's
// case-insensitive field matching would silently keep only one of them.
func uniqueJSONObjectKeys(raw []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("must be a JSON object")
	}
	var keys []string
	seen := make(map[string]bool)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyToken.(string)
		if seen[strings.ToLower(key)] {
			return nil, fmt.Errorf("key %q is repeated", key)
		}
		seen[strings.ToLower(key)] = true
		keys = append(keys, key)
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// decodeCatalogTaskDef decodes a task that carries catalog_action. Only
// agent, goal, constraints, and catalog_action are accepted, plus depends_on
// in a team without phases; everything else the runtime compiles from the
// catalog, so a model can never set it.
func decodeCatalogTaskDef(index int, raw json.RawMessage, workflowMode bool) (TaskDef, error) {
	keys, err := uniqueJSONObjectKeys(raw)
	if err != nil {
		return TaskDef{}, dispatchError(index, teamActionTaskInvalid, "%v", err)
	}
	allowed := []string{"agent", "goal", "constraints", "catalog_action"}
	if !workflowMode {
		allowed = append(allowed, "depends_on")
	}
	for _, key := range keys {
		if !slices.ContainsFunc(allowed, func(name string) bool { return strings.EqualFold(name, key) }) {
			return TaskDef{}, dispatchError(index, teamActionTaskFieldForbidden, "a catalog task may set only %s; %q is set by the catalog", strings.Join(allowed, ", "), key)
		}
	}
	var fields struct {
		Agent         string          `json:"agent"`
		Goal          string          `json:"goal"`
		Constraints   string          `json:"constraints"`
		DependsOn     []int           `json:"depends_on"`
		CatalogAction json.RawMessage `json:"catalog_action"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return TaskDef{}, dispatchError(index, teamActionTaskInvalid, "%v", err)
	}
	invocation, err := decodeCatalogInvocation(index, fields.CatalogAction)
	if err != nil {
		return TaskDef{}, err
	}
	return TaskDef{Agent: fields.Agent, Goal: fields.Goal, Constraints: fields.Constraints, DependsOn: fields.DependsOn, CatalogInvocation: invocation}, nil
}

func decodeCatalogInvocation(index int, raw json.RawMessage) (*CatalogInvocation, error) {
	if _, err := uniqueJSONObjectKeys(raw); err != nil {
		return nil, dispatchError(index, teamActionTaskInvalid, "catalog_action %v", err)
	}
	var request struct {
		ID        string          `json:"id"`
		Arguments json.RawMessage `json:"arguments"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, dispatchError(index, teamActionTaskInvalid, "catalog_action: %v", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, dispatchError(index, teamActionTaskInvalid, "catalog_action must be one JSON object")
	}
	arguments := bytes.TrimSpace(request.Arguments)
	switch {
	case len(arguments) == 0:
		arguments = []byte("{}")
	case arguments[0] != '{':
		return nil, dispatchError(index, teamActionArgumentsInvalid, "catalog_action.arguments must be a JSON object")
	}
	return &CatalogInvocation{ID: request.ID, Arguments: append(json.RawMessage(nil), arguments...)}, nil
}

// catalogDispatchPhase returns the phase a catalog entry would be dispatched
// in: "" for a team without phases, the current phase when it allows the
// entry, or ok=false when the current phase does not.
func (c *Coordinator) catalogDispatchPhase(entry ActionCatalogEntry) (Phase, bool) {
	if c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() {
		return "", true
	}
	state := c.phaseWorkflow.State()
	return state, state == PhaseExecute || state == PhasePrepare && entry.SideEffect == SideEffectNone
}

// catalogDecisionProfileBlocked reports whether a catalog task, whose own
// profile is off, would still resolve to an enabled decision profile (for
// example through a CLI --decision-profile override).
func (c *Coordinator) catalogDecisionProfileBlocked(task TaskDef) bool {
	task.DecisionProfile = DecisionProfileOff
	resolution, err := ResolveDecisionProfile(c.decisionConfig(), c.DecisionProfileOverride(), task)
	return err != nil || resolution.Enabled()
}

// teamActionInvocationsUsed counts the admitted catalog tasks of an action in
// this session, whether or not their provider ever started.
func (c *Coordinator) teamActionInvocationsUsed(actionID string) int {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return 0
	}
	used := 0
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.CatalogAction != nil && item.CatalogAction.ActionID == actionID && !IsPrimaryOccurrence(item) {
			used++
		}
	}
	return used
}

// catalogDispatchBlockedReason is the entry-level part of the dispatch
// checks, shared by the compiler and the coordinator's list/get. It returns a
// blocked_reason and the rejection code, or two empty strings.
func (c *Coordinator) catalogDispatchBlockedReason(entry ActionCatalogEntry, pendingInBatch int) (string, string) {
	switch {
	case c.initialDelegationPending():
		return "initial_batch", teamActionInitialBatchPending
	case !c.hasDurableEventJournal() || c.durableBranchID() == "":
		return "journal", teamActionJournalRequired
	}
	if _, ok := c.catalogDispatchPhase(entry); !ok {
		return "phase", teamActionPhaseForbidden
	}
	switch {
	case c.IsUnattended() && !entry.AllowUnattended:
		return "unattended", teamActionUnattendedDenied
	case c.teamActionInvocationsUsed(entry.ID)+pendingInBatch+1 > entry.MaxInvocations:
		return "budget", teamActionBudgetExceeded
	case c.catalogDecisionProfileBlocked(TaskDef{Agent: entry.Agent}):
		return "decision_profile", teamActionDecisionProfileForbidden
	}
	return "", ""
}

// compileCatalogActionTasks replaces every catalog request in tasks with the
// task the frozen catalog defines. The whole batch is rejected on the first
// failing request, before any task is created.
func (c *Coordinator) compileCatalogActionTasks(tasks []TaskDef) ([]TaskDef, error) {
	compiled := make([]TaskDef, len(tasks))
	seenArguments := make(map[string]bool)
	pending := make(map[string]int)
	for index, task := range tasks {
		if task.CatalogInvocation == nil {
			compiled[index] = task
			continue
		}
		next, err := c.compileCatalogActionTask(index, task, seenArguments, pending)
		if err != nil {
			return nil, err
		}
		compiled[index] = next
	}
	return compiled, nil
}

func (c *Coordinator) compileCatalogActionTask(index int, task TaskDef, seenArguments map[string]bool, pending map[string]int) (TaskDef, error) {
	request := task.CatalogInvocation
	if c.session == nil || c.session.ActionCatalog == nil {
		return TaskDef{}, dispatchError(index, teamActionCatalogAbsent, "this team declares no action catalog")
	}
	if c.initialDelegationPending() {
		return TaskDef{}, dispatchError(index, teamActionInitialBatchPending, "dispatch the required initial batch before any catalog action")
	}
	if !c.hasDurableEventJournal() || c.durableBranchID() == "" {
		return TaskDef{}, dispatchError(index, teamActionJournalRequired, "catalog actions need a durable event journal")
	}
	entry, ok := c.session.ActionCatalog.Lookup(request.ID)
	if !ok {
		return TaskDef{}, dispatchError(index, teamActionUnknown, "no catalog action %q; use team_action_list", request.ID)
	}
	if def, _, err := c.AgentPool().ResolveAgentName(task.Agent); err != nil || def == nil || normalizedName(def.Name) != entry.Agent {
		return TaskDef{}, dispatchError(index, teamActionAgentMismatch, "action %q runs as agent %q, not %q", entry.ID, entry.Agent, task.Agent)
	}
	phase, ok := c.catalogDispatchPhase(entry)
	if !ok {
		return TaskDef{}, dispatchError(index, teamActionPhaseForbidden, "action %q (%s) cannot run in phase %s", entry.ID, entry.SideEffect, c.phaseWorkflow.State())
	}
	if c.IsUnattended() && !entry.AllowUnattended {
		return TaskDef{}, dispatchError(index, teamActionUnattendedDenied, "action %q is not allowed in an unattended run", entry.ID)
	}
	arguments, argumentsHash, err := canonicalizeCatalogArguments(entry.InputSchema, request.Arguments)
	switch {
	case errors.Is(err, errCatalogArgumentsNotRedactionStable):
		return TaskDef{}, dispatchError(index, teamActionArgumentsNotStable, "%v", err)
	case err != nil:
		return TaskDef{}, dispatchError(index, teamActionArgumentsInvalid, "%v", err)
	}
	if identity := entry.ID + "\n" + argumentsHash; seenArguments[identity] {
		return TaskDef{}, dispatchError(index, teamActionDuplicateInBatch, "action %q is dispatched twice with the same arguments in this batch", entry.ID)
	} else {
		seenArguments[identity] = true
	}
	linked, recommended := c.linkedCatalogProposals(entry, argumentsHash)
	if entry.RequireProposal && !recommended {
		return TaskDef{}, dispatchError(index, teamActionProposalRequired, "action %q needs a worker proposal with assessment recommended for these arguments", entry.ID)
	}
	if c.teamActionInvocationsUsed(entry.ID)+pending[entry.ID]+1 > entry.MaxInvocations {
		return TaskDef{}, dispatchError(index, teamActionBudgetExceeded, "action %q already used its %d invocation(s) in this session", entry.ID, entry.MaxInvocations)
	}
	pending[entry.ID]++
	compiled := TaskDef{
		Agent: entry.Agent, Goal: task.Goal, Constraints: task.Constraints, DependsOn: slices.Clone(task.DependsOn), Phase: phase,
		Action:     &Action{Capability: entry.Capability, Type: entry.Type, Payload: string(arguments)},
		SideEffect: entry.SideEffect, Recovery: entry.Recovery, MaxRetries: 0, DecisionProfile: DecisionProfileOff,
		CatalogAction: &CatalogActionBinding{
			ActionID: entry.ID, EntryHash: entry.Hash, CatalogHash: c.session.ActionCatalog.Hash,
			ArgumentsHash: argumentsHash, ProposalIDs: linked,
		},
	}
	if c.catalogDecisionProfileBlocked(compiled) {
		return TaskDef{}, dispatchError(index, teamActionDecisionProfileForbidden, "catalog actions run without a decision profile, but this run selects one")
	}
	return compiled, nil
}

// linkedCatalogProposals returns the first proposal IDs (event order) for
// this entry and argument set, and whether one of them recommends running.
func (c *Coordinator) linkedCatalogProposals(entry ActionCatalogEntry, argumentsHash string) ([]string, bool) {
	var ids []string
	recommended := false
	for _, proposal := range c.matchingProposals(entry.ID, entry.Hash, argumentsHash) {
		if len(ids) == maxLinkedProposals {
			break
		}
		ids = append(ids, proposal.ProposalID)
		recommended = recommended || proposal.Assessment == "recommended"
	}
	return ids, recommended
}

// assignCatalogInvocationIDs gives each catalog task a durable invocation ID
// once its Todo ID is reserved, updating both the task and its spec so
// admission and task_created see the same binding.
func (c *Coordinator) assignCatalogInvocationIDs(tasks []TaskDef, specs []TodoSpec, ids []string) {
	branch := c.durableBranchID()
	for index := range tasks {
		if tasks[index].CatalogAction == nil || index >= len(ids) {
			continue
		}
		binding := tasks[index].CatalogAction.clone()
		sum := sha256.Sum256([]byte("hufu.team-action-invocation.v1\n" + branch + "\n" + ids[index] + "\n" + binding.EntryHash + "\n" + binding.ArgumentsHash))
		binding.InvocationID = "tai_" + hex.EncodeToString(sum[:])[:32]
		tasks[index].CatalogAction = binding
		specs[index].CatalogAction = binding.clone()
	}
}

// catalogDispatchRejected answers a rejected catalog dispatch. Up to three
// rejections per invocation get the recoverable TEAM ACTION DISPATCH
// REJECTED response; during policy repair or wrap-up, and after three, the
// rejection takes the ordinary delegation policy repair path.
func (t *runAgentsTool) catalogDispatchRejected(dispatchErr *teamActionDispatchError) (fantasy.ToolResponse, error) {
	c := t.coordinator
	if c.coordinatorPolicyRepairPending.Load() || (c.IsWrapUp() && !c.acceptanceRecovery.Load()) || c.teamActionRejections.Load() >= maxTeamActionRejections {
		var violation *delegationPolicyViolation
		if errors.As(c.rejectDelegationPolicy(dispatchErr.Error()), &violation) {
			return t.policyViolationResponse(violation)
		}
	}
	c.teamActionRejections.Add(1)
	return fantasy.NewTextErrorResponse(teamActionDispatchRejectedPrefix + " " + dispatchErr.Error() + "\n" + teamActionDispatchGuidance), nil
}

// policyViolationResponse answers a delegation policy violation with the
// bounded policy repair instruction.
func (t *runAgentsTool) policyViolationResponse(violation *delegationPolicyViolation) (fantasy.ToolResponse, error) {
	response := t.coordinator.coordinatorPolicyRepairResponse(violation)
	if t.coordinator.coordinatorPolicyRepairExhausted.Load() {
		return response, errCoordinatorPolicyRepairExhausted
	}
	return response, nil
}

// compileCatalogRequests admits the execution policy and compiles the
// batch's catalog requests. A policy drift keeps its existing fatal path.
func (t *runAgentsTool) compileCatalogRequests(tasks []TaskDef) ([]TaskDef, *fantasy.ToolResponse, error) {
	if !slices.ContainsFunc(tasks, func(task TaskDef) bool { return task.CatalogInvocation != nil }) {
		return tasks, nil, nil
	}
	if err := t.coordinator.AdmitExecutionPolicy(); err != nil {
		fatal := markCoordinatorFatal(err)
		response := renderRunAgentsToolResponse("", fatal)
		return nil, &response, fatal
	}
	compiled, err := t.coordinator.compileCatalogActionTasks(tasks)
	var dispatchErr *teamActionDispatchError
	if errors.As(err, &dispatchErr) {
		response, responseErr := t.catalogDispatchRejected(dispatchErr)
		return nil, &response, responseErr
	}
	return compiled, nil, err
}

// withoutCatalogTasks returns the tasks that are not catalog actions, for
// policy stages that apply only to model workers.
func withoutCatalogTasks(tasks []TaskDef) []TaskDef {
	ordinary := make([]TaskDef, 0, len(tasks))
	for _, task := range tasks {
		if task.CatalogAction == nil {
			ordinary = append(ordinary, task)
		}
	}
	return ordinary
}

// rejectUncompiledCatalogTasks is ExecuteTasks' defense against a catalog
// request that reached it without being compiled.
func (c *Coordinator) rejectUncompiledCatalogTasks(tasks []TaskDef) error {
	if slices.ContainsFunc(tasks, func(task TaskDef) bool { return task.CatalogInvocation != nil }) {
		return c.rejectDelegationPolicy("catalog_action was not compiled")
	}
	return nil
}

// catalogActionSchemaProperty is the agent tool's catalog_action property,
// or nil when no entry can be dispatched in the current phase. It depends
// only on the catalog and phase, so the schema is stable within a phase.
func (c *Coordinator) catalogActionSchemaProperty() map[string]any {
	if c == nil || c.session == nil || c.session.ActionCatalog == nil || c.initialDelegationPending() {
		return nil
	}
	var ids []string
	for _, entry := range c.session.ActionCatalog.Entries {
		if _, ok := c.catalogDispatchPhase(entry); ok {
			ids = append(ids, entry.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	id := map[string]any{"type": "string", "enum": ids}
	if len(ids) > maxCatalogSchemaEnum {
		id = map[string]any{"type": "string", "description": "Catalog action ID; see team_action_list."}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":        id,
			"arguments": map[string]any{"type": "object", "description": "Arguments matching the action's input schema (see team_action_get)."},
		},
		"required":             []string{"id"},
		"additionalProperties": false,
	}
}
