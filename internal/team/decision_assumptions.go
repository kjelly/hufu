package team

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Assumption status sources (docs/hufu-decision-aware-runtime-spec.md §18.1).
//
// The runtime never infers an assumption's status. Exactly three things may
// change one, and each has its own contract here:
//
//  1. a worker reporting a check in submit_result;
//  2. a verification the task declared as checking specific assumptions;
//  3. an operator command.
//
// Until one of them speaks, an assumption stays `unknown`, which deliberately
// does not block anything: gating on unchecked assumptions would deadlock every
// decision that declares one (§18.2).

// AssumptionCheck is one reported assumption check.
type AssumptionCheck struct {
	AssumptionID string   `json:"assumption_id"`
	Status       string   `json:"status"`
	Evidence     []string `json:"evidence,omitempty"`
	Note         string   `json:"note,omitempty"`
}

// ValidCheckStatus reports whether a reporter may assert this status.
//
// Only supported and contradicted are reportable. `unknown` is the absence of a
// check, not a finding, and `stale` is the runtime's own conclusion that an
// earlier check no longer applies — neither is something a worker or a
// verification can claim.
func ValidCheckStatus(status string) bool {
	switch status {
	case AssumptionSupported, AssumptionContradicted:
		return true
	}
	return false
}

// ApplyAssumptionChecks records reported checks against a task's armed
// decision. It returns the number of transitions applied.
//
// A check naming an assumption the decision never declared is rejected: a
// reporter may report on the assumptions the decision rests on, not invent new
// ones after the fact.
func (c *Coordinator) ApplyAssumptionChecks(ctx context.Context, todoID string, checks []AssumptionCheck, source string) (int, error) {
	if c == nil || len(checks) == 0 {
		return 0, nil
	}
	discipline := c.disciplineFor(todoID)
	if discipline == nil || discipline.decisionID == "" {
		// No decision governs this task, so there is nothing to check against.
		return 0, nil
	}
	if !ValidAssumptionSource(source) {
		return 0, fmt.Errorf("%q is not a source that may change assumption status", source)
	}
	discipline.mu.Lock()
	stale := discipline.staleMarked
	assumptions := append([]DecisionAssumption(nil), discipline.assumptions...)
	discipline.mu.Unlock()
	if stale {
		return 0, fmt.Errorf("decision %s is stale and cannot accept assumption checks", discipline.decisionID)
	}
	journal := c.decisionJournalOrNil()
	if journal == nil {
		return 0, nil
	}

	// Validate the entire batch before persisting anything. A malformed later
	// claim must not leave an earlier transition durable.
	ordered := append([]AssumptionCheck(nil), checks...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].AssumptionID < ordered[j].AssumptionID })
	seen := make(map[string]struct{}, len(ordered))
	for _, check := range ordered {
		id := strings.TrimSpace(check.AssumptionID)
		if id == "" {
			return 0, fmt.Errorf("assumption id cannot be blank")
		}
		if _, exists := seen[id]; exists {
			return 0, fmt.Errorf("assumption %s is reported more than once", id)
		}
		seen[id] = struct{}{}
		if !ValidCheckStatus(check.Status) {
			return 0, fmt.Errorf("assumption %s: %q is not a reportable status (want supported or contradicted)", id, check.Status)
		}
		declared := false
		for _, assumption := range assumptions {
			if assumption.ID == id {
				declared = true
				break
			}
		}
		if !declared {
			return 0, fmt.Errorf("assumption %q is not declared on this decision", id)
		}
		for _, digest := range check.Evidence {
			if strings.TrimSpace(digest) == "" {
				return 0, fmt.Errorf("assumption %s has a blank evidence reference", id)
			}
		}
	}
	for _, check := range ordered {
		if len(check.Evidence) > 0 {
			if err := c.validateAssumptionEvidence(ctx, ordered); err != nil {
				return 0, err
			}
			break
		}
	}

	applied := 0
	for _, check := range ordered {
		id := strings.TrimSpace(check.AssumptionID)
		discipline.mu.Lock()
		current := append([]DecisionAssumption(nil), discipline.assumptions...)
		discipline.mu.Unlock()

		previous := AssumptionUnknown
		for _, assumption := range current {
			if assumption.ID == id {
				previous = assumption.EffectiveStatus()
			}
		}
		if previous == check.Status {
			// Re-reporting the same status is not a transition.
			continue
		}

		updated, _, err := RecordAssumptionTransition(ctx, journal, current, AssumptionTransition{
			DecisionID:   discipline.decisionID,
			AssumptionID: id,
			From:         previous,
			To:           check.Status,
			Source:       source,
			EvidenceRefs: assumptionEvidenceRefs(check.Evidence),
			At:           c.disciplineNow(),
		})
		if err != nil {
			return applied, fmt.Errorf("recording assumption check: %w", err)
		}

		discipline.mu.Lock()
		discipline.assumptions = updated
		critical := CriticalContradiction(updated)
		alreadyStopped := discipline.stopped
		if critical != "" && !alreadyStopped {
			discipline.stopped = true
		}
		discipline.mu.Unlock()
		if critical != "" && !alreadyStopped {
			c.actOnCheckpoint(ctx, discipline, CheckpointDecision{
				Action: CheckpointReplan, Reason: ReasonAssumptionInvalidated,
				Detail: fmt.Sprintf("critical assumption %s was contradicted", critical),
			})
		}
		applied++
	}
	return applied, nil
}

func (c *Coordinator) validateAssumptionEvidence(ctx context.Context, checks []AssumptionCheck) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("assumption evidence cannot be validated without a workspace")
	}
	refs, err := WorkspaceArtifacts(c.session.Workspace)
	if err != nil {
		return fmt.Errorf("resolve assumption evidence: %w", err)
	}
	byDigest := NewArtifactResolver(refs)
	store := c.decisionArtifactStore()
	for _, check := range checks {
		for _, digest := range check.Evidence {
			ref, ok := byDigest.ResolveDigest(digest)
			if !ok || store == nil {
				return fmt.Errorf("assumption %s evidence %q is not resolvable in the workspace artifact store", check.AssumptionID, digest)
			}
			if err := store.Verify(ctx, ref); err != nil {
				return fmt.Errorf("assumption %s evidence %q failed verification: %w", check.AssumptionID, digest, err)
			}
		}
	}
	return nil
}

// assumptionEvidenceRefs turns reported digests into artifact references.
func assumptionEvidenceRefs(digests []string) []ArtifactRef {
	refs := make([]ArtifactRef, 0, len(digests))
	for _, digest := range digests {
		if trimmed := strings.TrimSpace(digest); trimmed != "" {
			refs = append(refs, ArtifactRef{SHA256: trimmed})
		}
	}
	return refs
}

// AssumptionChecksFromVerification derives assumption checks from a
// verification the task declared as checking them (§18.1 source 2).
//
// This is a deterministic second source, independent of what a worker says
// about its own work: the task contract names which assumptions a verification
// covers, and the verification's own pass or fail decides their status.
func AssumptionChecksFromVerification(spec *VerificationSpec, passed bool) []AssumptionCheck {
	if spec == nil || len(spec.AssumptionRefs) == 0 {
		return nil
	}
	status := AssumptionContradicted
	if passed {
		status = AssumptionSupported
	}

	checks := make([]AssumptionCheck, 0, len(spec.AssumptionRefs))
	for _, id := range spec.AssumptionRefs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			checks = append(checks, AssumptionCheck{
				AssumptionID: trimmed,
				Status:       status,
				Note:         fmt.Sprintf("declared verification %s", verificationLabel(spec, passed)),
			})
		}
	}
	return checks
}

// verificationLabel describes a verification outcome for an event reason.
func verificationLabel(spec *VerificationSpec, passed bool) string {
	kind := string(spec.Type)
	if kind == "" {
		kind = "verification"
	}
	if passed {
		return kind + " passed"
	}
	return kind + " failed"
}

// disciplineTodoIDFrom resolves the todo a discipline was armed under. The
// task context carries it; a task contract's own ID is the fallback for paths
// that run outside a task context.
func disciplineTodoIDFrom(ctx context.Context, task TaskDef) string {
	if todoID, _ := ctx.Value(todoIDKey{}).(string); strings.TrimSpace(todoID) != "" {
		return todoID
	}
	return strings.TrimSpace(task.ID)
}
