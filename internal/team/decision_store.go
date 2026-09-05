package team

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
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
	TaskID       string `json:"task_id,omitempty"`
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

	Packet           *DecisionEvidencePacket `json:"packet,omitempty"`
	Opinion          *DecisionOpinion        `json:"opinion,omitempty"`
	Aggregate        *DecisionAggregate      `json:"aggregate,omitempty"`
	Challenge        *DecisionChallenge      `json:"challenge,omitempty"`
	Revision         *DecisionRevision       `json:"revision,omitempty"`
	Premortem        *PremortemResult        `json:"premortem,omitempty"`
	Options          []DecisionOption        `json:"options,omitempty"`
	Record           *DecisionRecord         `json:"record,omitempty"`
	Question         string                  `json:"question,omitempty"`
	ForecastRequired bool                    `json:"forecast_required,omitempty"`
	RecordRef        ArtifactRef             `json:"record_ref,omitempty"`
	ContractRef      string                  `json:"contract_ref,omitempty"`
	ContractRevision uint64                  `json:"contract_revision,omitempty"`
	ContractArtifact ArtifactRef             `json:"contract_artifact,omitempty"`
	Degradation      *DecisionDegradation    `json:"degradation,omitempty"`

	// JudgeAliases records the anonymization mapping a challenger was NOT
	// given, so the run stays auditable without ever revealing identity to the
	// challenger itself (spec §23).
	JudgeAliases map[string]string `json:"judge_aliases,omitempty"`
}

// decisionState is the projection rebuilt from the event log.
type decisionState struct {
	DecisionID       string
	TaskID           string
	Profile          string
	Packet           DecisionEvidencePacket
	ProposedOptions  []DecisionOption
	Opinions         []DecisionOpinion
	Aggregates       map[int]DecisionAggregate
	Challenges       []DecisionChallenge
	Revisions        []DecisionRevision
	Premortem        *PremortemResult
	Degradations     []DecisionDegradation
	Record           *DecisionRecord
	ContractRef      string
	ContractRevision uint64
	ContractArtifact ArtifactRef
	Invalidated      bool
	// StaleHashes are evidence hashes superseded by a later seal. Opinions
	// formed on them are durable but must not be aggregated (spec §15.4).
	StaleHashes map[string]bool
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
	if journal == nil {
		return fmt.Errorf("decision event journal is unavailable")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding %s payload: %w", eventType, err)
	}
	if _, err := journal.Append(ctx, RunEvent{Type: eventType, Actor: decisionActor, Payload: raw}); err != nil {
		return fmt.Errorf("append %s event: %w", eventType, err)
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
		var payload decisionEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			// Foreign event types share the log; skip anything that is not a
			// decision payload rather than failing the whole projection.
			continue
		}
		if payload.DecisionID != decisionID {
			continue
		}
		switch event.Type {
		case agent.EventDecisionStarted:
			state.TaskID = payload.TaskID
			state.Profile = payload.Profile
		case agent.EventRequestContractCommitted:
			state.TaskID = payload.TaskID
			state.ContractRef = payload.ContractRef
			state.ContractRevision = payload.ContractRevision
			state.ContractArtifact = payload.ContractArtifact
		case agent.EventDecisionOptionsProposed:
			if len(payload.Options) > 0 {
				state.ProposedOptions = payload.Options
			}
		case agent.EventDecisionEvidenceSealed:
			if payload.Packet == nil {
				continue
			}
			if state.Packet.Hash != "" && state.Packet.Hash != payload.Packet.Hash {
				state.StaleHashes[state.Packet.Hash] = true
			}
			state.Packet = *payload.Packet
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
			if payload.Record != nil {
				record := *payload.Record
				state.Record = &record
			}
		case agent.EventAssumptionSupported, agent.EventAssumptionContradicted, agent.EventAssumptionStale:
			if state.Record == nil || payload.AssumptionID == "" {
				continue
			}
			assumptions, _, transitionErr := ApplyAssumptionTransition(state.Record.Assumptions, AssumptionTransition{
				AssumptionID: payload.AssumptionID, To: payload.To, Source: payload.Source,
				EvidenceRefs: payload.EvidenceRefs, At: payload.At,
			})
			if transitionErr == nil {
				state.Record.Assumptions = assumptions
			}
		case agent.EventDecisionInvalidated:
			state.Invalidated = true
			if state.Record != nil {
				state.Record.Stale = true
				if state.Record.StaleReason == "" {
					state.Record.StaleReason = payload.Reason
				}
			}
		}
	}
	return state, nil
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
