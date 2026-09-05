package team

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

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
	Profile      string `json:"profile,omitempty"`
	EvidenceHash string `json:"evidence_hash,omitempty"`
	Round        int    `json:"round,omitempty"`
	JudgeID      string `json:"judge_id,omitempty"`
	Reason       string `json:"reason,omitempty"`

	Packet      *DecisionEvidencePacket `json:"packet,omitempty"`
	Opinion     *DecisionOpinion        `json:"opinion,omitempty"`
	Aggregate   *DecisionAggregate      `json:"aggregate,omitempty"`
	Record      *DecisionRecord         `json:"record,omitempty"`
	Degradation *DecisionDegradation    `json:"degradation,omitempty"`
}

// decisionState is the projection rebuilt from the event log.
type decisionState struct {
	DecisionID   string
	Profile      string
	Packet       DecisionEvidencePacket
	Opinions     []DecisionOpinion
	Aggregates   map[int]DecisionAggregate
	Degradations []DecisionDegradation
	Record       *DecisionRecord
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
			state.Profile = payload.Profile
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
		case agent.EventDecisionBudgetDegraded:
			if payload.Degradation != nil {
				state.Degradations = append(state.Degradations, *payload.Degradation)
			}
		case agent.EventDecisionFinalized:
			if payload.Record != nil {
				record := *payload.Record
				state.Record = &record
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
