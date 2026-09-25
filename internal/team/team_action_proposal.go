package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

const (
	teamActionProposalSchemaVersion   = 1
	maxProposalEvidenceRefs           = 32
	maxProposalRationaleRunes         = 4000
	maxProposalExpectedOutcomeRunes   = 1000
	maxProposalsPerSession            = 256
	teamActionJournalRequired         = "team_action_journal_required"
	teamActionProposalForbidden       = "team_action_proposal_forbidden"
	teamActionArgumentsInvalid        = "team_action_arguments_invalid"
	teamActionArgumentsNotStable      = "team_action_arguments_not_redaction_stable"
	teamActionEvidenceInvalid         = "team_action_evidence_invalid"
	teamActionProposalLimitExceeded   = "team_action_proposal_limit_exceeded"
	teamActionProposalConflict        = "team_action_proposal_conflict"
	teamActionProposalAppendFailed    = "team_action_proposal_append_failed"
	teamActionProposalAssessmentField = "assessment"
)

var teamActionAssessments = []string{"recommended", "candidate", "defer", "reject"}

// TeamActionEvidenceRef is an artifact a proposal cites as evidence.
type TeamActionEvidenceRef struct {
	ID             string `json:"id"`
	SHA256         string `json:"sha256"`
	ProducerTaskID string `json:"producer_task_id"`
}

// TeamActionProposedPayload is the durable team_action_proposed payload.
type TeamActionProposedPayload struct {
	SchemaVersion      int                     `json:"schema_version"`
	Status             string                  `json:"status"`
	ProposalID         string                  `json:"proposal_id"`
	ActionID           string                  `json:"action_id"`
	EntryHash          string                  `json:"entry_hash"`
	CatalogHash        string                  `json:"catalog_hash"`
	Agent              string                  `json:"agent"`
	OccurrenceRevision int                     `json:"occurrence_revision"`
	Attempt            int                     `json:"attempt"`
	Arguments          json.RawMessage         `json:"arguments"`
	ArgumentsHash      string                  `json:"arguments_hash"`
	Assessment         string                  `json:"assessment"`
	Rationale          string                  `json:"rationale"`
	ExpectedOutcome    string                  `json:"expected_outcome,omitempty"`
	EvidenceRefs       []TeamActionEvidenceRef `json:"evidence_refs,omitempty"`
}

// TeamActionProposal is one recorded proposal in the session index.
type TeamActionProposal struct {
	TeamActionProposedPayload
	TaskID         string
	IdempotencyKey string
}

// contentHash identifies what a retried call must repeat to count as the same
// proposal: its assessment, rationale, expected outcome, and evidence.
func (p TeamActionProposedPayload) contentHash() string {
	encoded, _ := json.Marshal(struct {
		Assessment      string                  `json:"assessment"`
		Rationale       string                  `json:"rationale"`
		ExpectedOutcome string                  `json:"expected_outcome"`
		EvidenceRefs    []TeamActionEvidenceRef `json:"evidence_refs"`
	}{p.Assessment, p.Rationale, p.ExpectedOutcome, p.EvidenceRefs})
	return runInputHash(encoded)
}

// teamActionProposalIndex is the session-scoped proposal index, in event
// order. It is rebuilt from the active branch on every public entry.
type teamActionProposalIndex struct {
	mu        sync.Mutex
	proposals []TeamActionProposal
	byKey     map[string]int
}

func (x *teamActionProposalIndex) resetLocked() {
	x.proposals = nil
	x.byKey = make(map[string]int)
}

func (x *teamActionProposalIndex) addLocked(proposal TeamActionProposal) {
	if x.byKey == nil {
		x.byKey = make(map[string]int)
	}
	if _, exists := x.byKey[proposal.IdempotencyKey]; exists {
		return
	}
	x.byKey[proposal.IdempotencyKey] = len(x.proposals)
	x.proposals = append(x.proposals, proposal)
}

// rebuildTeamActionProposals replaces the index with the proposals recorded on
// the active branch. It is called from initEventStore.
func (c *Coordinator) rebuildTeamActionProposals(events []RunEvent) {
	c.actionProposals.mu.Lock()
	defer c.actionProposals.mu.Unlock()
	c.actionProposals.resetLocked()
	for _, event := range events {
		if event.Type != string(EventTeamActionProposed) {
			continue
		}
		var payload TeamActionProposedPayload
		if json.Unmarshal(event.Payload, &payload) != nil || strings.TrimSpace(event.IdempotencyKey) == "" {
			continue
		}
		c.actionProposals.addLocked(TeamActionProposal{TeamActionProposedPayload: payload, TaskID: event.TaskID, IdempotencyKey: event.IdempotencyKey})
	}
}

// resetTeamActionProposals empties the index before it is rebuilt.
func (c *Coordinator) resetTeamActionProposals() {
	c.actionProposals.mu.Lock()
	defer c.actionProposals.mu.Unlock()
	c.actionProposals.resetLocked()
}

// matchingProposals returns, in event order, the proposals recorded for this
// action, entry, and argument set.
func (c *Coordinator) matchingProposals(actionID, entryHash, argumentsHash string) []TeamActionProposal {
	c.actionProposals.mu.Lock()
	defer c.actionProposals.mu.Unlock()
	var matched []TeamActionProposal
	for _, proposal := range c.actionProposals.proposals {
		if proposal.ActionID == actionID && proposal.EntryHash == entryHash && proposal.ArgumentsHash == argumentsHash {
			matched = append(matched, proposal)
		}
	}
	return matched
}

// proposalsForAction returns an action's proposals, newest first.
func (c *Coordinator) proposalsForAction(actionID string) []TeamActionProposal {
	c.actionProposals.mu.Lock()
	defer c.actionProposals.mu.Unlock()
	var result []TeamActionProposal
	for index := len(c.actionProposals.proposals) - 1; index >= 0; index-- {
		if proposal := c.actionProposals.proposals[index]; proposal.ActionID == actionID {
			result = append(result, proposal)
		}
	}
	return result
}

func (c *Coordinator) proposalCount() int {
	c.actionProposals.mu.Lock()
	defer c.actionProposals.mu.Unlock()
	return len(c.actionProposals.proposals)
}

// durableBranchID is the event-store branch that proposal and invocation IDs
// are bound to. It reads the store's own branch rather than re-reading the
// session tree, and is empty when there is no durable event store.
func (c *Coordinator) durableBranchID() string {
	if c == nil || c.eventStore == nil {
		return ""
	}
	return c.eventStore.BranchID()
}

type teamActionProposeInput struct {
	Action          string          `json:"action"`
	Arguments       json.RawMessage `json:"arguments"`
	Assessment      string          `json:"assessment"`
	Rationale       string          `json:"rationale"`
	ExpectedOutcome string          `json:"expected_outcome"`
	EvidenceRefs    []string        `json:"evidence_refs"`
}

type teamActionProposeTool struct {
	coordinator *Coordinator
	todoID      string
	agent       string
}

func (*teamActionProposeTool) DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor {
	return tools.ToolWorkspaceScopeDescriptor{}
}
func (*teamActionProposeTool) boundArtifactPolicyTool() {}
func (*teamActionProposeTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (*teamActionProposeTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t *teamActionProposeTool) Info() fantasy.ToolInfo {
	ids := make([]string, 0)
	if t.coordinator != nil && t.coordinator.session != nil && t.coordinator.session.ActionCatalog != nil {
		for _, entry := range t.coordinator.session.ActionCatalog.Entries {
			if slices.Contains(entry.Propose, t.agent) {
				ids = append(ids, entry.ID)
			}
		}
	}
	return fantasy.ToolInfo{
		Name: teamActionProposeToolName,
		Description: "Record a recommendation that the coordinator run one predefined action with specific arguments. " +
			"This does not run the action or create a task; the coordinator decides. Check the input schema with team_action_get first.",
		Parameters: map[string]any{
			"action":           map[string]any{"type": "string", "enum": ids, "description": "Action ID to recommend."},
			"arguments":        map[string]any{"type": "object", "description": "Arguments matching the action's input schema."},
			"assessment":       map[string]any{"type": "string", "enum": teamActionAssessments, "description": "recommended: run it; candidate: worth considering; defer: not yet; reject: do not run it."},
			"rationale":        map[string]any{"type": "string", "description": "Why, citing what you observed (at most 4000 characters)."},
			"expected_outcome": map[string]any{"type": "string", "description": "Optional: what running it should show (at most 1000 characters)."},
			"evidence_refs":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional artifact IDs that support the recommendation (at most 32)."},
		},
		Required: []string{"action", teamActionProposalAssessmentField, "rationale"},
	}
}

func (t *teamActionProposeTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	c := t.coordinator
	identity, ok := t.callerIdentity(ctx)
	if !ok {
		return teamActionErrorResponse(teamActionCallerInvalid, "this tool belongs to a different task, agent, or attempt"), nil
	}
	if !c.hasDurableEventJournal() || c.durableBranchID() == "" {
		return teamActionErrorResponse(teamActionJournalRequired, "proposals need a durable event journal"), nil
	}
	var input teamActionProposeInput
	if err := decodeTeamActionInput(call.Input, &input); err != nil {
		return teamActionErrorResponse(teamActionInvalidToolRequest, err.Error()), nil
	}
	entry, ok := c.session.ActionCatalog.Lookup(input.Action)
	if !ok || !slices.Contains(entry.Propose, t.agent) {
		return teamActionErrorResponse(teamActionProposalForbidden, fmt.Sprintf("you may not propose action %q", input.Action)), nil
	}
	payload, failure := t.proposalPayload(ctx, entry, identity, input)
	if failure != nil {
		return *failure, nil
	}
	key := fmt.Sprintf("team-action-proposal:v1:%s:%d:%d:%s:%s", t.todoID, identity.OccurrenceRevision, identity.Attempt, entry.ID, payload.ArgumentsHash)
	sum := sha256.Sum256([]byte(c.durableBranchID() + "\n" + key))
	payload.ProposalID = "tap_" + hex.EncodeToString(sum[:])[:24]
	duplicate, response := c.recordTeamActionProposal(ctx, t.todoID, t.agent, identity.Attempt, key, payload)
	if response != nil {
		return *response, nil
	}
	return teamActionJSONResponse(map[string]any{
		"proposal_id": payload.ProposalID, "action": entry.ID, "assessment": payload.Assessment,
		"arguments_hash": payload.ArgumentsHash, "status": "recorded", "duplicate": duplicate,
	})
}

// callerIdentity checks the shared caller rules and that the call belongs to
// the task's currently open result occurrence.
func (t *teamActionProposeTool) callerIdentity(ctx context.Context) (submitResultRuntimeIdentity, bool) {
	if !teamActionCallerValid(ctx, t.todoID, t.agent) {
		return submitResultRuntimeIdentity{}, false
	}
	identity, err := submitResultRuntimeIdentityFromContext(ctx, t.coordinator, t.todoID)
	if err != nil {
		return submitResultRuntimeIdentity{}, false
	}
	active, ok := t.coordinator.activeTaskResultOccurrence(t.todoID)
	if !ok || !sameTaskResultOccurrence(active, identity) {
		return submitResultRuntimeIdentity{}, false
	}
	return identity, true
}

// proposalPayload validates the arguments, text, and evidence of a proposal
// and returns its payload without a proposal ID.
func (t *teamActionProposeTool) proposalPayload(ctx context.Context, entry ActionCatalogEntry, identity submitResultRuntimeIdentity, input teamActionProposeInput) (TeamActionProposedPayload, *fantasy.ToolResponse) {
	fail := func(code, detail string) (TeamActionProposedPayload, *fantasy.ToolResponse) {
		response := teamActionErrorResponse(code, detail)
		return TeamActionProposedPayload{}, &response
	}
	if !slices.Contains(teamActionAssessments, input.Assessment) {
		return fail(teamActionInvalidToolRequest, "assessment must be recommended, candidate, defer, or reject")
	}
	rawArguments := []byte(input.Arguments)
	if len(bytes.TrimSpace(rawArguments)) == 0 {
		rawArguments = []byte("{}")
	}
	arguments, argumentsHash, err := canonicalizeCatalogArguments(entry.InputSchema, rawArguments)
	if errors.Is(err, errCatalogArgumentsNotRedactionStable) {
		return fail(teamActionArgumentsNotStable, err.Error())
	}
	if err != nil {
		return fail(teamActionArgumentsInvalid, err.Error())
	}
	rationale := utils.TruncateRunes(strings.TrimSpace(utils.RedactSecrets(input.Rationale)), maxProposalRationaleRunes)
	if rationale == "" {
		return fail(teamActionInvalidToolRequest, "rationale must not be empty")
	}
	evidence, err := t.coordinator.proposalEvidence(ctx, input.EvidenceRefs)
	if err != nil {
		return fail(teamActionEvidenceInvalid, err.Error())
	}
	if t.coordinator.proposalCount() >= maxProposalsPerSession {
		return fail(teamActionProposalLimitExceeded, fmt.Sprintf("this session already has %d proposals", maxProposalsPerSession))
	}
	return TeamActionProposedPayload{
		SchemaVersion: teamActionProposalSchemaVersion, Status: "proposed", ActionID: entry.ID, EntryHash: entry.Hash,
		CatalogHash: t.coordinator.session.ActionCatalog.Hash, Agent: t.agent, OccurrenceRevision: identity.OccurrenceRevision,
		Attempt: identity.Attempt, Arguments: arguments, ArgumentsHash: argumentsHash, Assessment: input.Assessment, Rationale: rationale,
		ExpectedOutcome: utils.TruncateRunes(strings.TrimSpace(utils.RedactSecrets(input.ExpectedOutcome)), maxProposalExpectedOutcomeRunes),
		EvidenceRefs:    evidence,
	}, nil
}

// proposalEvidence resolves cited artifact IDs through the caller's artifact
// authorization. Only artifact IDs are accepted; a path is never resolved.
func (c *Coordinator) proposalEvidence(ctx context.Context, ids []string) ([]TeamActionEvidenceRef, error) {
	if len(ids) > maxProposalEvidenceRefs {
		return nil, fmt.Errorf("at most %d evidence refs are allowed", maxProposalEvidenceRefs)
	}
	var refs []TeamActionEvidenceRef
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || strings.ContainsAny(id, `/\`) || strings.HasPrefix(id, ".") {
			return nil, fmt.Errorf("evidence ref %q must be an artifact ID, not a path", id)
		}
		ref, producer, ok := c.authorizedArtifactRef(ctx, id)
		if !ok || ref.ID != id || ref.SHA256 == "" || producer == "" || (ref.TaskID != "" && ref.TaskID != producer) {
			return nil, fmt.Errorf("evidence ref %q is not an artifact you are authorized to cite", id)
		}
		refs = append(refs, TeamActionEvidenceRef{ID: ref.ID, SHA256: ref.SHA256, ProducerTaskID: producer})
	}
	return refs, nil
}

// recordTeamActionProposal appends a new proposal or recognizes a retried one.
// Looking up, appending, and indexing happen under one lock so concurrent
// workers cannot append the same key twice. It reports whether the proposal
// was already recorded, or a tool error response.
func (c *Coordinator) recordTeamActionProposal(ctx context.Context, todoID, agentName string, attempt int, key string, payload TeamActionProposedPayload) (bool, *fantasy.ToolResponse) {
	c.actionProposals.mu.Lock()
	defer c.actionProposals.mu.Unlock()
	if index, exists := c.actionProposals.byKey[key]; exists {
		if c.actionProposals.proposals[index].contentHash() == payload.contentHash() {
			return true, nil
		}
		response := teamActionErrorResponse(teamActionProposalConflict, "this task already recorded a different proposal for these arguments in this attempt")
		return false, &response
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		response := teamActionErrorResponse(teamActionProposalAppendFailed, err.Error())
		return false, &response
	}
	record, err := c.EventJournal().Append(context.WithoutCancel(ctx), RunEvent{
		Type: string(EventTeamActionProposed), Actor: agentName, TaskID: todoID, Attempt: attempt, IdempotencyKey: key, Payload: raw,
	})
	if err != nil {
		response := teamActionErrorResponse(teamActionProposalAppendFailed, utils.RedactSecrets(err.Error()))
		return false, &response
	}
	var durable TeamActionProposedPayload
	if err := json.Unmarshal(record.Payload, &durable); err != nil {
		durable = payload
	}
	c.actionProposals.addLocked(TeamActionProposal{TeamActionProposedPayload: durable, TaskID: todoID, IdempotencyKey: key})
	return false, nil
}

// validateTeamActionProposedPayload strictly decodes a team_action_proposed
// payload and checks its identity fields.
func validateTeamActionProposedPayload(event RunEvent) error {
	if strings.TrimSpace(event.TaskID) == "" {
		return fmt.Errorf("%s event has empty task id", event.Type)
	}
	var payload TeamActionProposedPayload
	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("decode %s payload: %w", event.Type, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("decode %s payload: trailing JSON value", event.Type)
	} else if err != io.EOF {
		return fmt.Errorf("decode %s trailing JSON: %w", event.Type, err)
	}
	switch {
	case payload.SchemaVersion != teamActionProposalSchemaVersion:
		return fmt.Errorf("%s payload schema_version %d is unsupported", event.Type, payload.SchemaVersion)
	case !strings.HasPrefix(payload.ProposalID, "tap_") || payload.ActionID == "" || payload.Agent == "":
		return fmt.Errorf("%s payload lacks proposal identity", event.Type)
	case !slices.Contains(teamActionAssessments, payload.Assessment):
		return fmt.Errorf("%s payload assessment %q is invalid", event.Type, payload.Assessment)
	}
	return nil
}
