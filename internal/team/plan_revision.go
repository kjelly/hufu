package team

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// PlanRevisionStatus is the durable lifecycle of a constrained replan.
type PlanRevisionStatus string

const (
	PlanRevisionProposed  PlanRevisionStatus = "proposed"
	PlanRevisionValidated PlanRevisionStatus = "validated"
	PlanRevisionApproved  PlanRevisionStatus = "approved"
	PlanRevisionRejected  PlanRevisionStatus = "rejected"
	PlanRevisionApplied   PlanRevisionStatus = "applied"
)

type PlanTaskChange struct {
	ID         string   `json:"id"`
	PreviousID string   `json:"previous_id,omitempty"`
	Agent      string   `json:"agent,omitempty"`
	Goal       string   `json:"goal,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	DependsOn  []string `json:"depends_on,omitempty"`
}

type PlanDiff struct {
	Added        []PlanTaskChange `json:"added,omitempty"`
	Removed      []PlanTaskChange `json:"removed,omitempty"`
	Modified     []PlanTaskChange `json:"modified,omitempty"`
	Unchanged    []PlanTaskChange `json:"unchanged,omitempty"`
	EdgesAdded   []string         `json:"edges_added,omitempty"`
	EdgesRemoved []string         `json:"edges_removed,omitempty"`
}

type PlanReviewResult struct {
	Status         string    `json:"status"` // pending, approved, rejected
	Reason         string    `json:"reason,omitempty"`
	Reviewer       string    `json:"reviewer,omitempty"`
	ReviewedAt     time.Time `json:"reviewed_at,omitempty"`
	RevisionID     string    `json:"revision_id,omitempty"`
	RevisionDigest string    `json:"revision_digest,omitempty"`
}

// PlanRevision is an immutable, auditable replacement for a failed plan.
// TaskDAG is copied on construction and is never mutated by validation.
type PlanRevision struct {
	ID                    string             `json:"id"`
	ParentID              string             `json:"parent_id,omitempty"`
	DiagnosticPacketIDs   []string           `json:"diagnostic_packet_ids,omitempty"`
	Goal                  string             `json:"goal"`
	Constraints           string             `json:"constraints,omitempty"`
	AcceptanceFingerprint string             `json:"acceptance_fingerprint"`
	TaskDAG               []TaskDef          `json:"task_dag"`
	DAGDiff               PlanDiff           `json:"dag_diff"`
	RepairHypothesisIDs   []string           `json:"repair_hypothesis_ids,omitempty"`
	CompletedTaskIDs      []string           `json:"completed_task_ids,omitempty"`
	Review                PlanReviewResult   `json:"review"`
	Status                PlanRevisionStatus `json:"status"`
	CreatedAt             time.Time          `json:"created_at"`
}

// PlanValidationContext contains deterministic authorization and budget facts.
// Empty maps mean that the corresponding policy has no additional restriction.
type PlanValidationContext struct {
	AcceptanceFingerprint      string
	AuthorizedTools            map[string]map[string]bool
	AllowedPaths               map[string][]string
	MaxTasks                   int
	MaxAttempts                int
	RemainingTokens            int64
	EstimatedTokensPerAttempt  int64
	RemainingSeconds           int64
	EstimatedSecondsPerAttempt int64
	TokenBudgetConfigured      bool
	DurationBudgetConfigured   bool
}

func AcceptanceFingerprint(spec *AcceptanceSpec, legacyCommand string) string {
	if spec == nil {
		return digestText("acceptance:none|" + legacyCommand)
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return digestText("acceptance:invalid|" + legacyCommand)
	}
	return digestText(string(b))
}

func taskRevisionID(task TaskDef) string {
	if task.ID != "" {
		return task.ID
	}
	b, _ := json.Marshal(task)
	return digestText(string(b))[:16]
}

// normalizePlanTaskIDs gives every task a stable identity before the plan
// diff is built. Explicit IDs are preserved; otherwise the deterministic
// task fingerprint is used. Duplicate identities are rejected so completion
// evidence cannot be attributed to the wrong task.
func normalizePlanTaskIDs(tasks []TaskDef) error {
	seen := make(map[string]int, len(tasks))
	for i := range tasks {
		id := tasks[i].ID
		if id == "" {
			id = taskRevisionID(tasks[i])
		}
		if previous, exists := seen[id]; exists {
			return fmt.Errorf("duplicate plan task ID %q at indexes %d and %d", id, previous, i)
		}
		tasks[i].ID = id
		seen[id] = i
	}
	return nil
}

func taskDependsOnIDs(tasks []TaskDef, index int) []string {
	var ids []string
	for _, dep := range tasks[index].DependsOn {
		if dep >= 0 && dep < len(tasks) {
			ids = append(ids, taskRevisionID(tasks[dep]))
		}
	}
	sort.Strings(ids)
	return ids
}

func taskSemanticDigest(tasks []TaskDef, index int) string {
	task := cloneTaskDef(tasks[index])
	dependencies := taskDependsOnIDs(tasks, index)
	onFailure := ""
	var invalidOnFailure *int
	if task.OnFailure != nil && *task.OnFailure >= 0 && *task.OnFailure < len(tasks) {
		onFailure = taskRevisionID(tasks[*task.OnFailure])
	} else if task.OnFailure != nil {
		invalid := *task.OnFailure
		invalidOnFailure = &invalid
	}
	task.ID = ""
	task.DependsOn = nil
	task.OnFailure = nil
	b, _ := json.Marshal(struct {
		Task             TaskDef  `json:"task"`
		Dependencies     []string `json:"dependencies,omitempty"`
		OnFailure        string   `json:"on_failure,omitempty"`
		InvalidOnFailure *int     `json:"invalid_on_failure,omitempty"`
	}{task, dependencies, onFailure, invalidOnFailure})
	return digestText(string(b))
}

func buildPlanDiff(parent []TaskDef, next []TaskDef) PlanDiff {
	max := len(parent)
	if len(next) > max {
		max = len(next)
	}
	var diff PlanDiff
	for i := 0; i < max; i++ {
		switch {
		case i >= len(parent):
			diff.Added = append(diff.Added, PlanTaskChange{ID: taskRevisionID(next[i]), Agent: next[i].Agent, Goal: next[i].Goal, Reason: "new task", DependsOn: taskDependsOnIDs(next, i)})
		case i >= len(next):
			diff.Removed = append(diff.Removed, PlanTaskChange{ID: taskRevisionID(parent[i]), Agent: parent[i].Agent, Goal: parent[i].Goal, Reason: "replaced by revision"})
		default:
			oldID, newID := taskRevisionID(parent[i]), taskRevisionID(next[i])
			change := PlanTaskChange{ID: newID, PreviousID: oldID, Agent: next[i].Agent, Goal: next[i].Goal, DependsOn: taskDependsOnIDs(next, i)}
			if oldID == newID && taskSemanticDigest(parent, i) == taskSemanticDigest(next, i) {
				diff.Unchanged = append(diff.Unchanged, change)
			} else {
				change.Reason = "task or dependency changed"
				diff.Modified = append(diff.Modified, change)
			}
		}
	}
	return diff
}

func ValidatePlanRevision(revision PlanRevision, parent *PlanRevision, ctx PlanValidationContext) error {
	if revision.ID == "" || revision.Goal == "" || revision.AcceptanceFingerprint == "" {
		return fmt.Errorf("plan revision requires id, goal, and acceptance fingerprint")
	}
	if parent != nil && revision.ParentID != parent.ID {
		return fmt.Errorf("plan revision parent %q does not match %q", revision.ParentID, parent.ID)
	}
	if parent != nil && parent.AcceptanceFingerprint != revision.AcceptanceFingerprint {
		return fmt.Errorf("acceptance fingerprint changed from parent revision")
	}
	if ctx.AcceptanceFingerprint != "" && revision.AcceptanceFingerprint != ctx.AcceptanceFingerprint {
		return fmt.Errorf("acceptance fingerprint changed across plan revision")
	}
	if ctx.MaxTasks > 0 && len(revision.TaskDAG) > ctx.MaxTasks {
		return fmt.Errorf("plan has %d tasks, exceeding budget %d", len(revision.TaskDAG), ctx.MaxTasks)
	}
	attempts := 0
	for i, task := range revision.TaskDAG {
		attempts += task.MaxRetries + 1
		for _, dep := range task.DependsOn {
			if dep < 0 || dep >= len(revision.TaskDAG) || dep == i {
				return fmt.Errorf("task %d has invalid dependency %d", i, dep)
			}
		}
		if isHighRiskSideEffect(task.SideEffect) && strings.TrimSpace(task.Verify) == "" && task.VerifySpec == nil {
			return fmt.Errorf("high-risk task %d requires verification", i)
		}
		if allowed, ok := ctx.AuthorizedTools[task.Agent]; ok {
			for _, required := range task.Requires {
				if !allowed[required] {
					return fmt.Errorf("task %d requires unauthorized tool %q", i, required)
				}
			}
		}
		if paths, ok := ctx.AllowedPaths[task.Agent]; ok {
			for _, path := range task.ContextFiles {
				if !pathAllowed(path, paths) {
					return fmt.Errorf("task %d uses unauthorized path %q", i, path)
				}
			}
		}
	}
	if ctx.MaxAttempts > 0 && attempts > ctx.MaxAttempts {
		return fmt.Errorf("plan attempts %d exceed budget %d", attempts, ctx.MaxAttempts)
	}
	if ctx.TokenBudgetConfigured {
		if ctx.RemainingTokens <= 0 {
			return fmt.Errorf("plan token budget is exhausted")
		}
		estimate := ctx.EstimatedTokensPerAttempt
		if estimate <= 0 {
			estimate = 1
		}
		if int64(attempts)*estimate > ctx.RemainingTokens {
			return fmt.Errorf("plan token budget estimate exceeds remaining budget")
		}
	}
	if ctx.DurationBudgetConfigured {
		if ctx.RemainingSeconds <= 0 {
			return fmt.Errorf("plan duration budget is exhausted")
		}
		estimate := ctx.EstimatedSecondsPerAttempt
		if estimate <= 0 {
			estimate = 1
		}
		if int64(attempts)*estimate > ctx.RemainingSeconds {
			return fmt.Errorf("plan duration estimate exceeds remaining budget")
		}
	}
	if cycle, ok := planCycle(revision.TaskDAG); ok {
		return fmt.Errorf("plan dependency cycle detected at task %d", cycle)
	}
	if err := validateResourceClaims(revision.TaskDAG); err != nil {
		return err
	}
	if err := validateOnFailureTargets(revision.TaskDAG); err != nil {
		return fmt.Errorf("on_failure validation failed: %w", err)
	}
	return nil
}

func isHighRiskSideEffect(effect SideEffectClass) bool {
	return effect == SideEffectExternalWrite || effect == SideEffectInfraMutation || effect == SideEffectCredential
}

func pathAllowed(path string, allowed []string) bool {
	clean := filepath.Clean(path)
	for _, root := range allowed {
		root = filepath.Clean(root)
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func planCycle(tasks []TaskDef) (int, bool) {
	state := make([]uint8, len(tasks))
	var visit func(int) (int, bool)
	visit = func(i int) (int, bool) {
		if state[i] == 1 {
			return i, true
		}
		if state[i] == 2 {
			return -1, false
		}
		state[i] = 1
		for _, dep := range tasks[i].DependsOn {
			if dep >= 0 && dep < len(tasks) {
				if found, ok := visit(dep); ok {
					return found, true
				}
			}
		}
		state[i] = 2
		return -1, false
	}
	for i := range tasks {
		if found, ok := visit(i); ok {
			return found, true
		}
	}
	return -1, false
}

func validateResourceClaims(tasks []TaskDef) error {
	for i := range tasks {
		for j := i + 1; j < len(tasks); j++ {
			if claimsConflict(resourceClaims(tasks[i]), resourceClaims(tasks[j])) && !dependsEither(tasks, i, j) {
				return fmt.Errorf("resource claims conflict between parallel tasks %d and %d", i, j)
			}
		}
	}
	return nil
}

func resourceClaims(task TaskDef) []ResourceClaim {
	claims := append([]ResourceClaim(nil), task.Resources...)
	for _, resource := range task.ResourceClaims {
		if resource != "" {
			claims = append(claims, ResourceClaim{Resource: resource, Mode: ResourceExclusive})
		}
	}
	return claims
}

func normalizeResourceClaimMode(mode ResourceClaimMode) ResourceClaimMode {
	if mode == "" {
		return ResourceExclusive
	}
	return mode
}

func claimsConflict(left, right []ResourceClaim) bool {
	for _, a := range left {
		if a.Resource == "" {
			continue
		}
		for _, b := range right {
			if a.Resource != b.Resource {
				continue
			}
			if normalizeResourceClaimMode(a.Mode) != ResourceRead || normalizeResourceClaimMode(b.Mode) != ResourceRead {
				return true
			}
		}
	}
	return false
}

func dependsEither(tasks []TaskDef, from, target int) bool {
	reaches := func(start, target int) bool {
		seen := map[int]bool{}
		var visit func(int) bool
		visit = func(i int) bool {
			if i == target {
				return true
			}
			if seen[i] {
				return false
			}
			seen[i] = true
			for _, dep := range tasks[i].DependsOn {
				if dep >= 0 && dep < len(tasks) && visit(dep) {
					return true
				}
			}
			return false
		}
		return visit(start)
	}
	return reaches(from, target) || reaches(target, from)
}

func NewPlanRevision(parent *PlanRevision, packet DiagnosticPacket, goal, constraints string, tasks []TaskDef, acceptanceFingerprint string) (PlanRevision, error) {
	if strings.TrimSpace(goal) == "" {
		return PlanRevision{}, fmt.Errorf("plan revision goal is required")
	}
	cloned := make([]TaskDef, len(tasks))
	for i := range tasks {
		cloned[i] = cloneTaskDef(tasks[i])
	}
	if err := normalizePlanTaskIDs(cloned); err != nil {
		return PlanRevision{}, err
	}
	parentID := ""
	var parentTasks []TaskDef
	if parent != nil {
		parentID, parentTasks = parent.ID, parent.TaskDAG
	}
	revision := PlanRevision{ParentID: parentID, DiagnosticPacketIDs: []string{packet.ID}, Goal: goal, Constraints: constraints, AcceptanceFingerprint: acceptanceFingerprint, TaskDAG: cloned, DAGDiff: buildPlanDiff(parentTasks, cloned), Status: PlanRevisionProposed, Review: PlanReviewResult{Status: "pending"}, CreatedAt: time.Now().UTC()}
	for _, hypothesis := range packet.Hypotheses {
		if hypothesis.ID != "" {
			revision.RepairHypothesisIDs = append(revision.RepairHypothesisIDs, hypothesis.ID)
		}
	}
	b, _ := json.Marshal(struct {
		Parent, Goal, Constraints, Acceptance string
		Packets                               []string
		Tasks                                 []TaskDef
	}{parentID, goal, constraints, acceptanceFingerprint, revision.DiagnosticPacketIDs, cloned})
	revision.ID = "plan-" + digestText(string(b))[:16]
	if err := ValidatePlanRevision(revision, parent, PlanValidationContext{AcceptanceFingerprint: acceptanceFingerprint}); err != nil {
		return PlanRevision{}, err
	}
	return revision, nil
}

func clonePlanRevision(src PlanRevision) PlanRevision {
	clone := src
	clone.DiagnosticPacketIDs = append([]string(nil), src.DiagnosticPacketIDs...)
	clone.RepairHypothesisIDs = append([]string(nil), src.RepairHypothesisIDs...)
	clone.CompletedTaskIDs = append([]string(nil), src.CompletedTaskIDs...)
	clone.TaskDAG = make([]TaskDef, len(src.TaskDAG))
	for i := range src.TaskDAG {
		clone.TaskDAG[i] = cloneTaskDef(src.TaskDAG[i])
	}
	clone.DAGDiff = clonePlanDiff(src.DAGDiff)
	return clone
}

func canonicalPlanRevisionDigest(revision PlanRevision) string {
	clone := clonePlanRevision(revision)
	clone.Review = PlanReviewResult{}
	clone.Status = PlanRevisionProposed
	// Completion IDs are runtime evidence derived from the coordinator task
	// tracker, not part of the caller-supplied immutable plan identity.
	clone.CompletedTaskIDs = nil
	b, _ := json.Marshal(clone)
	return digestText(string(b))
}

func clonePlanDiff(src PlanDiff) PlanDiff {
	clone := src
	clone.Added = clonePlanChanges(src.Added)
	clone.Removed = clonePlanChanges(src.Removed)
	clone.Modified = clonePlanChanges(src.Modified)
	clone.Unchanged = clonePlanChanges(src.Unchanged)
	clone.EdgesAdded = append([]string(nil), src.EdgesAdded...)
	clone.EdgesRemoved = append([]string(nil), src.EdgesRemoved...)
	return clone
}

func clonePlanChanges(src []PlanTaskChange) []PlanTaskChange {
	clone := make([]PlanTaskChange, len(src))
	for i, change := range src {
		clone[i] = change
		clone[i].DependsOn = append([]string(nil), change.DependsOn...)
	}
	return clone
}

// PersistPlanRevision validates and durably records a revision before any
// caller is allowed to dispatch its task DAG.
func (c *Coordinator) PersistPlanRevision(revision PlanRevision) error {
	if c == nil {
		return fmt.Errorf("coordinator is nil")
	}
	canonical := clonePlanRevision(revision)
	if err := normalizePlanTaskIDs(canonical.TaskDAG); err != nil {
		return err
	}
	// Review.Status in caller input is an untrusted proposal. Only a
	// coordinator-owned review record can authorize execution later.
	canonical.Review = PlanReviewResult{}
	canonical.CompletedTaskIDs = nil
	c.planRevisionsMu.RLock()
	var parent *PlanRevision
	for _, existing := range c.planRevisions {
		if existing.ID == canonical.ID {
			c.planRevisionsMu.RUnlock()
			return fmt.Errorf("plan revision %q already exists; equivalent replan is not allowed", revision.ID)
		}
	}
	if len(c.planRevisions) > 0 {
		copyParent := c.planRevisions[len(c.planRevisions)-1]
		parent = &copyParent
	}
	c.planRevisionsMu.RUnlock()
	if parent == nil && canonical.ParentID != "" {
		return fmt.Errorf("plan revision parent %q is not available", canonical.ParentID)
	}
	ctx, err := c.planValidationContext(canonical)
	if err != nil {
		return err
	}
	if err := ValidatePlanRevision(canonical, parent, ctx); err != nil {
		return err
	}
	if canonical.Review.Status == "approved" {
		canonical.Status = PlanRevisionApproved
	} else {
		canonical.Status = PlanRevisionValidated
	}
	c.planRevisionsMu.Lock()
	c.planRevisions = append(c.planRevisions, clonePlanRevision(canonical))
	c.planRevisionsMu.Unlock()
	if c.sessionData != nil {
		c.sessionData.PlanRevisions = append(c.sessionData.PlanRevisions, clonePlanRevision(canonical))
	}
	return c.emitEvent("plan_revision", "coordinator", "", map[string]interface{}{"revision": clonePlanRevision(canonical)})
}

// recordTrustedPlanReview is intentionally unexported: only the coordinator's
// reviewer integration may create an approval. The result is bound to the
// already-persisted canonical revision digest, so a caller cannot self-label a
// different revision as approved.
func (c *Coordinator) recordTrustedPlanReview(revisionID string, review PlanReviewResult) error {
	if c == nil || revisionID == "" || (review.Status != "approved" && review.Status != "rejected") || strings.TrimSpace(review.Reviewer) == "" {
		return fmt.Errorf("invalid trusted plan review")
	}
	c.planRevisionsMu.RLock()
	var revision *PlanRevision
	for _, candidate := range c.planRevisions {
		if candidate.ID == revisionID {
			copyRevision := clonePlanRevision(candidate)
			revision = &copyRevision
			break
		}
	}
	c.planRevisionsMu.RUnlock()
	if revision == nil {
		return fmt.Errorf("plan revision %q is not persisted", revisionID)
	}
	digest := canonicalPlanRevisionDigest(*revision)
	if review.RevisionID != revisionID || review.RevisionDigest != digest {
		return fmt.Errorf("plan review does not match canonical revision")
	}
	if review.ReviewedAt.IsZero() {
		return fmt.Errorf("plan review timestamp is required")
	}
	c.planReviewsMu.Lock()
	if c.planReviews == nil {
		c.planReviews = make(map[string]PlanReviewResult)
	}
	c.planReviews[revisionID] = review
	c.planReviewsMu.Unlock()
	if c.sessionData != nil {
		updated := false
		for i := range c.sessionData.PlanReviews {
			if c.sessionData.PlanReviews[i].RevisionID == revisionID {
				c.sessionData.PlanReviews[i] = review
				updated = true
				break
			}
		}
		if !updated {
			c.sessionData.PlanReviews = append(c.sessionData.PlanReviews, review)
		}
	}
	return c.emitEvent("plan_review", "plan-reviewer", "", map[string]interface{}{"review": review})
}

func (c *Coordinator) trustedPlanReview(revisionID, digest string) (PlanReviewResult, bool) {
	c.planReviewsMu.RLock()
	defer c.planReviewsMu.RUnlock()
	review, ok := c.planReviews[revisionID]
	if !ok || review.Status != "approved" || review.RevisionID != revisionID || review.RevisionDigest != digest || review.ReviewedAt.IsZero() {
		return PlanReviewResult{}, false
	}
	return review, true
}

// ReviewPlanRevision is the coordinator-owned review entry point. It runs
// deterministic safety checks and, when a plan-reviewer model is configured,
// invokes the existing planReviewer agent. The reviewer tools are placed in a
// revision-only mode so approval records a decision and never executes work.
// The resulting decision is always bound to the canonical revision digest.
func (c *Coordinator) ReviewPlanRevision(revisionID string) (PlanReviewResult, error) {
	return c.ReviewPlanRevisionWithContext(context.Background(), revisionID)
}

// ReviewPlanRevisionWithContext is the cancellable form used by coordinator
// execution paths that already own a request context.
func (c *Coordinator) ReviewPlanRevisionWithContext(ctx context.Context, revisionID string) (PlanReviewResult, error) {
	c.planRevisionsMu.RLock()
	var revision *PlanRevision
	for _, candidate := range c.planRevisions {
		if candidate.ID == revisionID {
			copyRevision := clonePlanRevision(candidate)
			revision = &copyRevision
			break
		}
	}
	c.planRevisionsMu.RUnlock()
	if revision == nil {
		return PlanReviewResult{}, fmt.Errorf("plan revision %q is not persisted", revisionID)
	}
	review := PlanReviewResult{
		Status:         "approved",
		Reviewer:       "coordinator-plan-reviewer",
		ReviewedAt:     time.Now().UTC(),
		RevisionID:     revision.ID,
		RevisionDigest: canonicalPlanRevisionDigest(*revision),
		Reason:         "deterministic feasibility, completeness, minimal-diff, and safety review passed",
	}
	if len(revision.DAGDiff.Added) == 0 && len(revision.DAGDiff.Modified) == 0 {
		review.Status = "rejected"
		review.Reason = "plan revision has no executable task changes"
	} else if _, err := planTasksForExecution(*revision, coordinatorCompletedTaskIDs(c)); err != nil {
		review.Status = "rejected"
		review.Reason = "plan review rejected: " + err.Error()
	} else if c.planReviewerModel != "" && c.providerManager != nil {
		review, err = c.reviewPlanRevisionWithReviewer(ctx, *revision, review)
		if err != nil {
			return PlanReviewResult{}, err
		}
	}
	if err := c.recordTrustedPlanReview(revision.ID, review); err != nil {
		return PlanReviewResult{}, err
	}
	return review, nil
}

func (c *Coordinator) reviewPlanRevisionWithReviewer(ctx context.Context, revision PlanRevision, review PlanReviewResult) (PlanReviewResult, error) {
	if c.pendingPlans == nil {
		c.pendingPlans = make(map[string]*PlanEntry)
	}
	if c.approvedOutputs == nil {
		c.approvedOutputs = make(map[string]string)
	}
	if c.approvedErrors == nil {
		c.approvedErrors = make(map[string]error)
	}

	agentName := "coordinator"
	var task TaskDef
	if len(revision.TaskDAG) > 0 {
		task = cloneTaskDef(revision.TaskDAG[0])
		if task.Agent != "" {
			agentName = task.Agent
		}
	}
	planText, err := json.Marshal(struct {
		RevisionID  string    `json:"revision_id"`
		Goal        string    `json:"goal"`
		Constraints string    `json:"constraints,omitempty"`
		TaskDAG     []TaskDef `json:"task_dag"`
		Diff        PlanDiff  `json:"diff"`
	}{revision.ID, revision.Goal, revision.Constraints, revision.TaskDAG, revision.DAGDiff})
	if err != nil {
		return PlanReviewResult{}, fmt.Errorf("encode plan revision for review: %w", err)
	}

	c.pendingPlansMu.Lock()
	c.pendingPlans[revision.ID] = &PlanEntry{
		TodoID: revision.ID, PlanRevisionID: revision.ID, Agent: agentName,
		Goal: revision.Goal, PlanText: string(planText), Status: "submitted", Task: task,
	}
	c.pendingPlansMu.Unlock()

	pr, err := c.getPlanReviewer(ctx, revision.ID)
	if err != nil {
		c.deletePlanRevisionReviewEntry(revision.ID)
		return PlanReviewResult{}, fmt.Errorf("create plan revision reviewer: %w", err)
	}
	_, _, _, err = pr.review(ctx, string(planText))
	if err != nil {
		c.deletePlanRevisionReviewEntry(revision.ID)
		return PlanReviewResult{}, fmt.Errorf("plan revision reviewer failed: %w", err)
	}

	c.pendingPlansMu.Lock()
	entry := c.pendingPlans[revision.ID]
	status, reason := "", ""
	if entry != nil {
		status, reason = entry.Status, entry.ReviewReason
	}
	c.pendingPlansMu.Unlock()
	c.deletePlanRevisionReviewEntry(revision.ID)
	if status != "approved" && status != "rejected" {
		return PlanReviewResult{}, fmt.Errorf("plan revision reviewer did not issue an approval decision")
	}
	review.Status = status
	review.Reviewer = "plan-reviewer:" + c.planReviewerModel
	if strings.TrimSpace(reason) != "" {
		review.Reason = reason
	} else if status == "approved" {
		review.Reason = "plan reviewer approved the canonical revision"
	}
	return review, nil
}

func (c *Coordinator) deletePlanRevisionReviewEntry(revisionID string) {
	c.pendingPlansMu.Lock()
	delete(c.pendingPlans, revisionID)
	delete(c.approvedOutputs, revisionID)
	delete(c.approvedErrors, revisionID)
	c.pendingPlansMu.Unlock()
}

// SetPlanBudget configures the remaining task/attempt budget used by the
// deterministic replan gate. Zero leaves that dimension unlimited.
func (c *Coordinator) SetPlanBudget(maxTasks, maxAttempts int) {
	if c == nil {
		return
	}
	c.planMaxTasks = maxTasks
	c.planMaxAttempts = maxAttempts
}

func (c *Coordinator) planValidationContext(revision PlanRevision) (PlanValidationContext, error) {
	ctx := PlanValidationContext{MaxTasks: c.planMaxTasks, MaxAttempts: c.planMaxAttempts}
	c.mu.RLock()
	var spec *AcceptanceSpec
	legacyAcceptance := c.acceptanceCmd
	if c.acceptanceSpec != nil {
		copySpec := cloneAcceptanceSpec(*c.acceptanceSpec)
		spec = &copySpec
	}
	session := c.session
	c.mu.RUnlock()
	ctx.AcceptanceFingerprint = AcceptanceFingerprint(spec, legacyAcceptance)
	if session != nil {
		if ctx.MaxTasks == 0 && session.Config.MaxConcurrent > 0 {
			// max-concurrent is the only configured dispatch-size bound; using it
			// here is conservative for a single constrained repair revision.
			ctx.MaxTasks = session.Config.MaxConcurrent
		}
	}
	if c.tokenBudget > 0 {
		ctx.TokenBudgetConfigured = true
		ctx.RemainingTokens = c.tokenBudget - c.tokensUsed.Load()
		ctx.EstimatedTokensPerAttempt = 1000
	}
	if c.maxWallClock > 0 {
		ctx.DurationBudgetConfigured = true
		elapsed := time.Duration(0)
		if !c.sessionTime.IsZero() {
			elapsed = time.Since(c.sessionTime)
		}
		ctx.RemainingSeconds = int64((c.maxWallClock - elapsed) / time.Second)
		ctx.EstimatedSecondsPerAttempt = 60
	}
	ctx.AuthorizedTools = make(map[string]map[string]bool)
	ctx.AllowedPaths = make(map[string][]string)
	for _, task := range revision.TaskDAG {
		if task.Agent == "" {
			return PlanValidationContext{}, fmt.Errorf("plan task requires an agent")
		}
		def, _, err := c.AgentPool().ResolveAgentName(task.Agent)
		if err != nil || def == nil {
			return PlanValidationContext{}, fmt.Errorf("plan task agent %q is not authorized: %v", task.Agent, err)
		}
		tools := make(map[string]bool)
		toolText := def.Tools
		if toolText == "" && session != nil {
			toolText = strings.Join(session.Config.ToolsAllowed, ",")
		}
		if toolText == "" {
			for _, tool := range c.coreTools {
				tools[tool.Info().Name] = true
			}
		}
		for _, name := range strings.FieldsFunc(toolText, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
			tools[name] = true
		}
		ctx.AuthorizedTools[task.Agent] = tools
		paths := append([]string(nil), def.AllowedPaths...)
		if len(paths) == 0 && session != nil {
			paths = append(paths, session.Config.AllowedPaths...)
		}
		if len(paths) > 0 {
			ctx.AllowedPaths[task.Agent] = paths
		}
	}
	return ctx, nil
}

// PlanRevisions returns detached revision snapshots; callers cannot mutate
// the coordinator's immutable audit history through this accessor.
func (c *Coordinator) PlanRevisions() []PlanRevision {
	if c == nil {
		return nil
	}
	c.planRevisionsMu.RLock()
	defer c.planRevisionsMu.RUnlock()
	result := make([]PlanRevision, len(c.planRevisions))
	for i, revision := range c.planRevisions {
		result[i] = clonePlanRevision(revision)
	}
	return result
}

// PlanTasksForExecution returns only tasks introduced or changed by a
// revision. Dependencies that point to unchanged tasks are removed only when
// their durable completion IDs are supplied; otherwise the revision is
// rejected rather than waiving the dependency.
func PlanTasksForExecution(revision PlanRevision) ([]TaskDef, error) {
	return planTasksForExecution(revision, nil)
}

func planTasksForExecution(revision PlanRevision, completed []string) ([]TaskDef, error) {
	for i, task := range revision.TaskDAG {
		for _, dep := range task.DependsOn {
			if dep < 0 || dep >= len(revision.TaskDAG) || dep == i {
				return nil, fmt.Errorf("task %d has invalid dependency %d", i, dep)
			}
		}
	}
	allowed := make(map[string]bool, len(revision.DAGDiff.Added)+len(revision.DAGDiff.Modified))
	for _, change := range revision.DAGDiff.Added {
		allowed[change.ID] = true
	}
	for _, change := range revision.DAGDiff.Modified {
		allowed[change.ID] = true
	}
	if len(revision.TaskDAG) > 0 && len(allowed) == 0 {
		return nil, fmt.Errorf("plan revision has no executable DAG diff")
	}
	selected := make([]TaskDef, 0, len(allowed))
	indexMap := make(map[int]int)
	for i, task := range revision.TaskDAG {
		if !allowed[taskRevisionID(task)] {
			continue
		}
		indexMap[i] = len(selected)
		selected = append(selected, cloneTaskDef(task))
	}
	for i := range selected {
		original := revision.TaskDAG[0]
		for source, target := range indexMap {
			if target == i {
				original = revision.TaskDAG[source]
				break
			}
		}
		deps := selected[i].DependsOn[:0]
		for _, dep := range original.DependsOn {
			if mapped, ok := indexMap[dep]; ok {
				deps = append(deps, mapped)
			} else if !planContainsString(completed, taskRevisionID(revision.TaskDAG[dep])) {
				return nil, fmt.Errorf("unfinished dependency %q for diff task %q", taskRevisionID(revision.TaskDAG[dep]), taskRevisionID(original))
			}
		}
		selected[i].DependsOn = deps
		if original.OnFailure != nil {
			target := *original.OnFailure
			if target < 0 || target >= len(revision.TaskDAG) {
				return nil, fmt.Errorf("task %q has invalid on_failure target %d", taskRevisionID(original), target)
			}
			mapped, ok := indexMap[target]
			if !ok {
				return nil, fmt.Errorf("on_failure target %q is omitted from diff task %q", taskRevisionID(revision.TaskDAG[target]), taskRevisionID(original))
			}
			selectedTarget := mapped
			selected[i].OnFailure = &selectedTarget
		}
	}
	return selected, nil
}

func planContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ExecutePlanRevision persists and validates a revision, then dispatches only
// its approved DAG diff. Callers must perform plan review before invoking it.
func (c *Coordinator) ExecutePlanRevision(ctx context.Context, revision PlanRevision) (string, error) {
	// Caller-provided CompletedTaskIDs and Review fields are deliberately
	// ignored. Both are audit inputs, not authorization.
	if err := normalizePlanTaskIDs(revision.TaskDAG); err != nil {
		return "", err
	}
	completed := coordinatorCompletedTaskIDs(c)
	revision.CompletedTaskIDs = append([]string(nil), completed...)
	tasks, err := planTasksForExecution(revision, completed)
	if err != nil {
		return "", err
	}
	digest := canonicalPlanRevisionDigest(revision)
	stored := false
	for _, candidate := range c.PlanRevisions() {
		if candidate.ID == revision.ID {
			if canonicalPlanRevisionDigest(candidate) != digest {
				return "", fmt.Errorf("execution revision does not match persisted canonical revision")
			}
			stored = true
			break
		}
	}
	if !stored {
		if err := c.PersistPlanRevision(revision); err != nil {
			return "", err
		}
	}
	if _, ok := c.trustedPlanReview(revision.ID, digest); !ok {
		return "", fmt.Errorf("trusted plan reviewer approval is required")
	}
	return c.ExecuteTasks(ctx, tasks)
}

func todoItemHasCompletedEvidence(item *TodoItem) bool {
	if item == nil || item.Status != TaskDone {
		return false
	}
	needsVerify := item.Verify != "" || item.VerifySpec != nil || item.Execution.RequiresVerification
	if !needsVerify {
		return true
	}
	return item.VerifyResult != nil && !item.VerifyResult.TimedOut && item.VerifyResult.ExitCode == 0
}

func coordinatorCompletedTaskIDs(c *Coordinator) []string {
	if c == nil || c.taskTracker == nil {
		return nil
	}
	var completed []string
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.PlanTaskID != "" && todoItemHasCompletedEvidence(item) {
			completed = append(completed, item.PlanTaskID)
		}
	}
	return completed
}
