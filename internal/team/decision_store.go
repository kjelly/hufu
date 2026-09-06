package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// Decision persistence and resume projection
// (docs/hufu-decision-aware-runtime-spec.md §35, §38).
//
// Decision state is reconstructed from the append-only event log rather than a
// separate mutable store, so a crash mid-round resumes by dispatching only what
// is missing and never silently regenerates a completed stage.

// decisionActor is the event actor for runtime-owned decision procedure. The
// runtime, not any agent, owns these transitions.
const decisionActor = "decision-runtime"

// decisionEvent is the payload carried by every decision event type. Fields are
// optional so one shape covers the whole lifecycle without a type switch at
// every call site.
type decisionEvent struct {
	DecisionID   string `json:"decision_id"`
	RunID        string `json:"run_id,omitempty"`
	TaskID       string `json:"task_id,omitempty"`
	Attempt      int    `json:"attempt,omitempty"`
	Profile      string `json:"profile,omitempty"`
	EvidenceHash string `json:"evidence_hash,omitempty"`
	Round        int    `json:"round,omitempty"`
	JudgeID      string `json:"judge_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// Assumption transition fields are structured canonical evidence; Reason is
	// retained for human-readable event summaries and backward compatibility.
	AssumptionID string        `json:"assumption_id,omitempty"`
	From         string        `json:"from,omitempty"`
	To           string        `json:"to,omitempty"`
	Source       string        `json:"source,omitempty"`
	EvidenceRefs []ArtifactRef `json:"evidence_refs,omitempty"`
	Note         string        `json:"note,omitempty"`
	At           time.Time     `json:"at,omitzero"`

	Packet                *DecisionEvidencePacket      `json:"packet,omitempty"`
	Opinion               *DecisionOpinion             `json:"opinion,omitempty"`
	Aggregate             *DecisionAggregate           `json:"aggregate,omitempty"`
	Challenge             *DecisionChallenge           `json:"challenge,omitempty"`
	Revision              *DecisionRevision            `json:"revision,omitempty"`
	Premortem             *PremortemResult             `json:"premortem,omitempty"`
	Options               []DecisionOption             `json:"options,omitempty"`
	Record                *DecisionRecord              `json:"record,omitempty"`
	Question              string                       `json:"question,omitempty"`
	ForecastRequired      bool                         `json:"forecast_required,omitempty"`
	RecordRef             ArtifactRef                  `json:"record_ref,omitempty"`
	EvidenceArtifact      ArtifactRef                  `json:"evidence_artifact,omitempty"`
	ContractRef           string                       `json:"contract_ref,omitempty"`
	ContractRevision      uint64                       `json:"contract_revision,omitempty"`
	ContractArtifact      ArtifactRef                  `json:"contract_artifact,omitempty"`
	Degradation           *DecisionDegradation         `json:"degradation,omitempty"`
	ReferenceInvocation   *ReferenceEvidenceInvocation `json:"reference_invocation,omitempty"`
	ReferenceResult       *ReferenceEvidenceResult     `json:"reference_result,omitempty"`
	ReferenceFailure      *ReferenceEvidenceFailure    `json:"reference_failure,omitempty"`
	EnvelopeRef           ArtifactRef                  `json:"envelope_ref,omitempty"`
	EnvelopeHash          string                       `json:"envelope_hash,omitempty"`
	Finalization          *DecisionFinalizationResult  `json:"finalization,omitempty"`
	FinalizationResultRef ArtifactRef                  `json:"finalization_result_ref,omitempty"`
	FinalizationMode      string                       `json:"finalization_mode,omitempty"`
	FinalizationJudgeID   string                       `json:"finalization_judge_id,omitempty"`

	// IdempotencyKey is carried by the outer RunEvent, not the decision payload.
	// It is intentionally excluded from JSON so replay identity cannot become
	// part of the decision's immutable content.
	IdempotencyKey string `json:"-"`

	// JudgeAliases records the anonymization mapping a challenger was NOT
	// given, so the run stays auditable without ever revealing identity to the
	// challenger itself (spec §23).
	JudgeAliases map[string]string `json:"judge_aliases,omitempty"`
}

// decisionState is the projection rebuilt from the event log.
type decisionState struct {
	DecisionID                   string
	RunID                        string
	TaskID                       string
	Attempt                      int
	Profile                      string
	Packet                       DecisionEvidencePacket
	ProposedOptions              []DecisionOption
	Opinions                     []DecisionOpinion
	Aggregates                   map[int]DecisionAggregate
	Challenges                   []DecisionChallenge
	Revisions                    []DecisionRevision
	Premortem                    *PremortemResult
	ChallengeSkipReason          string
	ChallengeSkipEvidenceHash    string
	Degradations                 []DecisionDegradation
	Record                       *DecisionRecord
	ContractRef                  string
	ContractRevision             uint64
	ContractArtifact             ArtifactRef
	EvidenceArtifact             ArtifactRef
	ReferenceInvocation          *ReferenceEvidenceInvocation
	ReferenceResult              *ReferenceEvidenceResult
	ReferenceEvidenceResultRef   ArtifactRef
	ReferenceFailure             *ReferenceEvidenceFailure
	EnvelopeRef                  ArtifactRef
	Finalization                 *DecisionFinalizationResult
	FinalizationResultRef        ArtifactRef
	FinalizationMode             string
	FinalizationJudgeID          string
	FinalizedRecordRef           ArtifactRef
	FinalizationQuestion         string
	FinalizationForecastRequired bool
	CanonicalRecord              *DecisionRecord
	Invalidated                  bool
	// StaleHashes are evidence hashes superseded by a later seal. Opinions
	// formed on them are durable but must not be aggregated (spec §15.4).
	StaleHashes map[string]bool
}

// decisionEventFor binds an event to the immutable task occurrence and gives
// each durable stage a semantic, replay-stable idempotency key.
func decisionEventFor(req DecisionRequest, stage string, parts ...string) decisionEvent {
	return decisionEvent{
		DecisionID:     req.DecisionID,
		RunID:          req.RunID,
		TaskID:         req.TaskID,
		Attempt:        req.Attempt,
		IdempotencyKey: decisionStageEventKey(req.DecisionID, stage, parts...),
	}
}

// OpinionsForRound returns the opinions recorded for one round, in judge order.
func (s decisionState) OpinionsForRound(round int) []DecisionOpinion {
	var out []DecisionOpinion
	for _, opinion := range s.Opinions {
		if opinion.Round == round && opinion.EvidenceHash == s.Packet.Hash {
			out = append(out, opinion)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JudgeID < out[j].JudgeID })
	return out
}

// JudgesWithValidOpinion returns the judges whose *valid* opinion for a round
// is already durable on the current evidence, so resume dispatches only the
// missing ones. A rejected opinion stays durable for audit but does not count
// as completed work: a judge that crashed or answered unusably must be able to
// answer again in a later run, while the in-run repair bound still stops the
// runtime from grinding out a quorum (spec §14.3, §38.1).
func (s decisionState) JudgesWithValidOpinion(round int) map[string]bool {
	seen := map[string]bool{}
	for _, opinion := range s.OpinionsForRound(round) {
		if opinion.Valid {
			seen[opinion.JudgeID] = true
		}
	}
	return seen
}

// ChallengesForHash returns the durable challenges formed on the current
// evidence, so a resume never re-runs a challenger that already answered.
func (s decisionState) ChallengesForHash() []DecisionChallenge {
	var out []DecisionChallenge
	for _, challenge := range s.Challenges {
		if challenge.EvidenceHash == s.Packet.Hash {
			out = append(out, challenge)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RevisionsForHash returns the durable revisions formed on the current
// evidence, keyed lookup left to the caller.
func (s decisionState) RevisionsForHash() []DecisionRevision {
	var out []DecisionRevision
	for _, revision := range s.Revisions {
		if revision.EvidenceHash == s.Packet.Hash {
			out = append(out, revision)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JudgeID < out[j].JudgeID })
	return out
}

// decisionJournal is the persistence surface the engine needs. EventJournal
// already satisfies it.
type decisionJournal interface {
	Append(context.Context, RunEvent) (RunEvent, error)
	ReadEvents(context.Context) ([]RunEvent, error)
}

// appendDecisionEvent writes one decision event.
func appendDecisionEvent(ctx context.Context, journal decisionJournal, eventType string, payload decisionEvent) error {
	_, err := appendDecisionEventResult(ctx, journal, eventType, payload)
	return err
}

func appendDecisionEventResult(ctx context.Context, journal decisionJournal, eventType string, payload decisionEvent) (RunEvent, error) {
	if journal == nil {
		return RunEvent{}, fmt.Errorf("decision event journal is unavailable")
	}
	if err := enforceDecisionEventProvenance(ctx, journal, &payload); err != nil {
		return RunEvent{}, fmt.Errorf("append %s event: %w", eventType, err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return RunEvent{}, fmt.Errorf("encoding %s payload: %w", eventType, err)
	}
	key := payload.IdempotencyKey
	if key == "" {
		key = decisionEventKey(eventType, payload)
	}
	persisted, err := journal.Append(ctx, RunEvent{Type: eventType, Actor: decisionActor, IdempotencyKey: key, RunID: payload.RunID, TaskID: payload.TaskID, Attempt: payload.Attempt, Payload: raw})
	if err != nil {
		return RunEvent{}, fmt.Errorf("append %s event: %w", eventType, err)
	}
	return persisted, nil
}

func decisionEventKey(eventType string, payload decisionEvent) string {
	// These lifecycle events predate decisionEventFor at some call sites. Their
	// fallback key must still identify the semantic transition, not the mutable
	// payload timestamp or a generated artifact/record ID.
	switch eventType {
	case agent.EventDecisionBudgetDegraded:
		if payload.Degradation != nil {
			return decisionStageEventKey(payload.DecisionID, "budget_degraded",
				payload.Degradation.Step, payload.Degradation.From, payload.Degradation.To)
		}
	case agent.EventDecisionStarted:
		if payload.Profile == "" {
			return decisionStageEventKey(payload.DecisionID, "execution_armed")
		}
	case agent.EventCommitGateBlocked:
		return decisionStageEventKey(payload.DecisionID, "commit_gate_blocked", payload.Reason)
	case agent.EventKillCriterionTriggered:
		return decisionStageEventKey(payload.DecisionID, "kill_criterion_triggered", payload.Reason)
	case agent.EventDecisionInvalidated:
		return decisionStageEventKey(payload.DecisionID, "invalidated", payload.Reason)
	case agent.EventReplanRequested:
		return decisionStageEventKey(payload.DecisionID, "replan_requested", payload.Reason)
	case agent.EventAssumptionSupported, agent.EventAssumptionContradicted, agent.EventAssumptionStale:
		return decisionStageEventKey(payload.DecisionID, "assumption_transition",
			payload.AssumptionID, payload.From, payload.To, payload.Source)
	}

	// Keep the generic compatibility path deterministic too. The selected fields
	// are the event's semantic occurrence/stage identity; At and nested generated
	// IDs are deliberately excluded.
	identity := struct {
		DecisionID   string
		RunID        string
		TaskID       string
		Attempt      int
		Profile      string
		EvidenceHash string
		Round        int
		JudgeID      string
		Reason       string
		AssumptionID string
		From         string
		To           string
		Source       string
		Note         string
		ContractRef  string
		ContractRev  uint64
	}{
		DecisionID: payload.DecisionID, RunID: payload.RunID, TaskID: payload.TaskID,
		Attempt: payload.Attempt, Profile: payload.Profile, EvidenceHash: payload.EvidenceHash,
		Round: payload.Round, JudgeID: payload.JudgeID, Reason: payload.Reason,
		AssumptionID: payload.AssumptionID, From: payload.From, To: payload.To,
		Source: payload.Source, Note: payload.Note, ContractRef: payload.ContractRef,
		ContractRev: payload.ContractRevision,
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return decisionStageEventKey(payload.DecisionID, eventType)
	}
	digest := sha256.Sum256(raw)
	return decisionStageEventKey(payload.DecisionID, eventType, hex.EncodeToString(digest[:]))
}

// enforceDecisionEventProvenance is the single append-side guard for decision
// lifecycle events. Once the immutable run envelope is anchored, no later
// event may silently fall back to the current coordinator run identity. Older
// pre-envelope records remain readable and writable for compatibility, but
// cannot acquire a fabricated occurrence identity here.
func enforceDecisionEventProvenance(ctx context.Context, journal decisionJournal, payload *decisionEvent) error {
	if payload == nil || strings.TrimSpace(payload.DecisionID) == "" {
		return fmt.Errorf("decision event requires a decision identity")
	}
	state, err := projectDecision(ctx, journal, payload.DecisionID)
	if err != nil {
		return err
	}
	if state.EnvelopeRef.ID == "" {
		return nil
	}
	if payload.RunID != "" && payload.RunID != state.RunID {
		return fmt.Errorf("decision event run identity conflicts with anchored occurrence")
	}
	if payload.TaskID != "" && payload.TaskID != state.TaskID {
		return fmt.Errorf("decision event task identity conflicts with anchored occurrence")
	}
	if payload.Attempt != 0 && payload.Attempt != state.Attempt {
		return fmt.Errorf("decision event attempt identity conflicts with anchored occurrence")
	}
	payload.RunID = state.RunID
	payload.TaskID = state.TaskID
	payload.Attempt = state.Attempt
	if payload.RunID == "" || payload.TaskID == "" || payload.Attempt < 1 {
		return &LegacyUnfinishedDecisionError{DecisionID: payload.DecisionID}
	}
	return nil
}

// projectDecision rebuilds one decision's state from the event log.
func projectDecision(ctx context.Context, journal decisionJournal, decisionID string) (decisionState, error) {
	state := decisionState{
		DecisionID:  decisionID,
		Aggregates:  map[int]DecisionAggregate{},
		StaleHashes: map[string]bool{},
	}
	seenKeys := map[string]RunEvent{}
	if journal == nil {
		return state, fmt.Errorf("decision event journal is unavailable")
	}
	events, err := journal.ReadEvents(ctx)
	if err != nil {
		return state, fmt.Errorf("reading decision events: %w", err)
	}
	for _, event := range events {
		if len(event.Payload) == 0 {
			continue
		}
		isReferenceEvent := event.Type == agent.EventDecisionReferenceStarted || event.Type == agent.EventDecisionReferenceCompleted || event.Type == agent.EventDecisionReferenceFailed
		var payload decisionEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			if isReferenceEvent {
				return state, fmt.Errorf("projecting %s event: decode payload: %w", event.Type, err)
			}
			// Foreign event types share the log; skip anything that is not a
			// decision payload rather than failing the whole projection.
			continue
		}
		if payload.DecisionID != decisionID {
			continue
		}
		if key := event.IdempotencyKey; key != "" {
			if _, seen := seenKeys[key]; seen {
				// Event-store idempotency normally returns the original event, but
				// replay may encounter duplicate log entries from an alternate
				// journal. Re-apply finalization-result duplicates so an altered
				// full artifact reference cannot be hidden by key deduplication.
				if event.Type == agent.EventDecisionFinalizationResult {
					if err := applyDecisionEvent(&state, event, payload); err != nil {
						return state, fmt.Errorf("projecting %s event: %w", event.Type, err)
					}
				}
				continue
			}
			seenKeys[key] = event
		}
		if err := applyDecisionEvent(&state, event, payload); err != nil {
			return state, fmt.Errorf("projecting %s event: %w", event.Type, err)
		}
	}
	return state, nil
}

func applyDecisionEvent(state *decisionState, event RunEvent, payload decisionEvent) error {
	if err := state.applyEventIdentity(event, payload); err != nil {
		return err
	}
	eventType := event.Type
	if eventType == agent.EventDecisionRunEnvelopeAnchored {
		return state.applyEnvelopeAnchor(payload)
	}
	if eventType == agent.EventDecisionReferenceStarted || eventType == agent.EventDecisionReferenceCompleted || eventType == agent.EventDecisionReferenceFailed {
		return state.applyReferenceEvent(eventType, payload)
	}
	switch eventType {
	case agent.EventDecisionStarted:
		if payload.TaskID != "" {
			state.TaskID = payload.TaskID
		}
		if payload.Profile != "" {
			state.Profile = payload.Profile
		}
	case agent.EventRequestContractCommitted:
		state.TaskID, state.ContractRef = payload.TaskID, payload.ContractRef
		state.ContractRevision, state.ContractArtifact = payload.ContractRevision, payload.ContractArtifact
	case agent.EventDecisionOptionsProposed:
		if len(payload.Options) > 0 {
			state.ProposedOptions = payload.Options
		}
	case agent.EventDecisionEvidenceSealed:
		return state.applySealedEvidence(payload)
	case agent.EventDecisionOpinionSubmitted, agent.EventDecisionOpinionRejected:
		if payload.Opinion != nil {
			state.Opinions = append(state.Opinions, *payload.Opinion)
		}
	case agent.EventDecisionAggregateComputed:
		if payload.Aggregate != nil {
			state.Aggregates[payload.Aggregate.Round] = *payload.Aggregate
		}
	case agent.EventDecisionChallengeSubmitted:
		if payload.Challenge != nil {
			state.Challenges = append(state.Challenges, *payload.Challenge)
		}
	case agent.EventDecisionChallengeSkipped:
		state.ChallengeSkipReason = payload.Reason
		state.ChallengeSkipEvidenceHash = payload.EvidenceHash
	case agent.EventDecisionRevisionSubmitted:
		if payload.Revision != nil {
			state.Revisions = append(state.Revisions, *payload.Revision)
		}
	case agent.EventDecisionPremortemSubmitted:
		if payload.Premortem != nil {
			premortem := *payload.Premortem
			state.Premortem = &premortem
		}
	case agent.EventDecisionBudgetDegraded:
		if payload.Degradation != nil {
			state.Degradations = append(state.Degradations, *payload.Degradation)
		}
	case agent.EventDecisionFinalized:
		return state.applyFinalizedRecord(payload)
	case agent.EventDecisionFinalizationResult:
		return state.applyFinalizationResult(payload)
	case agent.EventAssumptionSupported, agent.EventAssumptionContradicted, agent.EventAssumptionStale:
		state.applyAssumptionTransition(payload)
	case agent.EventDecisionInvalidated:
		state.applyInvalidation(payload)
	}
	return nil
}

func (state *decisionState) applySealedEvidence(payload decisionEvent) error {
	if payload.Packet == nil {
		return nil
	}
	if state.Packet.Hash != "" && state.Packet.Hash != payload.Packet.Hash {
		state.StaleHashes[state.Packet.Hash] = true
	}
	state.Packet, state.EvidenceArtifact = *payload.Packet, payload.EvidenceArtifact
	return nil
}

func (state *decisionState) applyFinalizedRecord(payload decisionEvent) error {
	if payload.Record == nil {
		return nil
	}
	record := *payload.Record
	if err := record.ValidateSchemaVersion(); err != nil {
		return err
	}
	switch record.SchemaVersion {
	case 1:
		// Explicit schema-v1 records are the only finalized records that may
		// use the pre-result-event compatibility path.
	case DecisionRecordSchemaVersion:
		if state.Finalization == nil {
			return fmt.Errorf("decision_finalized has no preceding finalization result")
		}
		if !finalizationResultRefValid(payload.RecordRef) {
			return fmt.Errorf("decision_finalized has no canonical record artifact reference")
		}
		if record.FinalOption != state.Finalization.OptionID ||
			record.FinalizationMode != state.Finalization.Mode ||
			record.FinalizationIdentity != state.Finalization.Identity ||
			record.FinalizationReason != state.Finalization.Reason ||
			record.FinalizationOutcome != state.Finalization.Outcome ||
			record.FinalizationStale != state.Finalization.Stale ||
			!sameDecisionStrings(record.FinalizationWarnings, state.Finalization.Warnings) ||
			!sameArtifactIdentity(derefArtifact(record.FinalizationResultRef), state.FinalizationResultRef) {
			return fmt.Errorf("decision_finalized does not match its finalization result")
		}
	default:
		return fmt.Errorf("decision_finalized has unsupported record schema version %d", record.SchemaVersion)
	}
	if state.Record != nil {
		return fmt.Errorf("decision has more than one finalized record")
	}
	canonical := record
	state.Record = &record
	state.CanonicalRecord = &canonical
	state.FinalizedRecordRef = payload.RecordRef
	state.FinalizationQuestion = payload.Question
	state.FinalizationForecastRequired = payload.ForecastRequired
	return nil
}

func (state *decisionState) applyFinalizationResult(payload decisionEvent) error {
	if payload.Finalization == nil {
		return fmt.Errorf("decision finalization result is missing")
	}
	if payload.Finalization.DecisionID != state.DecisionID || payload.EvidenceHash != payload.Finalization.EvidenceHash {
		return fmt.Errorf("decision finalization result does not match event identity")
	}
	if state.Packet.Hash == "" {
		return fmt.Errorf("decision finalization result has no sealed evidence")
	}
	if err := ValidateFinalizationResult(*payload.Finalization, state.Packet, aggregateForFinalization(state), effectiveDecisionPolicyForState(state)); err != nil {
		return err
	}
	if !finalizationResultRefValid(payload.FinalizationResultRef) {
		return fmt.Errorf("decision finalization result artifact reference is incomplete")
	}
	if state.Finalization != nil {
		if state.Finalization.OptionID != payload.Finalization.OptionID || state.Finalization.EvidenceHash != payload.Finalization.EvidenceHash ||
			!sameArtifactRef(state.FinalizationResultRef, payload.FinalizationResultRef) {
			return fmt.Errorf("decision finalization result conflicts with an earlier result")
		}
		return nil
	}
	result := *payload.Finalization
	state.Finalization = &result
	state.FinalizationResultRef = payload.FinalizationResultRef
	return nil
}

func (state *decisionState) applyAssumptionTransition(payload decisionEvent) {
	if state.Record == nil || payload.AssumptionID == "" {
		return
	}
	assumptions, _, err := ApplyAssumptionTransition(state.Record.Assumptions, AssumptionTransition{
		AssumptionID: payload.AssumptionID, To: payload.To, Source: payload.Source,
		EvidenceRefs: payload.EvidenceRefs, At: payload.At,
	})
	if err == nil {
		state.Record.Assumptions = assumptions
	}
}

func (state *decisionState) applyInvalidation(payload decisionEvent) {
	state.Invalidated = true
	if state.Record != nil {
		state.Record.Stale = true
		if state.Record.StaleReason == "" {
			state.Record.StaleReason = payload.Reason
		}
	}
}

func derefArtifact(ref *ArtifactRef) ArtifactRef {
	if ref == nil {
		return ArtifactRef{}
	}
	return *ref
}

func sameDecisionStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for idx := range left {
		if left[idx] != right[idx] {
			return false
		}
	}
	return true
}

func aggregateForFinalization(state *decisionState) DecisionAggregate {
	if state == nil || len(state.Aggregates) == 0 {
		return DecisionAggregate{}
	}
	if aggregate, ok := state.Aggregates[2]; ok && aggregate.EvidenceHash == state.Packet.Hash {
		return aggregate
	}
	return state.Aggregates[1]
}

func effectiveDecisionPolicyForState(state *decisionState) DecisionPolicy {
	// The reducer validates all identity and evidence fields. Policy is loaded
	// from the immutable run envelope by the engine; this zero-policy fallback
	// keeps legacy event projection readable without inventing configuration.
	if state == nil {
		return DecisionPolicy{}
	}
	return DecisionPolicy{Finalization: FinalizationPolicy{Mode: state.FinalizationMode, JudgeID: state.FinalizationJudgeID}}
}

func (state *decisionState) applyEnvelopeAnchor(payload decisionEvent) error {
	if payload.RunID == "" || payload.TaskID == "" || payload.Attempt < 1 || payload.EvidenceHash == "" ||
		payload.EnvelopeRef.ID == "" || payload.EnvelopeRef.SHA256 == "" || payload.EnvelopeHash == "" {
		return fmt.Errorf("decision run envelope anchor is incomplete")
	}
	if state.Packet.Hash == "" || payload.EvidenceHash != state.Packet.Hash {
		return fmt.Errorf("decision run envelope anchor evidence identity does not match sealed evidence")
	}
	if payload.EnvelopeHash != payload.EnvelopeRef.SHA256 {
		return fmt.Errorf("decision run envelope anchor hash does not match artifact reference")
	}
	if payload.EnvelopeRef.RunID != payload.RunID || payload.EnvelopeRef.TaskID != payload.TaskID || payload.EnvelopeRef.Attempt != payload.Attempt {
		return fmt.Errorf("decision run envelope anchor artifact identity does not match")
	}
	if state.EnvelopeRef.ID != "" {
		if state.EnvelopeRef.ID != payload.EnvelopeRef.ID || state.EnvelopeRef.SHA256 != payload.EnvelopeRef.SHA256 {
			return fmt.Errorf("decision run envelope has conflicting anchors")
		}
		return nil
	}
	state.EnvelopeRef = payload.EnvelopeRef
	state.FinalizationMode = payload.FinalizationMode
	state.FinalizationJudgeID = payload.FinalizationJudgeID
	return nil
}

func (state *decisionState) applyEventIdentity(event RunEvent, payload decisionEvent) error {
	if payload.DecisionID == "" || payload.DecisionID != state.DecisionID {
		return fmt.Errorf("decision event identity does not match decision projection")
	}
	if event.RunID != "" && payload.RunID != "" && event.RunID != payload.RunID {
		return fmt.Errorf("decision event run identity does not match payload")
	}
	if event.TaskID != "" && payload.TaskID != "" && event.TaskID != payload.TaskID {
		return fmt.Errorf("decision event task identity does not match payload")
	}
	if event.Attempt > 0 && payload.Attempt > 0 && event.Attempt != payload.Attempt {
		return fmt.Errorf("decision event attempt identity does not match payload")
	}
	if payload.RunID != "" && state.RunID != "" && payload.RunID != state.RunID {
		return fmt.Errorf("decision event run identity conflicts with prior event")
	}
	if event.RunID != "" && state.RunID != "" && event.RunID != state.RunID {
		return fmt.Errorf("decision event outer run identity conflicts with prior event")
	}
	if payload.TaskID != "" && state.TaskID != "" && payload.TaskID != state.TaskID {
		return fmt.Errorf("decision event task identity conflicts with prior event")
	}
	if event.TaskID != "" && state.TaskID != "" && event.TaskID != state.TaskID {
		return fmt.Errorf("decision event outer task identity conflicts with prior event")
	}
	if payload.Attempt > 0 && state.Attempt > 0 && payload.Attempt != state.Attempt {
		return fmt.Errorf("decision event attempt identity conflicts with prior event")
	}
	if event.Attempt > 0 && state.Attempt > 0 && event.Attempt != state.Attempt {
		return fmt.Errorf("decision event outer attempt identity conflicts with prior event")
	}
	if state.RunID == "" {
		state.RunID = payload.RunID
		if state.RunID == "" {
			state.RunID = event.RunID
		}
	}
	if state.TaskID == "" {
		state.TaskID = payload.TaskID
		if state.TaskID == "" {
			state.TaskID = event.TaskID
		}
	}
	if state.Attempt == 0 {
		state.Attempt = payload.Attempt
		if state.Attempt == 0 {
			state.Attempt = event.Attempt
		}
	}
	return nil
}

func (state *decisionState) applyReferenceEvent(eventType string, payload decisionEvent) error {
	if state == nil {
		return fmt.Errorf("reference event state is nil")
	}
	switch eventType {
	case agent.EventDecisionReferenceStarted:
		if payload.ReferenceInvocation == nil {
			return fmt.Errorf("reference start has no invocation")
		}
		if payload.ReferenceResult != nil || payload.ReferenceFailure != nil {
			return fmt.Errorf("reference start contains terminal data")
		}
		if state.ReferenceInvocation != nil || state.ReferenceResult != nil || state.ReferenceFailure != nil {
			return fmt.Errorf("reference invocation was already started or terminated")
		}
		invocation := *payload.ReferenceInvocation
		if err := validateReferenceInvocation(invocation); err != nil {
			return err
		}
		if invocation.DecisionID != state.DecisionID || payload.DecisionID != state.DecisionID {
			return fmt.Errorf("reference start decision identity does not match")
		}
		if payload.TaskID != "" && invocation.TaskID != payload.TaskID {
			return fmt.Errorf("reference start task identity does not match")
		}
		state.ReferenceInvocation = &invocation
	case agent.EventDecisionReferenceCompleted:
		if payload.ReferenceResult == nil {
			return fmt.Errorf("reference completion has no result")
		}
		if payload.ReferenceInvocation != nil || payload.ReferenceFailure != nil {
			return fmt.Errorf("reference completion contains contradictory terminal data")
		}
		if state.ReferenceInvocation == nil {
			return fmt.Errorf("reference completion precedes start")
		}
		if state.ReferenceResult != nil || state.ReferenceFailure != nil {
			return fmt.Errorf("reference invocation already has a terminal event")
		}
		result := *payload.ReferenceResult
		if err := validateReferenceEvidenceResult(result); err != nil {
			return err
		}
		if result.InvocationID != state.ReferenceInvocation.InvocationID || result.InputHash != state.ReferenceInvocation.InputHash {
			return fmt.Errorf("reference completion identity does not match invocation")
		}
		if payload.DecisionID != state.DecisionID {
			return fmt.Errorf("reference completion decision identity does not match")
		}
		result.BaseRates = cloneBaseRateEvidence(result.BaseRates)
		result.Artifacts = append([]ArtifactRef(nil), result.Artifacts...)
		result.Provenance = cloneEvidenceProvenance(result.Provenance)
		if result.ResultArtifactRef != nil {
			ref := *result.ResultArtifactRef
			result.ResultArtifactRef = &ref
		}
		state.ReferenceResult = &result
		state.ReferenceEvidenceResultRef = *result.ResultArtifactRef
	case agent.EventDecisionReferenceFailed:
		if payload.ReferenceFailure == nil {
			return fmt.Errorf("reference failure has no failure record")
		}
		if payload.ReferenceInvocation != nil || payload.ReferenceResult != nil {
			return fmt.Errorf("reference failure contains contradictory terminal data")
		}
		if state.ReferenceInvocation == nil {
			return fmt.Errorf("reference failure precedes start")
		}
		if state.ReferenceResult != nil || state.ReferenceFailure != nil {
			return fmt.Errorf("reference invocation already has a terminal event")
		}
		failure := *payload.ReferenceFailure
		if err := validateReferenceEvidenceFailure(failure); err != nil {
			return err
		}
		if failure.InvocationID != state.ReferenceInvocation.InvocationID || failure.InputHash != state.ReferenceInvocation.InputHash {
			return fmt.Errorf("reference failure identity does not match invocation")
		}
		if payload.DecisionID != state.DecisionID {
			return fmt.Errorf("reference failure decision identity does not match")
		}
		state.ReferenceFailure = &failure
	default:
		return fmt.Errorf("unsupported reference event type %q", eventType)
	}
	return nil
}

func validateReferenceInvocation(invocation ReferenceEvidenceInvocation) error {
	if invocation.SchemaVersion != ReferenceEvidenceSchemaVersion {
		return fmt.Errorf("unsupported reference invocation schema version %d", invocation.SchemaVersion)
	}
	if invocation.InvocationID == "" || invocation.InputHash == "" || invocation.DecisionID == "" {
		return fmt.Errorf("reference invocation identity is incomplete")
	}
	if invocation.Question == "" {
		return fmt.Errorf("reference invocation question is empty")
	}
	return nil
}

func validateReferenceEvidenceFailure(failure ReferenceEvidenceFailure) error {
	if failure.SchemaVersion != ReferenceEvidenceSchemaVersion {
		return fmt.Errorf("unsupported reference failure schema version %d", failure.SchemaVersion)
	}
	if failure.InvocationID == "" || failure.InputHash == "" {
		return fmt.Errorf("reference failure identity is incomplete")
	}
	if failure.Reason == "" {
		return fmt.Errorf("reference failure reason is empty")
	}
	return nil
}

// persistDecisionRecord writes the finished record to the artifact store so the
// decision survives as content-addressed evidence, not only as an event body.
func persistDecisionRecord(ctx context.Context, store ArtifactStore, record DecisionRecord) (ArtifactRef, error) {
	if store == nil {
		return ArtifactRef{}, nil
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("encoding decision record: %w", err)
	}
	result, err := store.Put(ctx, PutArtifactRequest{
		Content:     data,
		Path:        fmt.Sprintf("decisions/%s.json", record.ID),
		Kind:        "decision_record",
		Role:        "decision",
		Description: fmt.Sprintf("decision record %s", record.ID),
		MediaType:   "application/json",
		RunID:       record.RunID,
		TaskID:      record.TaskID,
	})
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("persisting decision record: %w", err)
	}
	return result.ArtifactRef, nil
}
