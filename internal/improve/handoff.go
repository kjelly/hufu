package improve

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

const HandoffSchemaVersion = 1

type ArtifactRef struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

type HandoffScope struct {
	ProjectID     string `json:"project_id"`
	TeamID        string `json:"team_id"`
	AgentID       string `json:"agent_id,omitempty"`
	PolicyVersion string `json:"policy_version,omitempty"`
}

type SourceBinding struct {
	Ref               ArtifactRef `json:"ref"`
	ContentHash       string      `json:"content_hash"`
	AggregateRevision int64       `json:"aggregate_revision,omitempty"`
	ProjectID         string      `json:"project_id"`
	TeamID            string      `json:"team_id"`
}

type BenchmarkBinding struct {
	Ref      ArtifactRef `json:"ref"`
	Name     string      `json:"name"`
	Category string      `json:"category"`
	Cases    int         `json:"cases"`
}

type HandoffKind string

const (
	HandoffMemoryPolicy  HandoffKind = "memory_policy"
	HandoffConsolidation HandoffKind = "context_consolidation"
	HandoffSkill         HandoffKind = "skill"
)

type HandoffStatus string

const (
	HandoffProposed            HandoffStatus = "proposed"
	HandoffCandidateReady      HandoffStatus = "candidate_ready"
	HandoffBenchmarkBound      HandoffStatus = "benchmark_bound"
	HandoffEvaluated           HandoffStatus = "evaluated"
	HandoffEligibleForReview   HandoffStatus = "eligible_for_review"
	HandoffApproved            HandoffStatus = "approved"
	HandoffAdopted             HandoffStatus = "adopted"
	HandoffMonitoring          HandoffStatus = "monitoring"
	HandoffRollbackRecommended HandoffStatus = "rollback_recommended"
	HandoffRejected            HandoffStatus = "rejected"
	HandoffStale               HandoffStatus = "stale"
)

type EvaluationState struct {
	Decision string       `json:"decision,omitempty"`
	Status   string       `json:"status,omitempty"`
	Report   *ArtifactRef `json:"report,omitempty"`
}

type ImprovementHandoff struct {
	Version      int               `json:"version"`
	ID           string            `json:"id"`
	Kind         HandoffKind       `json:"kind"`
	Scope        HandoffScope      `json:"scope"`
	Sources      []SourceBinding   `json:"sources,omitempty"`
	Proposal     ArtifactRef       `json:"proposal"`
	Candidate    *ArtifactRef      `json:"candidate,omitempty"`
	Benchmark    *BenchmarkBinding `json:"benchmark,omitempty"`
	Experiment   *ArtifactRef      `json:"experiment,omitempty"`
	Adoption     *ArtifactRef      `json:"adoption,omitempty"`
	Monitoring   []ArtifactRef     `json:"monitoring,omitempty"`
	Status       HandoffStatus     `json:"status"`
	Evaluation   EvaluationState   `json:"evaluation"`
	StatusReason string            `json:"status_reason,omitempty"`
	Revision     int64             `json:"revision"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

func NewImprovementHandoff(kind HandoffKind, scope HandoffScope, proposal ArtifactRef, sources []SourceBinding) (ImprovementHandoff, error) {
	now := time.Now().UTC()
	orderedSources := append([]SourceBinding(nil), sources...)
	slices.SortFunc(orderedSources, func(left, right SourceBinding) int {
		return cmp.Or(cmp.Compare(left.Ref.Kind, right.Ref.Kind), cmp.Compare(left.Ref.ID, right.Ref.ID))
	})
	handoff := ImprovementHandoff{
		Version: HandoffSchemaVersion, Kind: kind, Scope: scope, Proposal: proposal,
		Sources: orderedSources, Status: HandoffProposed,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	handoff.ID = HandoffID(kind, scope, proposal, sources)
	if err := handoff.Validate(); err != nil {
		return ImprovementHandoff{}, err
	}
	return handoff, nil
}

func HandoffID(kind HandoffKind, scope HandoffScope, proposal ArtifactRef, sources []SourceBinding) string {
	ordered := append([]SourceBinding(nil), sources...)
	slices.SortFunc(ordered, func(left, right SourceBinding) int {
		return cmp.Or(cmp.Compare(left.Ref.Kind, right.Ref.Kind), cmp.Compare(left.Ref.ID, right.Ref.ID))
	})
	identity := struct {
		Kind     HandoffKind
		Scope    HandoffScope
		Proposal ArtifactRef
		Sources  []SourceBinding
	}{kind, scope, proposal, ordered}
	data, _ := json.Marshal(identity)
	sum := sha256.Sum256(data)
	return "handoff-" + hex.EncodeToString(sum[:12])
}

func (h ImprovementHandoff) Validate() error {
	if err := validateHandoffIdentity(h); err != nil {
		return err
	}
	if err := validateHandoffSources(h); err != nil {
		return err
	}
	if err := validateHandoffRefs(h); err != nil {
		return err
	}
	return validateHandoffStatusBindings(h)
}

func validateHandoffIdentity(h ImprovementHandoff) error {
	if h.Version != HandoffSchemaVersion {
		return fmt.Errorf("unsupported handoff schema version %d", h.Version)
	}
	if err := validateArtifactID(h.ID); err != nil {
		return fmt.Errorf("handoff id: %w", err)
	}
	if h.Kind != HandoffMemoryPolicy && h.Kind != HandoffConsolidation && h.Kind != HandoffSkill {
		return fmt.Errorf("unsupported handoff kind %q", h.Kind)
	}
	if strings.TrimSpace(h.Scope.ProjectID) == "" || strings.TrimSpace(h.Scope.TeamID) == "" {
		return fmt.Errorf("handoff project and team are required")
	}
	if err := validateRefForKind(h.Kind, h.Proposal, true); err != nil {
		return fmt.Errorf("proposal: %w", err)
	}
	if h.Revision < 1 || h.CreatedAt.IsZero() || h.UpdatedAt.IsZero() {
		return fmt.Errorf("handoff revision and timestamps are required")
	}
	return nil
}

func validateHandoffSources(h ImprovementHandoff) error {
	if !slices.IsSortedFunc(h.Sources, func(left, right SourceBinding) int {
		return cmp.Or(cmp.Compare(left.Ref.Kind, right.Ref.Kind), cmp.Compare(left.Ref.ID, right.Ref.ID))
	}) {
		return fmt.Errorf("handoff sources must be sorted by kind and id")
	}
	seen := make(map[string]struct{}, len(h.Sources))
	for _, source := range h.Sources {
		if err := validateArtifactRef(source.Ref); err != nil {
			return fmt.Errorf("source: %w", err)
		}
		if strings.TrimSpace(source.ContentHash) == "" || strings.TrimSpace(source.ProjectID) == "" || strings.TrimSpace(source.TeamID) == "" {
			return fmt.Errorf("source binding requires hash, project, and team")
		}
		if source.ProjectID != h.Scope.ProjectID || source.TeamID != h.Scope.TeamID {
			return fmt.Errorf("source scope does not match handoff scope")
		}
		key := source.Ref.Kind + "\x00" + source.Ref.ID
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate source %q", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateHandoffRefs(h ImprovementHandoff) error {
	if h.Candidate != nil {
		if err := validateRefForKind(h.Kind, *h.Candidate, false); err != nil {
			return fmt.Errorf("candidate: %w", err)
		}
	}
	if h.Benchmark != nil {
		if h.Benchmark.Cases < 1 || h.Benchmark.Name == "" || h.Benchmark.Category == "" {
			return fmt.Errorf("benchmark binding is incomplete")
		}
		if err := validateArtifactRef(h.Benchmark.Ref); err != nil || h.Benchmark.Ref.Kind != "benchmark_fixture" {
			return fmt.Errorf("benchmark must reference a benchmark_fixture")
		}
	}
	if h.Experiment != nil {
		if err := validateArtifactRef(*h.Experiment); err != nil || h.Experiment.Kind != "experiment_report" {
			return fmt.Errorf("experiment must reference an experiment_report")
		}
	}
	if h.Adoption != nil {
		if err := validateArtifactRef(*h.Adoption); err != nil {
			return fmt.Errorf("adoption: %w", err)
		}
	}
	for _, monitoring := range h.Monitoring {
		if err := validateArtifactRef(monitoring); err != nil || monitoring.Kind != "monitoring_report" {
			return fmt.Errorf("monitoring must reference a monitoring_report")
		}
	}
	return nil
}

func validateHandoffStatusBindings(h ImprovementHandoff) error {
	if err := validateHandoffStatus(h.Status); err != nil {
		return err
	}
	if requiresCandidate(h.Status) && h.Candidate == nil {
		return fmt.Errorf("status %q requires a candidate", h.Status)
	}
	if requiresBenchmark(h.Status) && h.Benchmark == nil {
		return fmt.Errorf("status %q requires a benchmark", h.Status)
	}
	if requiresEvaluation(h.Status) && (h.Experiment == nil || h.Evaluation.Report == nil) {
		return fmt.Errorf("status %q requires an experiment and report", h.Status)
	}
	if h.Status == HandoffApproved && h.Evaluation.Decision != "eligible_for_review" {
		return fmt.Errorf("approved handoff requires eligible_for_review decision")
	}
	if requiresAdoption(h.Status) && h.Adoption == nil {
		return fmt.Errorf("status %q requires adoption evidence", h.Status)
	}
	if h.Status == HandoffRollbackRecommended && len(h.Monitoring) == 0 {
		return fmt.Errorf("rollback_recommended requires monitoring evidence")
	}
	return nil
}

func requiresCandidate(status HandoffStatus) bool {
	switch status {
	case HandoffCandidateReady, HandoffBenchmarkBound, HandoffEvaluated, HandoffEligibleForReview, HandoffApproved, HandoffAdopted, HandoffMonitoring, HandoffRollbackRecommended:
		return true
	default:
		return false
	}
}

func requiresBenchmark(status HandoffStatus) bool {
	switch status {
	case HandoffBenchmarkBound, HandoffEvaluated, HandoffEligibleForReview, HandoffApproved, HandoffAdopted, HandoffMonitoring, HandoffRollbackRecommended:
		return true
	default:
		return false
	}
}

func requiresEvaluation(status HandoffStatus) bool {
	switch status {
	case HandoffEvaluated, HandoffEligibleForReview, HandoffApproved, HandoffAdopted, HandoffMonitoring, HandoffRollbackRecommended:
		return true
	default:
		return false
	}
}

func requiresAdoption(status HandoffStatus) bool {
	switch status {
	case HandoffAdopted, HandoffMonitoring, HandoffRollbackRecommended:
		return true
	default:
		return false
	}
}

func validateHandoffStatus(status HandoffStatus) error {
	switch status {
	case HandoffProposed, HandoffCandidateReady, HandoffBenchmarkBound, HandoffEvaluated, HandoffEligibleForReview, HandoffApproved, HandoffAdopted, HandoffMonitoring, HandoffRollbackRecommended, HandoffRejected, HandoffStale:
		return nil
	default:
		return fmt.Errorf("unsupported handoff status %q", status)
	}
}

func validateArtifactRef(ref ArtifactRef) error {
	if strings.TrimSpace(ref.Kind) == "" || validateArtifactID(ref.ID) != nil || strings.TrimSpace(ref.Revision) == "" {
		return fmt.Errorf("artifact ref requires kind, safe id, and revision")
	}
	return nil
}

func validateRefForKind(kind HandoffKind, ref ArtifactRef, proposal bool) error {
	if err := validateArtifactRef(ref); err != nil {
		return err
	}
	if proposal {
		want := map[HandoffKind]string{HandoffMemoryPolicy: "memory_policy_proposal", HandoffConsolidation: "consolidation_proposal", HandoffSkill: "promotion_proposal"}[kind]
		if ref.Kind != want {
			return fmt.Errorf("kind %q requires %s proposal, got %s", kind, want, ref.Kind)
		}
		return nil
	}
	want := map[HandoffKind]string{HandoffMemoryPolicy: "memory_policy_snapshot", HandoffConsolidation: "context_item", HandoffSkill: "team_snapshot"}[kind]
	if ref.Kind != want {
		return fmt.Errorf("kind %q requires %s candidate, got %s", kind, want, ref.Kind)
	}
	return nil
}

func validHandoffTransition(from, to HandoffStatus) bool {
	if to == HandoffStale && from != HandoffStale && from != HandoffRejected {
		return from == HandoffProposed || from == HandoffCandidateReady || from == HandoffBenchmarkBound || from == HandoffEvaluated || from == HandoffEligibleForReview || from == HandoffApproved
	}
	switch from {
	case HandoffProposed:
		return to == HandoffCandidateReady || to == HandoffRejected
	case HandoffCandidateReady:
		return to == HandoffBenchmarkBound || to == HandoffRejected
	case HandoffBenchmarkBound:
		return to == HandoffEvaluated || to == HandoffRejected
	case HandoffEvaluated:
		return to == HandoffEligibleForReview || to == HandoffRejected
	case HandoffEligibleForReview:
		return to == HandoffApproved || to == HandoffRejected
	case HandoffApproved:
		return to == HandoffAdopted
	case HandoffAdopted:
		return to == HandoffMonitoring
	case HandoffMonitoring:
		return to == HandoffRollbackRecommended
	default:
		return false
	}
}
