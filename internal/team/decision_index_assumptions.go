package team

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// The operator assumption-check entry point, the third of the three status
// sources (docs/architecture/decision-runtime.md §18.1).
//
// It appends a superseding row; the DecisionRecord the row points at is never
// edited. Split out of decision_index.go unchanged.
func (i *DecisionIndex) CheckAssumption(decisionID, assumptionID, status, note string) (DecisionIndexEntry, DecisionAssumption, error) {
	var entry DecisionIndexEntry
	var assumption DecisionAssumption
	err := i.withExclusiveLock(func() error {
		var err error
		entry, assumption, err = i.checkAssumptionUnlocked(decisionID, assumptionID, status, note)
		return err
	})
	return entry, assumption, err
}

func (i *DecisionIndex) checkAssumptionUnlocked(decisionID, assumptionID, status, note string) (DecisionIndexEntry, DecisionAssumption, error) {
	if strings.TrimSpace(decisionID) == "" || strings.TrimSpace(assumptionID) == "" {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("decision and assumption IDs cannot be blank")
	}
	entries, err := i.listFileValidated(context.Background())
	if err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	var entry DecisionIndexEntry
	found := false
	for _, candidate := range entries {
		if candidate.DecisionID == decisionID {
			entry, found = candidate, true
			break
		}
	}
	if !found {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("decision %q is not in the index at %s", decisionID, i.path)
	}
	if entry.Stale {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("decision %q is stale and cannot accept assumption checks", decisionID)
	}
	if !ValidCheckStatus(status) {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf(
			"%q is not a reportable assumption status (want supported or contradicted)", status)
	}
	if len(entry.Assumptions) == 0 {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("decision %q declares no assumptions", decisionID)
	}
	if previous := assumptionStatusBefore(entry.Assumptions, assumptionID); previous == status {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("assumption %q already has status %q", assumptionID, status)
	}

	at := time.Now().UTC()
	updated, assumption, err := ApplyAssumptionTransition(entry.Assumptions, AssumptionTransition{
		DecisionID:   decisionID,
		AssumptionID: assumptionID,
		To:           status,
		Source:       AssumptionSourceOperator,
		At:           at,
	})
	if err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	journal := i.journalSnapshot()
	if journal == nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, fmt.Errorf("decision index: canonical event journal is unavailable")
	}
	transition := AssumptionTransition{DecisionID: decisionID, AssumptionID: assumptionID,
		From: assumptionStatusBefore(entry.Assumptions, assumptionID), To: status,
		Source: AssumptionSourceOperator, At: at}
	if err := appendDecisionEvent(context.Background(), journal, AssumptionStatusEvent(status), decisionEvent{
		DecisionID: decisionID, AssumptionID: transition.AssumptionID, From: transition.From,
		To: transition.To, Source: transition.Source, Note: note, At: transition.At,
		IdempotencyKey: assumptionTransitionEventKey(transition),
		Reason:         fmt.Sprintf("operator assumption check: %s", strings.TrimSpace(note)),
	}); err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	entry.Assumptions = updated
	entry.AssumptionNotes = appendAssumptionNote(entry.AssumptionNotes, assumptionID, status, note)
	if status == AssumptionContradicted && assumption.Critical {
		reason := fmt.Sprintf("%s: critical assumption %s contradicted", ReasonAssumptionInvalidated, assumptionID)
		if err := appendDecisionEvent(context.Background(), journal, agent.EventDecisionInvalidated, decisionEvent{
			DecisionID:     decisionID,
			Reason:         reason,
			IdempotencyKey: decisionStageEventKey(decisionID, "invalidated", reason),
		}); err != nil {
			return DecisionIndexEntry{}, DecisionAssumption{}, err
		}
		if err := RequestReplan(context.Background(), journal, decisionID, CheckpointDecision{
			Action: CheckpointReplan, Reason: ReasonAssumptionInvalidated, Detail: reason,
		}); err != nil {
			return DecisionIndexEntry{}, DecisionAssumption{}, err
		}
		entry.Stale = true
		entry.StaleReason = reason
	}
	entry.IndexedAt = time.Time{}
	if err := i.appendEntryUnlocked(entry); err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	if err := persistDecisionIndexSessionProjection(i.path, entry); err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	if err := recordDecisionIndexTaskProjection(i.path, entry); err != nil {
		return DecisionIndexEntry{}, DecisionAssumption{}, err
	}
	return entry, assumption, nil
}

// persistDecisionIndexSessionProjection keeps the restart-facing session
// projection aligned with operator changes. Missing session.json is normal for
// a workspace addressed before its first coordinator run.
func persistDecisionIndexSessionProjection(indexPath string, entry DecisionIndexEntry) error {
	workspace := filepath.Dir(filepath.Dir(filepath.Dir(indexPath)))
	session, err := loadSessionQuiet(workspace)
	if err != nil {
		return fmt.Errorf("load session for decision projection: %w", err)
	}
	if session == nil {
		return nil
	}
	for n := range session.DecisionProjections {
		if session.DecisionProjections[n].DecisionID == entry.DecisionID {
			session.DecisionProjections[n] = entry
			return SaveSession(workspace, session)
		}
	}
	session.DecisionProjections = append(session.DecisionProjections, entry)
	return SaveSession(workspace, session)
}

func assumptionStatusBefore(assumptions []DecisionAssumption, id string) string {
	for _, assumption := range assumptions {
		if assumption.ID == id {
			return assumption.EffectiveStatus()
		}
	}
	return AssumptionUnknown
}

// CriticalAssumptionContradicted returns the ID of a contradicted critical
// assumption on this decision, or an empty string.
func (e DecisionIndexEntry) CriticalAssumptionContradicted() string {
	return CriticalContradiction(e.Assumptions)
}

// appendAssumptionNote records one operator transition in a readable line.
func appendAssumptionNote(notes []string, assumptionID, status, note string) []string {
	line := fmt.Sprintf("%s: %s -> %s", time.Now().UTC().Format(time.RFC3339), assumptionID, status)
	if trimmed := strings.TrimSpace(note); trimmed != "" {
		line += " (" + trimmed + ")"
	}
	return append(notes, line)
}
