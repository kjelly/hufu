package improve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

const memoryPolicySnapshotVersion = 1

type MemoryRetrievalPolicy struct {
	TopK             int     `json:"top_k,omitempty"`
	CandidateTopK    int     `json:"candidate_top_k"`
	InjectTopK       int     `json:"inject_top_k"`
	MinimumRelevance float64 `json:"minimum_relevance"`
	UtilityWeight    float64 `json:"utility_weight"`
	FreshnessWeight  float64 `json:"freshness_weight"`
	PromptLayout     string  `json:"prompt_layout"`
	AgentCategory    string  `json:"agent_category,omitempty"`
}

type MemoryAttributionPolicy struct {
	RequireExplicitApplied bool    `json:"require_explicit_applied"`
	NegativeCausalMatch    bool    `json:"negative_causal_match"`
	MinimumConfidence      float64 `json:"minimum_confidence"`
}

type MemoryConsolidationPolicy struct {
	MinimumSources             int  `json:"minimum_sources"`
	RequireCrossTaskEvidence   bool `json:"require_cross_task_evidence"`
	RequireScopeWideningReview bool `json:"require_scope_widening_review"`
}

type MemoryPolicySnapshot struct {
	Version         int                        `json:"version"`
	ID              string                     `json:"id"`
	Status          string                     `json:"status"`
	PreviousID      string                     `json:"previous_id,omitempty"`
	Learning        agent.MemoryLearningPolicy `json:"learning"`
	Retrieval       MemoryRetrievalPolicy      `json:"retrieval"`
	Attribution     MemoryAttributionPolicy    `json:"attribution"`
	Consolidation   MemoryConsolidationPolicy  `json:"consolidation"`
	ChangedCategory string                     `json:"changed_category,omitempty"`
	EvaluationGates []GateResult               `json:"evaluation_gates,omitempty"`
	RevisionHash    string                     `json:"revision_hash"`
	CreatedAt       string                     `json:"created_at"`
	ApprovedAt      string                     `json:"approved_at,omitempty"`
}

type MemoryPolicyAdoption struct {
	ActiveID       string `json:"active_id"`
	PreviousID     string `json:"previous_id"`
	ActiveRevision string `json:"active_revision"`
	AdoptedAt      string `json:"adopted_at"`
}

func DefaultMemoryPolicySnapshot(id string) MemoryPolicySnapshot {
	return normalizedMemoryPolicySnapshot(MemoryPolicySnapshot{
		ID: id, Status: "baseline", Learning: agent.DefaultMemoryLearningPolicy(),
		Retrieval:     MemoryRetrievalPolicy{CandidateTopK: 20, InjectTopK: 4, MinimumRelevance: 0.05, UtilityWeight: 0.50, FreshnessWeight: 1, PromptLayout: "canonical-v1"},
		Attribution:   MemoryAttributionPolicy{RequireExplicitApplied: true, NegativeCausalMatch: true},
		Consolidation: MemoryConsolidationPolicy{MinimumSources: 2, RequireCrossTaskEvidence: true, RequireScopeWideningReview: true},
	})
}

func CreateMemoryPolicyCandidate(id string, baseline, candidate MemoryPolicySnapshot) (MemoryPolicySnapshot, error) {
	if strings.TrimSpace(id) == "" || baseline.ID == "" {
		return MemoryPolicySnapshot{}, fmt.Errorf("candidate and baseline IDs are required")
	}
	if err := validateMemoryRetrievalPolicy(baseline.Retrieval); err != nil {
		return MemoryPolicySnapshot{}, fmt.Errorf("baseline retrieval policy: %w", err)
	}
	if err := validateMemoryRetrievalPolicy(candidate.Retrieval); err != nil {
		return MemoryPolicySnapshot{}, fmt.Errorf("candidate retrieval policy: %w", err)
	}
	candidate.ID, candidate.PreviousID, candidate.Status = id, baseline.ID, "candidate"
	candidate.EvaluationGates = nil
	categories := changedMemoryPolicyCategories(baseline, candidate)
	if len(categories) != 1 {
		return MemoryPolicySnapshot{}, fmt.Errorf("memory policy candidate must change exactly one variable category; changed=%v", categories)
	}
	candidate.ChangedCategory = categories[0]
	return normalizedMemoryPolicySnapshot(candidate), nil
}

func validateMemoryRetrievalPolicy(policy MemoryRetrievalPolicy) error {
	if policy.CandidateTopK == 0 && policy.InjectTopK == 0 {
		if policy.TopK <= 0 {
			return fmt.Errorf("legacy top_k must be positive")
		}
		return nil
	}
	if policy.CandidateTopK <= 0 || policy.InjectTopK <= 0 {
		return fmt.Errorf("candidate_top_k and inject_top_k must both be positive")
	}
	if policy.InjectTopK > policy.CandidateTopK {
		return fmt.Errorf("inject_top_k must not exceed candidate_top_k")
	}
	return nil
}

func changedMemoryPolicyCategories(a, b MemoryPolicySnapshot) []string {
	var changed []string
	if !reflect.DeepEqual(a.Learning, b.Learning) {
		changed = append(changed, "reinforcement")
	}
	if !reflect.DeepEqual(a.Retrieval, b.Retrieval) {
		changed = append(changed, "retrieval")
	}
	if !reflect.DeepEqual(a.Attribution, b.Attribution) {
		changed = append(changed, "attribution")
	}
	if !reflect.DeepEqual(a.Consolidation, b.Consolidation) {
		changed = append(changed, "consolidation")
	}
	return changed
}

func normalizedMemoryPolicySnapshot(snapshot MemoryPolicySnapshot) MemoryPolicySnapshot {
	snapshot.Version = memoryPolicySnapshotVersion
	if snapshot.CreatedAt == "" {
		snapshot.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	copyForHash := snapshot
	copyForHash.RevisionHash, copyForHash.Status, copyForHash.CreatedAt, copyForHash.ApprovedAt = "", "", "", ""
	data, _ := json.Marshal(copyForHash)
	sum := sha256.Sum256(data)
	snapshot.RevisionHash = hex.EncodeToString(sum[:])
	return snapshot
}

func EvaluateMemoryPolicyCandidate(candidate MemoryPolicySnapshot, baseline, candidateMetrics Metrics) (MemoryPolicySnapshot, []GateResult, error) {
	if candidate.Status != "candidate" || candidate.ChangedCategory == "" {
		return candidate, nil, fmt.Errorf("memory policy candidate was not produced by CreateMemoryPolicyCandidate")
	}
	gates := []GateResult{
		{Name: "harmful_rate", Passed: candidateMetrics.MemoryHarmfulUseRate == 0, Expected: "0", Observed: fmt.Sprintf("%.4f", candidateMetrics.MemoryHarmfulUseRate)},
		{Name: "attribution_coverage", Passed: candidateMetrics.MemoryAttributionCoverage >= baseline.MemoryAttributionCoverage, Expected: fmt.Sprintf(">= %.4f", baseline.MemoryAttributionCoverage), Observed: fmt.Sprintf("%.4f", candidateMetrics.MemoryAttributionCoverage)},
		{Name: "completion", Passed: completionRate(candidateMetrics) >= completionRate(baseline), Expected: "non-regression", Observed: fmt.Sprintf("%.4f", completionRate(candidateMetrics))},
		{Name: "errors", Passed: candidateMetrics.Error <= baseline.Error, Expected: fmt.Sprintf("<= %d", baseline.Error), Observed: fmt.Sprintf("%d", candidateMetrics.Error)},
		{Name: "retries", Passed: candidateMetrics.RetriedTasks <= baseline.RetriedTasks, Expected: fmt.Sprintf("<= %d", baseline.RetriedTasks), Observed: fmt.Sprintf("%d", candidateMetrics.RetriedTasks)},
		{Name: "assisted_retry_rate", Passed: candidateMetrics.MemoryAssistedRetryRate <= baseline.MemoryAssistedRetryRate, Expected: "non-regression", Observed: fmt.Sprintf("%.4f", candidateMetrics.MemoryAssistedRetryRate)},
		{Name: "unassisted_retry_rate", Passed: candidateMetrics.MemoryUnassistedRetryRate <= baseline.MemoryUnassistedRetryRate, Expected: "non-regression", Observed: fmt.Sprintf("%.4f", candidateMetrics.MemoryUnassistedRetryRate)},
		{Name: "token_overhead", Passed: candidateMetrics.MemoryTokenOverhead <= max(.10, baseline.MemoryTokenOverhead*1.10), Expected: "<= 10%", Observed: fmt.Sprintf("%.4f", candidateMetrics.MemoryTokenOverhead)},
		{Name: "stale_rate", Passed: candidateMetrics.MemoryStaleRetrievalRate <= baseline.MemoryStaleRetrievalRate, Expected: "non-regression", Observed: fmt.Sprintf("%.4f", candidateMetrics.MemoryStaleRetrievalRate)},
	}
	candidate.Status = "eligible_for_review"
	for _, gate := range gates {
		if !gate.Passed {
			candidate.Status = "rejected"
			break
		}
	}
	candidate.EvaluationGates = append([]GateResult(nil), gates...)
	return normalizedMemoryPolicySnapshot(candidate), gates, nil
}

func WriteMemoryPolicySnapshot(workspace string, snapshot MemoryPolicySnapshot) (string, error) {
	if snapshot.ID == "" || snapshot.RevisionHash == "" {
		return "", fmt.Errorf("invalid memory policy snapshot")
	}
	dir := filepath.Join(ImprovementRoot(workspace), "memory-policies")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, snapshot.ID+".json")
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return "", err
	}
	if existing, readErr := os.ReadFile(path); readErr == nil {
		var stored MemoryPolicySnapshot
		if json.Unmarshal(existing, &stored) != nil || stored.RevisionHash != snapshot.RevisionHash {
			return "", fmt.Errorf("memory policy snapshot %q is immutable and already has a different revision", snapshot.ID)
		}
	} else if !os.IsNotExist(readErr) {
		return "", readErr
	}
	temp, err := os.CreateTemp(dir, ".memory-policy-*.tmp")
	if err != nil {
		return "", err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err = os.Rename(tempName, path); err != nil {
		return "", err
	}
	repo, repoErr := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if repoErr != nil {
		return path, fmt.Errorf("open memory policy repository: %w", repoErr)
	}
	defer func() { _ = repo.Close() }()
	createdAt, _ := time.Parse(time.RFC3339Nano, snapshot.CreatedAt)
	if repoErr = repo.SaveMemoryPolicyVersion(context.Background(), snapshot.ID, data, snapshot.RevisionHash, snapshot.Status, createdAt); repoErr != nil {
		return path, fmt.Errorf("record memory policy version: %w", repoErr)
	}
	if snapshot.Status == "eligible_for_review" || snapshot.Status == "rejected" {
		store, openErr := team.OpenEventStore(workspace)
		if openErr != nil {
			return path, fmt.Errorf("open memory policy event store: %w", openErr)
		}
		defer func() { _ = store.Close() }()
		payload, marshalErr := json.Marshal(map[string]any{"schema_version": 1, "policy_version": snapshot.ID, "revision_hash": snapshot.RevisionHash, "status": snapshot.Status, "changed_category": snapshot.ChangedCategory})
		if marshalErr != nil {
			return path, marshalErr
		}
		if appendErr := store.Append(team.RunEvent{Type: "memory_policy_evaluated", Actor: "improve", IdempotencyKey: "memory:policy_evaluated:" + snapshot.ID + ":" + snapshot.RevisionHash, Payload: payload}); appendErr != nil {
			return path, fmt.Errorf("record memory policy evaluation: %w", appendErr)
		}
	}
	return path, nil
}

func LoadMemoryPolicySnapshot(workspace, id string) (MemoryPolicySnapshot, error) {
	data, err := os.ReadFile(filepath.Join(ImprovementRoot(workspace), "memory-policies", id+".json"))
	if err != nil {
		return MemoryPolicySnapshot{}, err
	}
	var snapshot MemoryPolicySnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return snapshot, err
	}
	want := normalizedMemoryPolicySnapshot(snapshot).RevisionHash
	if snapshot.Version != memoryPolicySnapshotVersion || snapshot.RevisionHash != want {
		return snapshot, fmt.Errorf("memory policy snapshot %q failed revision validation", id)
	}
	return snapshot, nil
}

func ApproveMemoryPolicyCandidate(workspace, id string, explicitApproval bool) (MemoryPolicySnapshot, error) {
	if !explicitApproval {
		return MemoryPolicySnapshot{}, fmt.Errorf("explicit approval is required")
	}
	candidate, err := LoadMemoryPolicySnapshot(workspace, id)
	if err != nil {
		return candidate, err
	}
	if candidate.Status != "eligible_for_review" {
		return candidate, fmt.Errorf("candidate %q is not eligible for review", id)
	}
	previous, err := LoadMemoryPolicySnapshot(workspace, candidate.PreviousID)
	if err != nil {
		return candidate, fmt.Errorf("load rollback policy %q: %w", candidate.PreviousID, err)
	}
	categories := changedMemoryPolicyCategories(previous, candidate)
	if len(categories) != 1 || categories[0] != candidate.ChangedCategory {
		return candidate, fmt.Errorf("candidate %q no longer changes exactly its declared policy category", id)
	}
	if len(candidate.EvaluationGates) == 0 {
		return candidate, fmt.Errorf("candidate %q has no durable evaluation gates", id)
	}
	for _, gate := range candidate.EvaluationGates {
		if !gate.Passed {
			return candidate, fmt.Errorf("candidate %q has a failing %s gate", id, gate.Name)
		}
	}
	candidate.Status, candidate.ApprovedAt = "active", time.Now().UTC().Format(time.RFC3339Nano)
	candidate = normalizedMemoryPolicySnapshot(candidate)
	adoption := MemoryPolicyAdoption{ActiveID: candidate.ID, PreviousID: previous.ID, ActiveRevision: candidate.RevisionHash, AdoptedAt: candidate.ApprovedAt}
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		return candidate, err
	}
	defer func() { _ = repo.Close() }()
	if err := repo.ActivateMemoryPolicy(context.Background(), candidate.ID, previous.ID, "superseded"); err != nil {
		return candidate, err
	}
	if err := appendMemoryPolicyLifecycleEvent(workspace, "memory_policy_adopted", candidate.ID, previous.ID, candidate.RevisionHash); err != nil {
		return candidate, err
	}
	// The JSON pointer is a disposable operator projection. SQLite is the
	// canonical adoption record used by runtime startup.
	_ = writeMemoryPolicyAdoption(workspace, adoption)
	return candidate, nil
}

func RollbackMemoryPolicy(workspace string, explicitApproval bool) (MemoryPolicySnapshot, error) {
	if !explicitApproval {
		return MemoryPolicySnapshot{}, fmt.Errorf("explicit approval is required")
	}
	data, err := os.ReadFile(memoryPolicyAdoptionPath(workspace))
	if err != nil {
		return MemoryPolicySnapshot{}, err
	}
	var adoption MemoryPolicyAdoption
	if err := json.Unmarshal(data, &adoption); err != nil {
		return MemoryPolicySnapshot{}, err
	}
	previous, err := LoadMemoryPolicySnapshot(workspace, adoption.PreviousID)
	if err != nil {
		return previous, err
	}
	rolledBack := MemoryPolicyAdoption{ActiveID: previous.ID, PreviousID: adoption.ActiveID, ActiveRevision: previous.RevisionHash, AdoptedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		return previous, err
	}
	defer func() { _ = repo.Close() }()
	if err := repo.ActivateMemoryPolicy(context.Background(), previous.ID, adoption.ActiveID, "rolled_back"); err != nil {
		return previous, err
	}
	if err := appendMemoryPolicyLifecycleEvent(workspace, "memory_policy_rolled_back", previous.ID, adoption.ActiveID, previous.RevisionHash); err != nil {
		return previous, err
	}
	_ = writeMemoryPolicyAdoption(workspace, rolledBack)
	return previous, nil
}

func appendMemoryPolicyLifecycleEvent(workspace, eventType, activeID, previousID, revision string) error {
	store, err := team.OpenEventStore(workspace)
	if err != nil {
		return fmt.Errorf("open memory policy event store: %w", err)
	}
	defer func() { _ = store.Close() }()
	payload, err := json.Marshal(map[string]any{"schema_version": 1, "policy_version": activeID, "previous_policy_version": previousID, "revision_hash": revision})
	if err != nil {
		return err
	}
	return store.Append(team.RunEvent{Type: eventType, Actor: "improve", IdempotencyKey: "memory:" + eventType + ":" + activeID + ":" + revision, Payload: payload})
}

func memoryPolicyAdoptionPath(workspace string) string {
	return filepath.Join(ImprovementRoot(workspace), "memory-policies", "active-policy.json")
}

func writeMemoryPolicyAdoption(workspace string, adoption MemoryPolicyAdoption) error {
	path := memoryPolicyAdoptionPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(adoption, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".active-policy-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

type MemoryOptimizerProposal struct {
	BasePolicyID string               `json:"base_policy_id"`
	Candidate    MemoryPolicySnapshot `json:"candidate"`
	Reason       string               `json:"reason"`
}

// ContextOutcomeSummary is a content-free optimizer input aggregated from
// context_item × phase × trigger × role × environment observations.
type ContextOutcomeSummary struct {
	Selected int `json:"selected"`
	Positive int `json:"positive"`
	Negative int `json:"negative"`
}

// ProposeContextPolicyOptimization creates an immutable candidate only. It
// deliberately reuses the shadow evaluation, explicit approval, adoption,
// explain, and rollback machinery below; observations can never mutate the
// active runtime policy directly.
func ProposeContextPolicyOptimization(id string, baseline MemoryPolicySnapshot, summary ContextOutcomeSummary) (MemoryOptimizerProposal, error) {
	candidate := baseline
	reason := "insufficient negative observations; tighten minimum relevance conservatively"
	if summary.Negative > summary.Positive && candidate.Retrieval.CandidateTopK > candidate.Retrieval.InjectTopK {
		candidate.Retrieval.CandidateTopK--
		reason = "reduce candidate breadth after dimensioned negative context outcomes"
	} else {
		candidate.Retrieval.MinimumRelevance = min(1, candidate.Retrieval.MinimumRelevance+0.01)
	}
	created, err := CreateMemoryPolicyCandidate(id, baseline, candidate)
	return MemoryOptimizerProposal{BasePolicyID: baseline.ID, Candidate: created, Reason: reason}, err
}

func MemoryL3BenchmarkFixture(teamName string) BenchmarkFixture {
	return BenchmarkFixture{
		Version: benchmarkVersion, Name: "memory-l3", Team: teamName, Category: "outcome-driven-memory",
		Description: "Fixed L3 transfer, irrelevance, and stale/harmful memory gates.",
		Cases: []BenchmarkCase{
			{ID: "positive-transfer", Type: "positive_transfer", Prompt: "Apply a previously verified reusable procedure to an equivalent task and report objective verification."},
			{ID: "irrelevant-high-utility", Type: "irrelevant_high_utility", Prompt: "Complete the task without selecting an unrelated memory solely because it has high historical utility."},
			{ID: "stale-harmful", Type: "stale_harmful", Prompt: "Complete the task while excluding stale or causally harmful memory from the active prompt."},
		},
	}
}

func ProposeMemoryPolicyOptimization(id string, baseline MemoryPolicySnapshot, metrics Metrics) (MemoryOptimizerProposal, error) {
	candidate := baseline
	reason := "reduce memory retrieval breadth after observed harm or stale recall"
	if metrics.MemoryHarmfulUseRate > 0 || metrics.MemoryStaleRetrievalRate > 0 {
		if baseline.Retrieval.CandidateTopK > baseline.Retrieval.InjectTopK {
			candidate.Retrieval.CandidateTopK = baseline.Retrieval.CandidateTopK - 1
		} else {
			candidate.Retrieval.InjectTopK = max(1, baseline.Retrieval.InjectTopK-1)
		}
	} else {
		candidate.Retrieval.MinimumRelevance = min(1, baseline.Retrieval.MinimumRelevance+0.01)
		reason = "raise relevance threshold conservatively; proposal only"
	}
	candidate, err := CreateMemoryPolicyCandidate(id, baseline, candidate)
	return MemoryOptimizerProposal{BasePolicyID: baseline.ID, Candidate: candidate, Reason: reason}, err
}

func ControlledMemoryExplorationAllowed(sideEffect team.SideEffectClass, recovery team.RecoveryPolicy, tools []string, sandboxed bool) bool {
	if sideEffect != team.SideEffectNone || !sandboxed || recovery == team.RecoveryManual || recovery == team.RecoveryNever {
		return false
	}
	readOnly := map[string]bool{"view": true, "grep": true, "glob": true, "ls": true, "math": true}
	for _, tool := range tools {
		if !readOnly[strings.ToLower(strings.TrimSpace(tool))] {
			return false
		}
	}
	return true
}

type SkillProposal struct {
	Name               string   `json:"name"`
	SourceContextIDs   []string `json:"source_context_ids"`
	SourceRevisions    []int64  `json:"source_revisions"`
	CandidateSnapshot  string   `json:"candidate_snapshot"`
	CandidateSkillPath string   `json:"candidate_skill_path"`
	Status             string   `json:"status"`
	RequiresBenchmark  bool     `json:"requires_benchmark"`
	RequiresReview     bool     `json:"requires_review"`
	RequiresPR         bool     `json:"requires_pr"`
	RequiresMonitoring bool     `json:"requires_monitoring"`
	RollbackRequired   bool     `json:"rollback_required"`
}

func ProposeSkillCandidate(name string, sourceIDs []string, revisions []int64) (SkillProposal, error) {
	name = strings.TrimSpace(name)
	if validateArtifactID(name) != nil || len(sourceIDs) < 2 || len(sourceIDs) != len(revisions) {
		return SkillProposal{}, fmt.Errorf("skill proposal requires a name and at least two revision-bound sources")
	}
	type sourceRevision struct {
		id       string
		revision int64
	}
	pairs := make([]sourceRevision, len(sourceIDs))
	for i := range sourceIDs {
		pairs[i] = sourceRevision{id: sourceIDs[i], revision: revisions[i]}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].id < pairs[j].id })
	ids, orderedRevisions := make([]string, len(pairs)), make([]int64, len(pairs))
	var identity strings.Builder
	identity.WriteString(name)
	for i, pair := range pairs {
		ids[i], orderedRevisions[i] = pair.id, pair.revision
		fmt.Fprintf(&identity, "\x00%s\x00%d", pair.id, pair.revision)
	}
	digest := sha256.Sum256([]byte(identity.String()))
	return SkillProposal{Name: name, SourceContextIDs: ids, SourceRevisions: orderedRevisions, CandidateSnapshot: "skill-candidate-" + hex.EncodeToString(digest[:10]), CandidateSkillPath: filepath.ToSlash(filepath.Join(".agents", "skills", name, "SKILL.md")), Status: "proposal", RequiresBenchmark: true, RequiresReview: true, RequiresPR: true, RequiresMonitoring: true, RollbackRequired: true}, nil
}
