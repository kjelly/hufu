package context

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// ConsolidationFreshnessState summarizes whether a consolidation proposal and
// its sources are still current. Precedence is invalid > blocked > stale >
// fresh.
type ConsolidationFreshnessState string

const (
	ConsolidationFresh   ConsolidationFreshnessState = "fresh"
	ConsolidationStale   ConsolidationFreshnessState = "stale"
	ConsolidationBlocked ConsolidationFreshnessState = "blocked"
	ConsolidationInvalid ConsolidationFreshnessState = "invalid"
)

// ConsolidationReason is a fixed machine-readable reason code. Behaviour is
// driven only by these codes, never by message text.
type ConsolidationReason string

const (
	ReasonProposalDecodeFailed     ConsolidationReason = "proposal_decode_failed"
	ReasonSourceSetInvalid         ConsolidationReason = "source_set_invalid"
	ReasonCandidateMissing         ConsolidationReason = "candidate_missing"
	ReasonCandidateLinkMismatch    ConsolidationReason = "candidate_link_mismatch"
	ReasonCandidateShared          ConsolidationReason = "candidate_shared"
	ReasonLifecycleMismatch        ConsolidationReason = "lifecycle_mismatch"
	ReasonEdgeMismatch             ConsolidationReason = "edge_mismatch"
	ReasonOrphanCandidate          ConsolidationReason = "orphan_candidate"
	ReasonSourceConflictOpen       ConsolidationReason = "source_conflict_open"
	ReasonMarkedStale              ConsolidationReason = "marked_stale"
	ReasonCandidateSuperseded      ConsolidationReason = "candidate_superseded"
	ReasonSourceMissing            ConsolidationReason = "source_missing"
	ReasonSourceNotConfirmed       ConsolidationReason = "source_not_confirmed"
	ReasonSourceSuperseded         ConsolidationReason = "source_superseded"
	ReasonSourceExpired            ConsolidationReason = "source_expired"
	ReasonSourceOutsideValidity    ConsolidationReason = "source_outside_validity"
	ReasonSourceRevisionChanged    ConsolidationReason = "source_revision_changed"
	ReasonSourceScopeMismatch      ConsolidationReason = "source_scope_mismatch"
	ReasonSourceContradiction      ConsolidationReason = "source_contradiction"
	ReasonAggregateRevisionChanged ConsolidationReason = "aggregate_revision_changed"
	ReasonSupportInsufficient      ConsolidationReason = "support_insufficient"
)

var invalidConsolidationReasons = map[ConsolidationReason]bool{
	ReasonProposalDecodeFailed: true, ReasonSourceSetInvalid: true, ReasonCandidateMissing: true,
	ReasonCandidateLinkMismatch: true, ReasonCandidateShared: true, ReasonLifecycleMismatch: true,
	ReasonEdgeMismatch: true, ReasonOrphanCandidate: true,
}

// ConsolidationFreshness is the content-free result of checking one proposal.
type ConsolidationFreshness struct {
	ProposalID    string                           `json:"proposal_id"`
	Status        string                           `json:"status"`
	CandidateID   string                           `json:"candidate_id"`
	State         ConsolidationFreshnessState      `json:"state"`
	Reasons       []ConsolidationReason            `json:"reasons,omitempty"`
	SourceReasons map[string][]ConsolidationReason `json:"source_reasons,omitempty"`
}

// Err returns nil for a fresh proposal, ErrConsolidationInconsistent for an
// invalid one, and a *ConsolidationSourceError (or ErrConsolidationSourceInvalid)
// otherwise.
func (f ConsolidationFreshness) Err() error {
	switch f.State {
	case ConsolidationFresh:
		return nil
	case ConsolidationInvalid:
		return fmt.Errorf("%w: proposal %s (%s)", ErrConsolidationInconsistent, f.ProposalID, joinConsolidationReasons(f.Reasons))
	}
	if len(f.SourceReasons) > 0 {
		return &ConsolidationSourceError{SourceReasons: f.SourceReasons}
	}
	return fmt.Errorf("%w: proposal %s is %s (%s)", ErrConsolidationSourceInvalid, f.ProposalID, f.State, joinConsolidationReasons(f.Reasons))
}

// ConsolidationSourceError reports per-source reason codes. Its message
// describes the first failing source in ID order.
type ConsolidationSourceError struct {
	SourceReasons map[string][]ConsolidationReason
}

func (e *ConsolidationSourceError) Error() string {
	ids := make([]string, 0, len(e.SourceReasons))
	for id := range e.SourceReasons {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ErrConsolidationSourceInvalid.Error()
	}
	reasons := e.SourceReasons[ids[0]]
	return fmt.Sprintf("%s: %s (%s)", ErrConsolidationSourceInvalid, consolidationReasonText(ids[0], reasons[0]), joinConsolidationReasons(reasons))
}

func (e *ConsolidationSourceError) Unwrap() error { return ErrConsolidationSourceInvalid }

func consolidationReasonText(id string, reason ConsolidationReason) string {
	switch reason {
	case ReasonSourceMissing:
		return fmt.Sprintf("source %q was not found", id)
	case ReasonSourceNotConfirmed, ReasonSourceSuperseded, ReasonSourceExpired, ReasonSourceOutsideValidity:
		return fmt.Sprintf("source %q is not current confirmed knowledge", id)
	case ReasonSourceScopeMismatch:
		return fmt.Sprintf("source %q would widen or mix scope/kind", id)
	case ReasonSourceContradiction:
		return fmt.Sprintf("source %q contradicts another selected source; contradictory sources cannot be merged", id)
	case ReasonSourceConflictOpen:
		return fmt.Sprintf("source %q has an unresolved memory conflict", id)
	case ReasonSourceRevisionChanged:
		return fmt.Sprintf("source %q revision changed; create a new proposal", id)
	case ReasonAggregateRevisionChanged:
		return fmt.Sprintf("source %q aggregate revision changed; create a new proposal", id)
	case ReasonSupportInsufficient:
		return fmt.Sprintf("source %q lacks stable verified cross-task support", id)
	}
	return fmt.Sprintf("source %q is not eligible", id)
}

func joinConsolidationReasons(reasons []ConsolidationReason) string {
	parts := make([]string, len(reasons))
	for i, reason := range reasons {
		parts[i] = string(reason)
	}
	return strings.Join(parts, ",")
}

// ConsolidationSupportPolicy is the create-time evidence threshold for every
// source. The zero value disables the check; production callers pass the
// memory learning policy thresholds.
type ConsolidationSupportPolicy struct {
	MinConfirmedSupport int
	MinIndependentTasks int
}

func (p ConsolidationSupportPolicy) enabled() bool {
	return p.MinConfirmedSupport > 0 || p.MinIndependentTasks > 0
}

// ConsolidationSourceSelection names the sources of a new proposal and the
// policy their experience evidence is read under.
type ConsolidationSourceSelection struct {
	ProjectID     string
	TeamID        string
	SourceIDs     []string
	PolicyVersion string
	Support       ConsolidationSupportPolicy
}

type consolidationCheck int

const (
	// consolidationCheckCreate applies support thresholds to new sources.
	consolidationCheckCreate consolidationCheck = iota
	// consolidationCheckApprove also requires the frozen aggregate revisions.
	consolidationCheckApprove
	// consolidationCheckInspect compares frozen content revisions only.
	consolidationCheckInspect
)

type consolidationSourceCheck struct {
	projectID, teamID string
	ids               []string
	frozen            map[string]string
	aggregates        map[string]int64
	check             consolidationCheck
	policyVersion     string
	support           ConsolidationSupportPolicy
	now               time.Time
}

type consolidationSourceResult struct {
	sources            []ContextItem
	aggregateRevisions map[string]int64
	reasons            map[string][]ConsolidationReason
}

func (r *SQLiteRepository) checkConsolidationSourcesQ(ctx context.Context, q queryer, in consolidationSourceCheck) (consolidationSourceResult, error) {
	result := consolidationSourceResult{aggregateRevisions: map[string]int64{}, reasons: map[string][]ConsolidationReason{}}
	add := func(id string, reason ConsolidationReason) {
		if !slices.Contains(result.reasons[id], reason) {
			result.reasons[id] = append(result.reasons[id], reason)
		}
	}
	selected := make(map[string]bool, len(in.ids))
	for _, id := range in.ids {
		selected[id] = true
	}
	for _, id := range in.ids {
		item, err := getItemQ(ctx, q, id)
		if errors.Is(err, sql.ErrNoRows) {
			add(id, ReasonSourceMissing)
			continue
		}
		if err != nil {
			return result, fmt.Errorf("load consolidation source %q: %w", id, err)
		}
		result.sources = append(result.sources, item)
	}
	for _, item := range result.sources {
		for _, reason := range sourceItemReasons(item, result.sources[0], selected, in) {
			add(item.ID, reason)
		}
		reason, revision, found, err := sourceAggregateReason(ctx, q, item.ID, in)
		if err != nil {
			return result, err
		}
		if found {
			result.aggregateRevisions[item.ID] = revision
		}
		if reason != "" {
			add(item.ID, reason)
		}
	}
	byScope := map[[2]string][]string{}
	for _, item := range result.sources {
		key := [2]string{item.Scope.ProjectID, item.Scope.TeamID}
		byScope[key] = append(byScope[key], item.ID)
	}
	for key, ids := range byScope {
		conflicts, err := r.openConflictsForItemsQ(ctx, q, key[0], key[1], ids)
		if err != nil {
			return result, fmt.Errorf("check memory conflicts for consolidation: %w", err)
		}
		for id, found := range conflicts {
			if len(found) > 0 {
				add(id, ReasonSourceConflictOpen)
			}
		}
	}
	for id := range result.reasons {
		slices.Sort(result.reasons[id])
	}
	return result, nil
}

// sourceItemReasons checks one loaded source against lifecycle, validity,
// scope, contradiction, and frozen-revision rules.
func sourceItemReasons(item, first ContextItem, selected map[string]bool, in consolidationSourceCheck) []ConsolidationReason {
	var reasons []ConsolidationReason
	if item.Lifecycle != LifecycleConfirmed {
		reasons = append(reasons, ReasonSourceNotConfirmed)
	}
	if item.SupersededBy != "" {
		reasons = append(reasons, ReasonSourceSuperseded)
	}
	if item.ExpiresAt != nil && !in.now.Before(*item.ExpiresAt) {
		reasons = append(reasons, ReasonSourceExpired)
	}
	if (item.ValidFrom != nil && in.now.Before(*item.ValidFrom)) || (item.ValidUntil != nil && !in.now.Before(*item.ValidUntil)) {
		reasons = append(reasons, ReasonSourceOutsideValidity)
	}
	if item.Scope.ProjectID != in.projectID || item.Scope.TeamID != in.teamID || item.Scope != first.Scope || item.Kind != first.Kind {
		reasons = append(reasons, ReasonSourceScopeMismatch)
	}
	for _, contradiction := range strings.Split(item.Metadata["contradicts_ids"], ",") {
		if contradiction = strings.TrimSpace(contradiction); contradiction != "" && selected[contradiction] {
			reasons = append(reasons, ReasonSourceContradiction)
			break
		}
	}
	if in.frozen != nil && in.frozen[item.ID] != item.ContentHash {
		reasons = append(reasons, ReasonSourceRevisionChanged)
	}
	return reasons
}

// sourceAggregateReason reads a source's experience aggregate and applies the
// create-time support threshold or the approve-time revision equality.
func sourceAggregateReason(ctx context.Context, q queryer, id string, in consolidationSourceCheck) (ConsolidationReason, int64, bool, error) {
	aggregate, err := experienceAggregateQ(ctx, q, id, in.policyVersion)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, fmt.Errorf("load experience aggregate for %q: %w", id, err)
	}
	switch in.check {
	case consolidationCheckCreate:
		if in.support.enabled() && (!found || aggregate.VerifiedSupportCount < in.support.MinConfirmedSupport || aggregate.IndependentTaskCount < in.support.MinIndependentTasks || aggregate.CausalFailureCount > 0) {
			return ReasonSupportInsufficient, aggregate.Revision, found, nil
		}
	case consolidationCheckApprove:
		if in.aggregates[id] != aggregate.Revision {
			return ReasonAggregateRevisionChanged, aggregate.Revision, found, nil
		}
	}
	return "", aggregate.Revision, found, nil
}

// ValidateConsolidationSources applies the create-time source rules outside a
// transaction and returns the sources in sorted ID order with those IDs. The
// draft path uses it before a model call; CreateConsolidationProposal repeats
// the same check inside its transaction.
func (r *SQLiteRepository) ValidateConsolidationSources(ctx context.Context, selection ConsolidationSourceSelection) ([]ContextItem, []string, error) {
	ids := sortedUniqueIDs(selection.SourceIDs)
	if len(ids) < 2 {
		return nil, nil, errors.New("consolidation requires at least two source items")
	}
	result, err := r.checkConsolidationSourcesQ(ctx, r.db, consolidationSourceCheck{
		projectID: selection.ProjectID, teamID: selection.TeamID, ids: ids, check: consolidationCheckCreate,
		policyVersion: selection.PolicyVersion, support: selection.Support, now: time.Now(),
	})
	if err != nil {
		return nil, nil, err
	}
	if len(result.reasons) > 0 {
		return nil, nil, &ConsolidationSourceError{SourceReasons: result.reasons}
	}
	return result.sources, ids, nil
}

// EvaluateConsolidationProposal reports whether proposal, its candidate, and
// its sources are still current. checkAggregates also requires the frozen
// aggregate revisions, which is what approval and improve handoffs need.
func (r *SQLiteRepository) EvaluateConsolidationProposal(ctx context.Context, proposal ConsolidationProposal, policyVersion string, checkAggregates bool) (ConsolidationFreshness, error) {
	check := consolidationCheckInspect
	if checkAggregates {
		check = consolidationCheckApprove
	}
	return r.evaluateConsolidationQ(ctx, r.db, proposal, check, policyVersion, time.Now())
}

func (r *SQLiteRepository) evaluateConsolidationQ(ctx context.Context, q queryer, p ConsolidationProposal, check consolidationCheck, policyVersion string, now time.Time) (ConsolidationFreshness, error) {
	f := ConsolidationFreshness{ProposalID: p.ID, Status: p.Status, CandidateID: p.CandidateContextItemID}
	reasons := map[ConsolidationReason]bool{}
	ids := sortedUniqueIDs(p.SourceIDs)
	if len(ids) < 2 || len(ids) != len(p.SourceIDs) {
		reasons[ReasonSourceSetInvalid] = true
	}
	for _, id := range ids {
		if strings.TrimSpace(p.SourceRevisions[id]) == "" {
			reasons[ReasonSourceSetInvalid] = true
		}
	}
	candidate, err := getItemQ(ctx, q, p.CandidateContextItemID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		reasons[ReasonCandidateMissing] = true
	case err != nil:
		return f, fmt.Errorf("load consolidation candidate %q: %w", p.CandidateContextItemID, err)
	default:
		if candidate.Source.Type != SourceTypeConsolidationProposal || candidate.Source.Ref != p.ID || candidate.Scope.ProjectID != p.ProjectID || candidate.Scope.TeamID != p.TeamID {
			reasons[ReasonCandidateLinkMismatch] = true
		}
		var references int
		if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM consolidation_proposals WHERE candidate_context_item_id=?", candidate.ID).Scan(&references); err != nil {
			return f, fmt.Errorf("count consolidation proposals for candidate %q: %w", candidate.ID, err)
		}
		if references > 1 {
			reasons[ReasonCandidateShared] = true
		}
		switch p.Status {
		case ConsolidationStatusProposed, ConsolidationStatusStale:
			if candidate.Lifecycle != LifecycleCandidate {
				reasons[ReasonLifecycleMismatch] = true
			}
		case ConsolidationStatusApproved:
			if candidate.Lifecycle != LifecycleConfirmed {
				reasons[ReasonLifecycleMismatch] = true
			} else if candidate.SupersededBy != "" {
				reasons[ReasonCandidateSuperseded] = true
			}
		case ConsolidationStatusRejected:
			if candidate.Lifecycle != LifecycleRejected {
				reasons[ReasonLifecycleMismatch] = true
			}
		}
		edges, err := derivedFromEdgesQ(ctx, q, candidate.ID)
		if err != nil {
			return f, err
		}
		if !slices.Equal(edges, ids) {
			reasons[ReasonEdgeMismatch] = true
		}
	}
	if p.Status == ConsolidationStatusStale {
		reasons[ReasonMarkedStale] = true
	}
	active := p.Status == ConsolidationStatusProposed || p.Status == ConsolidationStatusApproved || p.Status == ConsolidationStatusStale
	if active && !reasons[ReasonSourceSetInvalid] {
		result, err := r.checkConsolidationSourcesQ(ctx, q, consolidationSourceCheck{
			projectID: p.ProjectID, teamID: p.TeamID, ids: ids, frozen: p.SourceRevisions, aggregates: p.AggregateRevisions,
			check: check, policyVersion: policyVersion, now: now,
		})
		if err != nil {
			return f, err
		}
		if len(result.reasons) > 0 {
			f.SourceReasons = result.reasons
		}
		for _, sourceReasons := range result.reasons {
			for _, reason := range sourceReasons {
				reasons[reason] = true
			}
		}
	}
	f.Reasons, f.State = consolidationStateFor(reasons)
	return f, nil
}

func consolidationStateFor(set map[ConsolidationReason]bool) ([]ConsolidationReason, ConsolidationFreshnessState) {
	reasons := make([]ConsolidationReason, 0, len(set))
	for reason := range set {
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	state := ConsolidationFresh
	for _, reason := range reasons {
		switch {
		case invalidConsolidationReasons[reason]:
			return reasons, ConsolidationInvalid
		case reason == ReasonSourceConflictOpen:
			state = ConsolidationBlocked
		case state == ConsolidationFresh:
			state = ConsolidationStale
		}
	}
	return reasons, state
}

func derivedFromEdgesQ(ctx context.Context, q queryer, candidateID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT to_id FROM context_edges WHERE from_id=? AND relation='derived_from' ORDER BY to_id", candidateID)
	if err != nil {
		return nil, fmt.Errorf("load derived_from edges for %q: %w", candidateID, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan derived_from edge: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
