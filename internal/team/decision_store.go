package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

// FetchDecisionArtifact reads and JSON-decodes a content-addressed artifact
// this package persisted (a decision record or a reference evidence result),
// verifying its digest before decoding. It is exported for read-only
// inspection tooling (hufu decision explain) that has no other durable path
// to a specific stage's detail beyond the cross-run index summary.
func FetchDecisionArtifact[T any](ctx context.Context, store ArtifactStore, ref ArtifactRef) (T, error) {
	var out T
	if store == nil || ref.ID == "" {
		return out, fmt.Errorf("artifact reference is unavailable")
	}
	resolved, err := store.Resolve(ctx, ref)
	if err != nil {
		return out, fmt.Errorf("resolve artifact %s: %w", ref.ID, err)
	}
	if err := store.Verify(ctx, resolved); err != nil {
		return out, fmt.Errorf("verify artifact %s: %w", ref.ID, err)
	}
	reader, err := store.Open(ctx, resolved.ID)
	if err != nil {
		return out, fmt.Errorf("open artifact %s: %w", ref.ID, err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		return out, fmt.Errorf("read artifact %s: %w", ref.ID, err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("decode artifact %s: %w", ref.ID, err)
	}
	return out, nil
}
