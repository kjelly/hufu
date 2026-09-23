package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/improve"
	"github.com/kjelly/hufu/internal/promotion"
	"github.com/spf13/cobra"
)

var errHandoffEvidenceStale = errors.New("handoff canonical evidence is stale")

var (
	improveHandoffProject        string
	improveHandoffPolicy         string
	improveHandoffJSON           bool
	improveHandoffMemory         string
	improveHandoffConsolidate    string
	improveHandoffPromotion      string
	improveHandoffBaselineTeam   string
	improveHandoffBaselinePolicy string
	improveHandoffExperiment     string
	improveHandoffExpected       int64
	improveHandoffAdoption       string
	improveHandoffMonitoring     string
)

var improveHandoffCmd = &cobra.Command{
	Use:   "handoff",
	Short: "Create and inspect review-gated improvement handoffs",
}

var improveHandoffCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a handoff from one canonical proposal",
	Args:  cobra.NoArgs,
	RunE:  runImproveHandoffCreate,
}

var improveHandoffShowCmd = &cobra.Command{
	Use:   "show <handoff-id>",
	Short: "Show a durable improvement handoff",
	Args:  cobra.ExactArgs(1),
	RunE:  runImproveHandoffShow,
}

var improveHandoffPrepareCmd = &cobra.Command{
	Use:   "prepare <handoff-id>",
	Short: "Prepare an isolated candidate for a handoff",
	Args:  cobra.ExactArgs(1),
	RunE:  runImproveHandoffPrepare,
}

var improveHandoffEvaluateCmd = &cobra.Command{Use: "evaluate <handoff-id>", Short: "Bind an immutable experiment report to a handoff", Args: cobra.ExactArgs(1), RunE: runImproveHandoffEvaluate}
var improveHandoffApproveCmd = &cobra.Command{Use: "approve <handoff-id>", Short: "Record explicit handoff review approval", Args: cobra.ExactArgs(1), RunE: runImproveHandoffApprove}
var improveHandoffRejectCmd = &cobra.Command{Use: "reject <handoff-id>", Short: "Record explicit handoff review rejection", Args: cobra.ExactArgs(1), RunE: runImproveHandoffReject}
var improveHandoffAdoptCmd = &cobra.Command{Use: "adopt <handoff-id>", Short: "Link canonical adoption evidence to a handoff", Args: cobra.ExactArgs(1), RunE: runImproveHandoffAdopt}
var improveHandoffMonitorCmd = &cobra.Command{Use: "monitor <handoff-id>", Short: "Link monitoring evidence to an adopted handoff", Args: cobra.ExactArgs(1), RunE: runImproveHandoffMonitor}

func init() {
	improveCmd.AddCommand(improveHandoffCmd)
	improveHandoffCmd.AddCommand(improveHandoffCreateCmd, improveHandoffShowCmd, improveHandoffPrepareCmd, improveHandoffEvaluateCmd, improveHandoffApproveCmd, improveHandoffRejectCmd, improveHandoffAdoptCmd, improveHandoffMonitorCmd)
	flags := improveHandoffCmd.PersistentFlags()
	flags.StringVar(&improveHandoffProject, "project", "", "Project scope for the handoff (required)")
	flags.StringVar(&improveHandoffPolicy, "policy-version", "", "Optional memory policy version in the handoff scope")
	flags.BoolVar(&improveHandoffJSON, "json", false, "Print the handoff as JSON")
	improveHandoffPrepareCmd.Flags().StringVar(&improveHandoffBaselineTeam, "baseline-team", "", "Baseline team snapshot ID (required for skill preparation)")
	improveHandoffPrepareCmd.Flags().StringVar(&improveHandoffBaselinePolicy, "baseline-policy", "", "Baseline memory policy snapshot ID (required for memory policy preparation)")
	improveHandoffEvaluateCmd.Flags().StringVar(&improveHandoffExperiment, "experiment", "", "Experiment report ID (required)")
	_ = improveHandoffEvaluateCmd.MarkFlagRequired("experiment")
	for _, command := range []*cobra.Command{improveHandoffApproveCmd, improveHandoffRejectCmd, improveHandoffAdoptCmd, improveHandoffMonitorCmd} {
		command.Flags().Int64Var(&improveHandoffExpected, "expected-revision", 0, "Expected handoff revision (required)")
		_ = command.MarkFlagRequired("expected-revision")
	}
	improveHandoffAdoptCmd.Flags().StringVar(&improveHandoffAdoption, "adoption-ref", "", "Canonical adoption artifact ref kind:id:revision (required)")
	_ = improveHandoffAdoptCmd.MarkFlagRequired("adoption-ref")
	improveHandoffMonitorCmd.Flags().StringVar(&improveHandoffMonitoring, "monitoring-ref", "", "Monitoring report ref kind:id:revision (required)")
	_ = improveHandoffMonitorCmd.MarkFlagRequired("monitoring-ref")
	improveHandoffCreateCmd.Flags().StringVar(&improveHandoffMemory, "from-memory-policy", "", "Durable memory policy proposal ID")
	improveHandoffCreateCmd.Flags().StringVar(&improveHandoffConsolidate, "from-consolidation", "", "Canonical consolidation proposal ID")
	improveHandoffCreateCmd.Flags().StringVar(&improveHandoffPromotion, "from-promotion", "", "Canonical skill promotion proposal ID")
}

func runImproveHandoffCreate(cmd *cobra.Command, _ []string) error {
	scope, err := handoffScope()
	if err != nil {
		return err
	}
	ids := []string{improveHandoffMemory, improveHandoffConsolidate, improveHandoffPromotion}
	selected := 0
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			selected++
		}
	}
	if selected != 1 {
		return fmt.Errorf("exactly one --from-memory-policy, --from-consolidation, or --from-promotion is required")
	}
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	var handoff improve.ImprovementHandoff
	switch {
	case improveHandoffMemory != "":
		handoff, err = createMemoryPolicyHandoff(cmd.Context(), workspace, scope, improveHandoffMemory)
	case improveHandoffConsolidate != "":
		handoff, err = createConsolidationHandoff(cmd.Context(), workspace, scope, improveHandoffConsolidate)
	case improveHandoffPromotion != "":
		handoff, err = createSkillHandoff(cmd.Context(), workspace, scope, improveHandoffPromotion)
	}
	if err != nil {
		return err
	}
	created, err := improve.NewHandoffStore(workspace).Create(cmd.Context(), handoff)
	if err != nil {
		return err
	}
	return printHandoff(created)
}

func runImproveHandoffShow(cmd *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	handoff, err := improve.NewHandoffStore(workspace).Get(args[0])
	if err != nil {
		return err
	}
	if err := validateHandoffCommandScope(handoff); err != nil {
		return err
	}
	if err := validateCanonicalHandoffEvidence(cmd.Context(), workspace, handoff); err != nil {
		return err
	}
	return printHandoff(handoff)
}

func runImproveHandoffPrepare(cmd *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	store := improve.NewHandoffStore(workspace)
	handoff, err := store.Get(args[0])
	if err != nil {
		return err
	}
	if err := validateHandoffCommandScope(handoff); err != nil {
		return err
	}
	if err := ensureCanonicalHandoffEvidence(cmd.Context(), workspace, store, handoff); err != nil {
		return err
	}
	if handoff.Status == improve.HandoffCandidateReady {
		if handoff.Candidate == nil {
			return fmt.Errorf("handoff %q is candidate_ready without a candidate ref", handoff.ID)
		}
		if err := validatePreparedHandoffCandidate(cmd.Context(), workspace, handoff); err != nil {
			return err
		}
		if err := store.RetryAudit(cmd.Context(), handoff.ID); err != nil {
			return fmt.Errorf("reconcile prepared handoff audit: %w", err)
		}
		return printHandoff(handoff)
	}
	if handoff.Status != improve.HandoffProposed {
		return fmt.Errorf("handoff %q is %s; only proposed handoffs can be prepared", handoff.ID, handoff.Status)
	}
	ctx := cmd.Context()
	switch handoff.Kind {
	case improve.HandoffMemoryPolicy:
		return prepareMemoryPolicyHandoff(ctx, workspace, store, handoff)
	case improve.HandoffConsolidation:
		return prepareConsolidationHandoff(cmd, workspace, store, handoff)
	}
	if strings.TrimSpace(improveHandoffBaselineTeam) == "" {
		return fmt.Errorf("--baseline-team is required for skill preparation")
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		return fmt.Errorf("open context repository: %w", err)
	}
	defer func() { _ = repo.Close() }()
	proposal, err := repo.GetPromotion(ctx, handoff.Proposal.ID, handoff.Scope.ProjectID, handoff.Scope.TeamID)
	if err != nil {
		return fmt.Errorf("load skill promotion proposal: %w", err)
	}
	if proposal.Type != contextstore.PromotionTypeSkill {
		return fmt.Errorf("promotion proposal %q is not a skill proposal", proposal.ID)
	}
	if proposal.DraftHash != handoff.Proposal.Revision {
		return fmt.Errorf("skill promotion proposal revision changed")
	}
	if proposal.Status != contextstore.PromotionStatusProposed && proposal.Status != contextstore.PromotionStatusApproved {
		return fmt.Errorf("skill promotion proposal %q is %s and cannot prepare a candidate", proposal.ID, proposal.Status)
	}
	skillName := skillNameFromSkillTarget(proposal.TargetPath)
	if skillName == "" || proposal.TargetPath != promotion.TargetPathForSkill(skillName) {
		return fmt.Errorf("skill promotion target must be skills/<name>/SKILL.md")
	}
	if err := promotion.ValidateDraft(promotion.TypeSkill, proposal.Draft, skillName); err != nil {
		return fmt.Errorf("validate skill promotion draft: %w", err)
	}
	if err := samePromotionSources(handoff, proposal); err != nil {
		return err
	}
	if err := (promotion.Service{Repo: repo}).ValidateProposalEvidence(ctx, proposal); err != nil {
		return fmt.Errorf("skill promotion evidence is stale: %w", err)
	}
	baseline, _, err := improve.LoadBaselineSnapshot(workspace, improveHandoffBaselineTeam)
	if err != nil {
		return fmt.Errorf("load baseline team snapshot: %w", err)
	}
	if !strings.EqualFold(baseline.Team, handoff.Scope.TeamID) {
		return fmt.Errorf("baseline team %q does not match handoff team %q", baseline.Team, handoff.Scope.TeamID)
	}
	candidateID := "skill-candidate-" + digestRevision(struct {
		HandoffID string
		DraftHash string
		Baseline  string
	}{handoff.ID, proposal.DraftHash, baseline.ContentRevision})[:20]
	snapshot, _, err := improve.CreateSkillCandidateSnapshot(workspace, candidateID, improveHandoffBaselineTeam, skillName, proposal.Draft)
	if err != nil {
		return err
	}
	next := handoff
	next.Status = improve.HandoffCandidateReady
	next.Candidate = &improve.ArtifactRef{Kind: "team_snapshot", ID: snapshot.ID, Revision: snapshot.DefinitionRevision}
	next.StatusReason = improve.HandoffReasonSkillCandidatePrepared
	updated, err := store.Transition(ctx, handoff.ID, handoff.Revision, next)
	if err != nil {
		return err
	}
	return printHandoff(updated)
}

func prepareConsolidationHandoff(cmd *cobra.Command, workspace string, store *improve.HandoffStore, handoff improve.ImprovementHandoff) error {
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		return fmt.Errorf("open context repository: %w", err)
	}
	defer func() { _ = repo.Close() }()
	proposal, err := repo.GetConsolidationProposal(cmd.Context(), handoff.Proposal.ID)
	if err != nil {
		return fmt.Errorf("load consolidation proposal: %w", err)
	}
	if proposal.ProjectID != handoff.Scope.ProjectID || proposal.TeamID != handoff.Scope.TeamID || proposal.Status != "proposed" {
		return fmt.Errorf("consolidation proposal scope or status changed")
	}
	if err := validateConsolidationHandoffCurrent(cmd.Context(), repo, proposal, handoff.Scope.PolicyVersion); err != nil {
		return fmt.Errorf("consolidation evidence is stale: %w", err)
	}
	candidate, err := repo.Get(cmd.Context(), proposal.CandidateContextItemID)
	if err != nil {
		return fmt.Errorf("load consolidation candidate: %w", err)
	}
	if candidate.Lifecycle != contextstore.LifecycleCandidate || candidate.SupersededBy != "" || candidate.Source.Type != "consolidation_proposal" || candidate.Source.Ref != proposal.ID || candidate.Scope.ProjectID != handoff.Scope.ProjectID || candidate.Scope.TeamID != handoff.Scope.TeamID {
		return fmt.Errorf("consolidation candidate is no longer an unconfirmed candidate for this proposal")
	}
	next := handoff
	next.Status = improve.HandoffCandidateReady
	next.Candidate = &improve.ArtifactRef{Kind: "context_item", ID: candidate.ID, Revision: candidate.ContentHash}
	next.StatusReason = improve.HandoffReasonConsolidationCandidateReady
	updated, err := store.Transition(cmd.Context(), handoff.ID, handoff.Revision, next)
	if err != nil {
		return err
	}
	return printHandoff(updated)
}

func validateConsolidationHandoffCurrent(ctx context.Context, repo *contextstore.SQLiteRepository, proposal contextstore.ConsolidationProposal, policyVersion string) error {
	sources, err := repo.GetMany(ctx, proposal.SourceIDs)
	if err != nil {
		return err
	}
	if err := validateConsolidationSources(sources, proposal.ProjectID, proposal.TeamID); err != nil {
		return err
	}
	if strings.TrimSpace(policyVersion) == "" {
		policyVersion = "memory-policy-v1"
	}
	for _, source := range sources {
		if proposal.SourceRevisions[source.ID] != source.ContentHash {
			return fmt.Errorf("source %q content revision changed", source.ID)
		}
		aggregate, err := repo.ExperienceAggregate(ctx, source.ID, policyVersion)
		if err != nil || proposal.AggregateRevisions[source.ID] != aggregate.Revision {
			return fmt.Errorf("source %q aggregate revision changed", source.ID)
		}
	}
	return nil
}

func runImproveHandoffEvaluate(cmd *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	store := improve.NewHandoffStore(workspace)
	handoff, err := store.Get(args[0])
	if err != nil {
		return err
	}
	if err := validateHandoffCommandScope(handoff); err != nil {
		return err
	}
	if err := ensureCanonicalHandoffEvidence(cmd.Context(), workspace, store, handoff); err != nil {
		return err
	}
	if handoff.Status != improve.HandoffCandidateReady && handoff.Status != improve.HandoffBenchmarkBound && handoff.Status != improve.HandoffEvaluated && handoff.Status != improve.HandoffEligibleForReview {
		return fmt.Errorf("handoff %q is %s; evaluate requires a prepared candidate", handoff.ID, handoff.Status)
	}
	report, err := improve.LoadExperimentReport(workspace, improveHandoffExperiment)
	if err != nil {
		return err
	}
	if err := validateHandoffExperiment(handoff, report, workspace); err != nil {
		return err
	}
	benchmark := &improve.BenchmarkBinding{Ref: improve.ArtifactRef{Kind: "benchmark_fixture", ID: report.Benchmark.Name, Revision: report.Benchmark.Revision}, Name: report.Benchmark.Name, Category: report.Benchmark.Category, Cases: report.Benchmark.Cases}
	experimentRef := &improve.ArtifactRef{Kind: "experiment_report", ID: report.ID, Revision: improve.ExperimentReportRevision(report)}
	next := handoff
	if next.Status == improve.HandoffCandidateReady {
		next.Status = improve.HandoffBenchmarkBound
		next.Benchmark = benchmark
		next.StatusReason = improve.HandoffReasonBenchmarkBound
		next, err = store.Transition(cmd.Context(), handoff.ID, handoff.Revision, next)
		if err != nil {
			return err
		}
	}
	if next.Benchmark == nil || *next.Benchmark != *benchmark {
		return fmt.Errorf("experiment benchmark does not match existing handoff binding")
	}
	if next.Status == improve.HandoffBenchmarkBound {
		if err := store.RetryAudit(cmd.Context(), next.ID); err != nil {
			return fmt.Errorf("reconcile benchmark binding audit: %w", err)
		}
		next.Status = improve.HandoffEvaluated
		next.Experiment = experimentRef
		next.Evaluation = improve.EvaluationState{Decision: report.Decision, Status: report.Status, Report: experimentRef}
		next.StatusReason = improve.HandoffReasonExperimentEvaluated
		next, err = store.Transition(cmd.Context(), handoff.ID, next.Revision, next)
		if err != nil {
			return err
		}
	}
	if next.Experiment == nil || *next.Experiment != *experimentRef || next.Evaluation.Report == nil || *next.Evaluation.Report != *experimentRef || next.Evaluation.Decision != report.Decision || next.Evaluation.Status != report.Status {
		return fmt.Errorf("experiment report does not match existing handoff evaluation")
	}
	if next.Status == improve.HandoffEvaluated {
		if err := store.RetryAudit(cmd.Context(), next.ID); err != nil {
			return fmt.Errorf("reconcile evaluation audit: %w", err)
		}
	}
	if next.Status == improve.HandoffEvaluated && report.Decision == "eligible_for_review" {
		next.Status = improve.HandoffEligibleForReview
		next.StatusReason = improve.HandoffReasonExperimentEligible
		next, err = store.Transition(cmd.Context(), handoff.ID, next.Revision, next)
		if err != nil {
			return err
		}
	}
	if next.Status == improve.HandoffEligibleForReview {
		if err := store.RetryAudit(cmd.Context(), next.ID); err != nil {
			return fmt.Errorf("reconcile review eligibility audit: %w", err)
		}
	}
	return printHandoff(next)
}

func validateHandoffExperiment(handoff improve.ImprovementHandoff, report improve.ExperimentReport, workspace string) error {
	fixture, err := improve.LoadBenchmark(improve.BenchmarkPath(workspace, report.Benchmark.Name))
	if err != nil {
		return fmt.Errorf("load experiment benchmark: %w", err)
	}
	if !strings.EqualFold(fixture.Team, handoff.Scope.TeamID) || improve.BenchmarkRevision(fixture) != report.Benchmark.Revision {
		return fmt.Errorf("experiment benchmark does not match handoff scope or revision")
	}
	if handoff.Candidate == nil {
		return fmt.Errorf("handoff candidate is required")
	}
	switch handoff.Kind {
	case improve.HandoffMemoryPolicy:
		proposal, err := improve.LoadMemoryPolicyOptimizationProposal(workspace, handoff.Proposal.ID)
		if err != nil {
			return err
		}
		if handoff.Candidate.ID != proposal.Candidate.ID || handoff.Candidate.Revision != proposal.Candidate.Revision {
			return fmt.Errorf("handoff memory candidate does not match proposal")
		}
		return improve.ValidateMemoryPolicyExperimentReport(report, proposal.BasePolicy, proposal.Candidate)
	case improve.HandoffSkill:
		candidate, _, err := improve.LoadCandidateSnapshot(workspace, handoff.Candidate.ID)
		if err != nil {
			return err
		}
		if report.Candidate.SnapshotID != candidate.ID || report.Candidate.DefinitionRevision != candidate.DefinitionRevision || report.Candidate.ContentRevision != candidate.ContentRevision {
			return fmt.Errorf("experiment report candidate does not match handoff candidate")
		}
	case improve.HandoffConsolidation:
		return improve.ValidateContextCandidateExperimentReport(report, *handoff.Candidate)
	}
	return nil
}

func runImproveHandoffApprove(cmd *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	store := improve.NewHandoffStore(workspace)
	handoff, err := store.Get(args[0])
	if err != nil {
		return err
	}
	if err := validateHandoffCommandScope(handoff); err != nil {
		return err
	}
	if err := ensureCanonicalHandoffEvidence(cmd.Context(), workspace, store, handoff); err != nil {
		return err
	}
	if handoff.Status == improve.HandoffApproved {
		if improveHandoffExpected != handoff.Revision-1 {
			return fmt.Errorf("handoff %q revision conflict: retry expected %d, got %d", handoff.ID, handoff.Revision-1, improveHandoffExpected)
		}
		if err := store.RetryAudit(cmd.Context(), handoff.ID); err != nil {
			return fmt.Errorf("reconcile approval audit: %w", err)
		}
		return printHandoff(handoff)
	}
	if handoff.Status != improve.HandoffEligibleForReview || handoff.Evaluation.Decision != "eligible_for_review" {
		return fmt.Errorf("handoff %q is not eligible for explicit approval", handoff.ID)
	}
	next := handoff
	next.Status = improve.HandoffApproved
	next.StatusReason = improve.HandoffReasonReviewApproved
	approved, err := store.Transition(cmd.Context(), handoff.ID, improveHandoffExpected, next)
	if err != nil {
		return err
	}
	return printHandoff(approved)
}

func runImproveHandoffReject(cmd *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	store := improve.NewHandoffStore(workspace)
	handoff, err := store.Get(args[0])
	if err != nil {
		return err
	}
	if err := validateHandoffCommandScope(handoff); err != nil {
		return err
	}
	if err := ensureCanonicalHandoffEvidence(cmd.Context(), workspace, store, handoff); err != nil {
		return err
	}
	if handoff.Status == improve.HandoffRejected {
		if improveHandoffExpected != handoff.Revision-1 {
			return fmt.Errorf("handoff %q revision conflict: retry expected %d, got %d", handoff.ID, handoff.Revision-1, improveHandoffExpected)
		}
		if err := store.RetryAudit(cmd.Context(), handoff.ID); err != nil {
			return fmt.Errorf("reconcile rejection audit: %w", err)
		}
		return printHandoff(handoff)
	}
	if handoff.Status != improve.HandoffEvaluated && handoff.Status != improve.HandoffEligibleForReview {
		return fmt.Errorf("handoff %q is %s; only evaluated handoffs can be rejected", handoff.ID, handoff.Status)
	}
	next := handoff
	next.Status = improve.HandoffRejected
	next.StatusReason = improve.HandoffReasonReviewRejected
	rejected, err := store.Transition(cmd.Context(), handoff.ID, improveHandoffExpected, next)
	if err != nil {
		return err
	}
	return printHandoff(rejected)
}

func runImproveHandoffAdopt(cmd *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	store := improve.NewHandoffStore(workspace)
	handoff, err := store.Get(args[0])
	if err != nil {
		return err
	}
	if err := validateHandoffCommandScope(handoff); err != nil {
		return err
	}
	if err := ensureCanonicalHandoffEvidence(cmd.Context(), workspace, store, handoff); err != nil {
		return err
	}
	adoption, err := parseHandoffRef(improveHandoffAdoption)
	if err != nil {
		return err
	}
	if err := validateHandoffAdoption(cmd.Context(), workspace, handoff, adoption); err != nil {
		return err
	}
	if handoff.Status == improve.HandoffAdopted {
		if handoff.Adoption == nil || *handoff.Adoption != adoption || improveHandoffExpected != handoff.Revision-1 {
			return fmt.Errorf("handoff %q adoption retry does not match durable state", handoff.ID)
		}
		if err := store.RetryAudit(cmd.Context(), handoff.ID); err != nil {
			return fmt.Errorf("reconcile adoption audit: %w", err)
		}
		return printHandoff(handoff)
	}
	if handoff.Status != improve.HandoffApproved {
		return fmt.Errorf("handoff %q is %s; approval is required before adoption", handoff.ID, handoff.Status)
	}
	next := handoff
	next.Status = improve.HandoffAdopted
	next.Adoption = &adoption
	next.StatusReason = improve.HandoffReasonAdoptionLinked
	adopted, err := store.Transition(cmd.Context(), handoff.ID, improveHandoffExpected, next)
	if err != nil {
		return err
	}
	return printHandoff(adopted)
}

func runImproveHandoffMonitor(cmd *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	store := improve.NewHandoffStore(workspace)
	handoff, err := store.Get(args[0])
	if err != nil {
		return err
	}
	if err := validateHandoffCommandScope(handoff); err != nil {
		return err
	}
	if err := ensureCanonicalHandoffEvidence(cmd.Context(), workspace, store, handoff); err != nil {
		return err
	}
	if handoff.Status != improve.HandoffAdopted && handoff.Status != improve.HandoffMonitoring && handoff.Status != improve.HandoffRollbackRecommended {
		return fmt.Errorf("handoff %q is %s; monitoring requires adoption", handoff.ID, handoff.Status)
	}
	monitoring, err := parseHandoffRef(improveHandoffMonitoring)
	if err != nil {
		return err
	}
	if monitoring.Kind != "monitoring_report" {
		return fmt.Errorf("monitoring ref must have kind monitoring_report")
	}
	report, err := improve.LoadMonitoringReport(workspace, monitoring.ID)
	if err != nil {
		return err
	}
	if handoff.Adoption == nil || handoff.Candidate == nil || report.AdoptionID != handoff.Adoption.ID || !strings.EqualFold(report.Team, handoff.Scope.TeamID) || report.ExpectedRevision != handoff.Candidate.Revision || monitoring.Revision != improve.MonitoringReportRevision(report) {
		return fmt.Errorf("monitoring report does not match handoff adoption or revision")
	}
	if err := validateHandoffMonitoringEvidence(workspace, handoff, report); err != nil {
		return err
	}
	expectedRevision := improveHandoffExpected
	if slices.Contains(handoff.Monitoring, monitoring) {
		retryRevision := handoff.Revision - 1
		if handoff.Status == improve.HandoffRollbackRecommended && report.RollbackSuggestion != nil {
			retryRevision--
		}
		if improveHandoffExpected != retryRevision {
			return fmt.Errorf("handoff %q monitoring retry does not match durable revision", handoff.ID)
		}
		if err := store.RetryAudit(cmd.Context(), handoff.ID); err != nil {
			return fmt.Errorf("reconcile monitoring audit: %w", err)
		}
		if handoff.Status == improve.HandoffRollbackRecommended || report.RollbackSuggestion == nil {
			return printHandoff(handoff)
		}
		expectedRevision = handoff.Revision
	} else if handoff.Status == improve.HandoffRollbackRecommended {
		return fmt.Errorf("handoff %q already recommends rollback and cannot accept new monitoring evidence", handoff.ID)
	}
	next := handoff
	if !slices.Contains(next.Monitoring, monitoring) {
		next.Status = improve.HandoffMonitoring
		next.Monitoring = append(slices.Clone(handoff.Monitoring), monitoring)
		next.StatusReason = improve.HandoffReasonMonitoringLinked
		next, err = store.Transition(cmd.Context(), handoff.ID, expectedRevision, next)
		if err != nil {
			return err
		}
	}
	if report.RollbackSuggestion != nil {
		next.Status = improve.HandoffRollbackRecommended
		next.StatusReason = improve.HandoffReasonRollbackRecommended
		next, err = store.Transition(cmd.Context(), handoff.ID, next.Revision, next)
		if err != nil {
			return err
		}
	}
	return printHandoff(next)
}

func parseHandoffRef(value string) (improve.ArtifactRef, error) {
	parts := strings.SplitN(strings.TrimSpace(value), ":", 3)
	if len(parts) != 3 {
		return improve.ArtifactRef{}, fmt.Errorf("artifact ref must use kind:id:revision")
	}
	ref := improve.ArtifactRef{Kind: parts[0], ID: parts[1], Revision: parts[2]}
	if err := improve.ValidateArtifactRef(ref); err != nil {
		return improve.ArtifactRef{}, err
	}
	return ref, nil
}

func validateHandoffAdoption(ctx context.Context, workspace string, handoff improve.ImprovementHandoff, adoption improve.ArtifactRef) error {
	if handoff.Candidate == nil {
		return fmt.Errorf("handoff candidate is required before adoption")
	}
	switch handoff.Kind {
	case improve.HandoffSkill:
		if adoption.Kind != "improve_adoption" {
			return fmt.Errorf("skill adoption requires an improve_adoption ref")
		}
		record, err := improve.LoadAdoption(workspace, adoption.ID)
		if err != nil {
			return err
		}
		if handoff.Experiment == nil || record.ExperimentID != handoff.Experiment.ID || record.Team != handoff.Scope.TeamID || record.CandidateSnapshotID != handoff.Candidate.ID || record.CandidateRevision != handoff.Candidate.Revision || adoption.Revision != improve.AdoptionRevision(record) {
			return fmt.Errorf("adoption does not match handoff candidate")
		}
	case improve.HandoffMemoryPolicy:
		if adoption.Kind != "memory_policy_activation" || adoption.ID != handoff.Candidate.ID || adoption.Revision != handoff.Candidate.Revision {
			return fmt.Errorf("memory adoption must reference the matching activated policy")
		}
		repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
		if err != nil {
			return err
		}
		defer func() { _ = repo.Close() }()
		active, err := repo.ActiveMemoryPolicyVersion(ctx)
		if err != nil {
			return err
		}
		if active.PolicyVersion != adoption.ID || active.RevisionHash != adoption.Revision || active.Status != "active" {
			return fmt.Errorf("memory policy %q is not active", adoption.ID)
		}
	case improve.HandoffConsolidation:
		if adoption.Kind != "context_consolidation_approval" {
			return fmt.Errorf("consolidation adoption requires a context_consolidation_approval ref")
		}
		repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
		if err != nil {
			return err
		}
		defer func() { _ = repo.Close() }()
		proposal, err := repo.GetConsolidationProposal(ctx, adoption.ID)
		if err != nil || proposal.Status != "approved" || proposal.CandidateContextItemID != handoff.Candidate.ID || adoption.Revision != handoff.Candidate.Revision {
			return fmt.Errorf("consolidation approval does not match handoff candidate")
		}
		candidate, err := repo.Get(ctx, proposal.CandidateContextItemID)
		if err != nil || candidate.Lifecycle != contextstore.LifecycleConfirmed || candidate.ContentHash != adoption.Revision {
			return fmt.Errorf("consolidation candidate is not canonically confirmed")
		}
	}
	return nil
}

func validateHandoffMonitoringEvidence(workspace string, handoff improve.ImprovementHandoff, report improve.MonitoringReport) error {
	if handoff.Experiment == nil || handoff.Adoption == nil {
		return fmt.Errorf("monitoring requires experiment and adoption evidence")
	}
	experiment, err := improve.LoadExperimentReport(workspace, handoff.Experiment.ID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(report.BaselineMetrics, experiment.Baseline.Metrics) {
		return fmt.Errorf("monitoring baseline metrics do not match handoff experiment")
	}
	if report.RollbackSuggestion == nil {
		return nil
	}
	wantBaselineID := experiment.Baseline.SnapshotID
	wantRollbackRevision := experiment.Baseline.DefinitionRevision
	switch handoff.Kind {
	case improve.HandoffSkill:
		adoption, err := improve.LoadAdoption(workspace, handoff.Adoption.ID)
		if err != nil {
			return err
		}
		wantBaselineID = adoption.BaselineSnapshotID
		wantRollbackRevision = adoption.RollbackRevision
	case improve.HandoffMemoryPolicy:
		if experiment.Baseline.MemoryPolicy == nil {
			return fmt.Errorf("memory policy monitoring requires a baseline policy ref")
		}
		wantBaselineID = experiment.Baseline.MemoryPolicy.ID
		wantRollbackRevision = experiment.Baseline.MemoryPolicy.Revision
	}
	if report.RollbackSuggestion.BaselineSnapshotID != wantBaselineID || report.RollbackSuggestion.RollbackRevision != wantRollbackRevision {
		return fmt.Errorf("monitoring rollback suggestion does not match canonical baseline")
	}
	return nil
}

func prepareMemoryPolicyHandoff(ctx context.Context, workspace string, store *improve.HandoffStore, handoff improve.ImprovementHandoff) error {
	if strings.TrimSpace(improveHandoffBaselinePolicy) == "" {
		return fmt.Errorf("--baseline-policy is required for memory policy preparation")
	}
	proposal, err := improve.LoadMemoryPolicyOptimizationProposal(workspace, handoff.Proposal.ID)
	if err != nil {
		return fmt.Errorf("load memory policy proposal: %w", err)
	}
	if proposal.RevisionHash != handoff.Proposal.Revision {
		return fmt.Errorf("memory policy proposal revision changed")
	}
	if proposal.BasePolicy.ID != improveHandoffBaselinePolicy {
		return fmt.Errorf("baseline policy %q does not match proposal base %q", improveHandoffBaselinePolicy, proposal.BasePolicy.ID)
	}
	base, err := improve.LoadMemoryPolicySnapshot(workspace, proposal.BasePolicy.ID)
	if err != nil {
		return fmt.Errorf("load baseline memory policy: %w", err)
	}
	candidate, err := improve.LoadMemoryPolicySnapshot(workspace, proposal.Candidate.ID)
	if err != nil {
		return fmt.Errorf("load candidate memory policy: %w", err)
	}
	if base.RevisionHash != proposal.BasePolicy.Revision || candidate.RevisionHash != proposal.Candidate.Revision || candidate.Status != "candidate" || candidate.PreviousID != base.ID {
		return fmt.Errorf("memory policy proposal snapshot bindings are stale")
	}
	next := handoff
	next.Status = improve.HandoffCandidateReady
	next.Candidate = &improve.ArtifactRef{Kind: "memory_policy_snapshot", ID: candidate.ID, Revision: candidate.RevisionHash}
	next.StatusReason = improve.HandoffReasonMemoryPolicyCandidateReady
	updated, err := store.Transition(ctx, handoff.ID, handoff.Revision, next)
	if err != nil {
		return err
	}
	return printHandoff(updated)
}

func handoffScope() (improve.HandoffScope, error) {
	project := strings.TrimSpace(improveHandoffProject)
	team := strings.TrimSpace(improveTeam)
	if project == "" || team == "" {
		return improve.HandoffScope{}, fmt.Errorf("--project and --team are required")
	}
	return improve.HandoffScope{ProjectID: project, TeamID: team, PolicyVersion: strings.TrimSpace(improveHandoffPolicy)}, nil
}

func validateHandoffCommandScope(handoff improve.ImprovementHandoff) error {
	scope, err := handoffScope()
	if err != nil {
		return err
	}
	if scope != handoff.Scope {
		return fmt.Errorf("command scope does not match handoff scope")
	}
	return nil
}

func validatePreparedHandoffCandidate(ctx context.Context, workspace string, handoff improve.ImprovementHandoff) error {
	if handoff.Candidate == nil {
		return fmt.Errorf("handoff %q has no candidate", handoff.ID)
	}
	switch handoff.Kind {
	case improve.HandoffSkill:
		candidate, _, err := improve.LoadCandidateSnapshot(workspace, handoff.Candidate.ID)
		if err != nil {
			return fmt.Errorf("load existing skill candidate: %w", err)
		}
		if candidate.DefinitionRevision != handoff.Candidate.Revision {
			return fmt.Errorf("skill candidate revision changed")
		}
	case improve.HandoffMemoryPolicy:
		candidate, err := improve.LoadMemoryPolicySnapshot(workspace, handoff.Candidate.ID)
		if err != nil {
			return err
		}
		if candidate.RevisionHash != handoff.Candidate.Revision || candidate.Status != "candidate" {
			return fmt.Errorf("memory policy candidate revision or status changed")
		}
	case improve.HandoffConsolidation:
		repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
		if err != nil {
			return err
		}
		defer func() { _ = repo.Close() }()
		candidate, err := repo.Get(ctx, handoff.Candidate.ID)
		if err != nil {
			return err
		}
		if candidate.ContentHash != handoff.Candidate.Revision || candidate.Lifecycle != contextstore.LifecycleCandidate || candidate.SupersededBy != "" {
			return fmt.Errorf("consolidation candidate revision or lifecycle changed")
		}
	}
	return nil
}

func validateCanonicalHandoffEvidence(ctx context.Context, workspace string, handoff improve.ImprovementHandoff) error {
	if handoff.Status == improve.HandoffStale {
		return nil
	}
	if err := validateCanonicalHandoffProposal(ctx, workspace, handoff); err != nil {
		return err
	}
	return validateCanonicalHandoffBindings(ctx, workspace, handoff)
}

func validateCanonicalHandoffProposal(ctx context.Context, workspace string, handoff improve.ImprovementHandoff) error {
	switch handoff.Kind {
	case improve.HandoffSkill:
		repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
		if err != nil {
			return err
		}
		defer func() { _ = repo.Close() }()
		proposal, err := repo.GetPromotion(ctx, handoff.Proposal.ID, handoff.Scope.ProjectID, handoff.Scope.TeamID)
		if err != nil {
			return canonicalArtifactError("skill promotion proposal", err)
		}
		if proposal.DraftHash != handoff.Proposal.Revision || proposal.Type != contextstore.PromotionTypeSkill || proposal.Status == contextstore.PromotionStatusRejected || proposal.Status == contextstore.PromotionStatusStale {
			return staleHandoffEvidencef("skill promotion proposal changed")
		}
		if err := samePromotionSources(handoff, proposal); err != nil {
			return staleHandoffEvidencef("%v", err)
		}
		if err := (promotion.Service{Repo: repo}).ValidateProposalEvidence(ctx, proposal); err != nil {
			return staleHandoffEvidencef("skill promotion evidence changed: %v", err)
		}
	case improve.HandoffMemoryPolicy:
		proposal, err := improve.LoadMemoryPolicyOptimizationProposal(workspace, handoff.Proposal.ID)
		if err != nil {
			return canonicalArtifactError("memory policy proposal", err)
		}
		if proposal.RevisionHash != handoff.Proposal.Revision {
			return staleHandoffEvidencef("memory policy proposal changed")
		}
	case improve.HandoffConsolidation:
		repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
		if err != nil {
			return err
		}
		defer func() { _ = repo.Close() }()
		proposal, err := repo.GetConsolidationProposal(ctx, handoff.Proposal.ID)
		if err != nil {
			return canonicalArtifactError("consolidation proposal", err)
		}
		if proposal.ProjectID != handoff.Scope.ProjectID || proposal.TeamID != handoff.Scope.TeamID || consolidationProposalRevision(proposal) != handoff.Proposal.Revision || proposal.Status == "rejected" || proposal.Status == "failed" {
			return staleHandoffEvidencef("consolidation proposal changed")
		}
		if !handoffHasAdoption(handoff.Status) {
			if err := validateConsolidationHandoffCurrent(ctx, repo, proposal, handoff.Scope.PolicyVersion); err != nil {
				return staleHandoffEvidencef("consolidation source changed: %v", err)
			}
		}
	}
	return nil
}

func validateCanonicalHandoffBindings(ctx context.Context, workspace string, handoff improve.ImprovementHandoff) error {
	if handoff.Candidate != nil {
		if err := validateCanonicalCandidate(ctx, workspace, handoff); err != nil {
			return err
		}
	}
	if handoff.Benchmark != nil {
		fixture, err := improve.LoadBenchmark(improve.BenchmarkPath(workspace, handoff.Benchmark.Name))
		if err != nil {
			return canonicalArtifactError("benchmark fixture", err)
		}
		if improve.BenchmarkRevision(fixture) != handoff.Benchmark.Ref.Revision || !strings.EqualFold(fixture.Team, handoff.Scope.TeamID) || fixture.Category != handoff.Benchmark.Category || len(fixture.Cases) != handoff.Benchmark.Cases {
			return staleHandoffEvidencef("benchmark binding changed")
		}
	}
	if handoff.Experiment != nil {
		report, err := improve.LoadExperimentReport(workspace, handoff.Experiment.ID)
		if err != nil {
			return canonicalArtifactError("experiment report", err)
		}
		if improve.ExperimentReportRevision(report) != handoff.Experiment.Revision {
			return staleHandoffEvidencef("experiment report changed")
		}
		if err := validateHandoffExperiment(handoff, report, workspace); err != nil {
			return staleHandoffEvidencef("experiment evidence changed: %v", err)
		}
	}
	if handoff.Adoption != nil {
		if err := validateHandoffAdoption(ctx, workspace, handoff, *handoff.Adoption); err != nil {
			return staleHandoffEvidencef("adoption evidence changed: %v", err)
		}
	}
	for _, ref := range handoff.Monitoring {
		report, err := improve.LoadMonitoringReport(workspace, ref.ID)
		if err != nil {
			return canonicalArtifactError("monitoring report", err)
		}
		if ref.Revision != improve.MonitoringReportRevision(report) || handoff.Adoption == nil || handoff.Candidate == nil || report.AdoptionID != handoff.Adoption.ID || report.ExpectedRevision != handoff.Candidate.Revision || !strings.EqualFold(report.Team, handoff.Scope.TeamID) {
			return staleHandoffEvidencef("monitoring evidence changed")
		}
		if err := validateHandoffMonitoringEvidence(workspace, handoff, report); err != nil {
			return staleHandoffEvidencef("monitoring evidence changed: %v", err)
		}
	}
	return nil
}

func validateCanonicalCandidate(ctx context.Context, workspace string, handoff improve.ImprovementHandoff) error {
	if handoff.Candidate == nil {
		return nil
	}
	switch handoff.Kind {
	case improve.HandoffSkill:
		candidate, _, err := improve.LoadCandidateSnapshot(workspace, handoff.Candidate.ID)
		if err != nil {
			return canonicalArtifactError("skill candidate", err)
		}
		if candidate.DefinitionRevision != handoff.Candidate.Revision || !strings.EqualFold(candidate.Team, handoff.Scope.TeamID) {
			return staleHandoffEvidencef("skill candidate changed")
		}
	case improve.HandoffMemoryPolicy:
		candidate, err := improve.LoadMemoryPolicySnapshot(workspace, handoff.Candidate.ID)
		if err != nil {
			return canonicalArtifactError("memory policy candidate", err)
		}
		if candidate.RevisionHash != handoff.Candidate.Revision || candidate.Status == "rejected" {
			return staleHandoffEvidencef("memory policy candidate changed")
		}
	case improve.HandoffConsolidation:
		repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
		if err != nil {
			return err
		}
		defer func() { _ = repo.Close() }()
		candidate, err := repo.Get(ctx, handoff.Candidate.ID)
		if err != nil {
			return canonicalArtifactError("consolidation candidate", err)
		}
		wantLifecycle := contextstore.LifecycleCandidate
		if handoffHasAdoption(handoff.Status) {
			wantLifecycle = contextstore.LifecycleConfirmed
		}
		lifecycleMatches := candidate.Lifecycle == wantLifecycle
		if handoff.Status == improve.HandoffApproved {
			lifecycleMatches = candidate.Lifecycle == contextstore.LifecycleCandidate || candidate.Lifecycle == contextstore.LifecycleConfirmed
		}
		if candidate.ContentHash != handoff.Candidate.Revision || !lifecycleMatches || candidate.Scope.ProjectID != handoff.Scope.ProjectID || candidate.Scope.TeamID != handoff.Scope.TeamID {
			return staleHandoffEvidencef("consolidation candidate changed")
		}
	}
	return nil
}

func ensureCanonicalHandoffEvidence(ctx context.Context, workspace string, store *improve.HandoffStore, handoff improve.ImprovementHandoff) error {
	if handoff.Status == improve.HandoffStale {
		if err := store.RetryAudit(ctx, handoff.ID); err != nil {
			return errors.Join(errHandoffEvidenceStale, err)
		}
		return errHandoffEvidenceStale
	}
	err := validateCanonicalHandoffEvidence(ctx, workspace, handoff)
	if err == nil || !errors.Is(err, errHandoffEvidenceStale) {
		return err
	}
	if handoff.Status == improve.HandoffProposed || handoff.Status == improve.HandoffCandidateReady || handoff.Status == improve.HandoffBenchmarkBound || handoff.Status == improve.HandoffEvaluated || handoff.Status == improve.HandoffEligibleForReview || handoff.Status == improve.HandoffApproved {
		next := handoff
		next.Status = improve.HandoffStale
		next.StatusReason = improve.HandoffReasonCanonicalEvidenceStale
		_, transitionErr := store.Transition(ctx, handoff.ID, handoff.Revision, next)
		return errors.Join(err, transitionErr)
	}
	return err
}

func staleHandoffEvidencef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errHandoffEvidenceStale, fmt.Sprintf(format, args...))
}

func canonicalArtifactError(label string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("load %s: %w", label, err)
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy") {
		return fmt.Errorf("load %s: %w", label, err)
	}
	return staleHandoffEvidencef("%s is missing or invalid: %v", label, err)
}

func handoffHasAdoption(status improve.HandoffStatus) bool {
	return status == improve.HandoffAdopted || status == improve.HandoffMonitoring || status == improve.HandoffRollbackRecommended
}

func createMemoryPolicyHandoff(ctx context.Context, workspace string, scope improve.HandoffScope, id string) (improve.ImprovementHandoff, error) {
	proposal, err := improve.LoadMemoryPolicyOptimizationProposal(workspace, id)
	if err != nil {
		return improve.ImprovementHandoff{}, fmt.Errorf("load memory policy proposal: %w", err)
	}
	return improve.NewImprovementHandoff(improve.HandoffMemoryPolicy, scope, improve.ArtifactRef{Kind: "memory_policy_proposal", ID: proposal.ID, Revision: proposal.RevisionHash}, nil)
}

func createConsolidationHandoff(ctx context.Context, workspace string, scope improve.HandoffScope, id string) (improve.ImprovementHandoff, error) {
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		return improve.ImprovementHandoff{}, fmt.Errorf("open context repository: %w", err)
	}
	defer func() { _ = repo.Close() }()
	proposal, err := repo.GetConsolidationProposal(ctx, id)
	if err != nil {
		return improve.ImprovementHandoff{}, fmt.Errorf("load consolidation proposal: %w", err)
	}
	if proposal.ProjectID != scope.ProjectID || proposal.TeamID != scope.TeamID {
		return improve.ImprovementHandoff{}, fmt.Errorf("consolidation proposal scope does not match handoff scope")
	}
	if proposal.Status != "proposed" {
		return improve.ImprovementHandoff{}, fmt.Errorf("consolidation proposal %q is %s", proposal.ID, proposal.Status)
	}
	if err := validateConsolidationHandoffCurrent(ctx, repo, proposal, scope.PolicyVersion); err != nil {
		return improve.ImprovementHandoff{}, fmt.Errorf("consolidation evidence is stale: %w", err)
	}
	candidate, err := repo.Get(ctx, proposal.CandidateContextItemID)
	if err != nil {
		return improve.ImprovementHandoff{}, fmt.Errorf("load consolidation candidate: %w", err)
	}
	if candidate.Lifecycle != contextstore.LifecycleCandidate || candidate.SupersededBy != "" || candidate.Source.Type != "consolidation_proposal" || candidate.Source.Ref != proposal.ID || candidate.Scope.ProjectID != scope.ProjectID || candidate.Scope.TeamID != scope.TeamID {
		return improve.ImprovementHandoff{}, fmt.Errorf("consolidation candidate is not an unconfirmed candidate for this proposal")
	}
	sources := make([]improve.SourceBinding, 0, len(proposal.SourceIDs))
	for _, sourceID := range proposal.SourceIDs {
		hash := strings.TrimSpace(proposal.SourceRevisions[sourceID])
		if hash == "" {
			return improve.ImprovementHandoff{}, fmt.Errorf("consolidation proposal source %q has no content revision", sourceID)
		}
		aggregate := proposal.AggregateRevisions[sourceID]
		sources = append(sources, improve.SourceBinding{Ref: improve.ArtifactRef{Kind: "context_item", ID: sourceID, Revision: hash}, ContentHash: hash, AggregateRevision: aggregate, ProjectID: scope.ProjectID, TeamID: scope.TeamID})
	}
	revision := consolidationProposalRevision(proposal)
	return improve.NewImprovementHandoff(improve.HandoffConsolidation, scope, improve.ArtifactRef{Kind: "consolidation_proposal", ID: proposal.ID, Revision: revision}, sources)
}

func consolidationProposalRevision(proposal contextstore.ConsolidationProposal) string {
	return digestRevision(struct {
		ID         string
		Sources    map[string]string
		Aggregates map[string]int64
		Candidate  string
	}{proposal.ID, proposal.SourceRevisions, proposal.AggregateRevisions, proposal.CandidateContextItemID})
}

func createSkillHandoff(ctx context.Context, workspace string, scope improve.HandoffScope, id string) (improve.ImprovementHandoff, error) {
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		return improve.ImprovementHandoff{}, fmt.Errorf("open context repository: %w", err)
	}
	defer func() { _ = repo.Close() }()
	proposal, err := repo.GetPromotion(ctx, id, scope.ProjectID, scope.TeamID)
	if err != nil {
		return improve.ImprovementHandoff{}, fmt.Errorf("load promotion proposal: %w", err)
	}
	if proposal.Type != contextstore.PromotionTypeSkill {
		return improve.ImprovementHandoff{}, fmt.Errorf("promotion proposal %q is %q, only skill proposals can be handed off", id, proposal.Type)
	}
	if proposal.Status != contextstore.PromotionStatusProposed && proposal.Status != contextstore.PromotionStatusApproved {
		return improve.ImprovementHandoff{}, fmt.Errorf("promotion proposal %q is %s and cannot create a handoff", proposal.ID, proposal.Status)
	}
	skillName := skillNameFromSkillTarget(proposal.TargetPath)
	if skillName == "" || proposal.TargetPath != promotion.TargetPathForSkill(skillName) {
		return improve.ImprovementHandoff{}, fmt.Errorf("skill promotion target must be skills/<name>/SKILL.md")
	}
	if err := (promotion.Service{Repo: repo}).ValidateProposalEvidence(ctx, proposal); err != nil {
		return improve.ImprovementHandoff{}, fmt.Errorf("skill promotion evidence is stale: %w", err)
	}
	sources := make([]improve.SourceBinding, 0, len(proposal.Sources))
	for _, source := range proposal.Sources {
		sources = append(sources, improve.SourceBinding{Ref: improve.ArtifactRef{Kind: "context_item", ID: source.ContextItemID, Revision: source.ContentHash}, ContentHash: source.ContentHash, AggregateRevision: source.AggregateRevision, ProjectID: scope.ProjectID, TeamID: scope.TeamID})
	}
	return improve.NewImprovementHandoff(improve.HandoffSkill, scope, improve.ArtifactRef{Kind: "promotion_proposal", ID: proposal.ID, Revision: proposal.DraftHash}, sources)
}

func samePromotionSources(handoff improve.ImprovementHandoff, proposal contextstore.PromotionProposal) error {
	got := make([]improve.SourceBinding, 0, len(proposal.Sources))
	for _, source := range proposal.Sources {
		got = append(got, improve.SourceBinding{
			Ref:         improve.ArtifactRef{Kind: "context_item", ID: source.ContextItemID, Revision: source.ContentHash},
			ContentHash: source.ContentHash, AggregateRevision: source.AggregateRevision,
			ProjectID: proposal.ProjectID, TeamID: proposal.TeamID,
		})
	}
	slices.SortFunc(got, func(left, right improve.SourceBinding) int { return cmp.Compare(left.Ref.ID, right.Ref.ID) })
	if !reflect.DeepEqual(handoff.Sources, got) {
		return fmt.Errorf("skill promotion source bindings changed")
	}
	return nil
}

func skillNameFromSkillTarget(target string) string {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(target)), "/")
	if len(parts) != 3 || parts[0] != "skills" || parts[2] != "SKILL.md" {
		return ""
	}
	return parts[1]
}

func digestRevision(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func printHandoff(handoff improve.ImprovementHandoff) error {
	if improveHandoffJSON {
		data, err := json.MarshalIndent(handoff, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("%s\nkind: %s\nstatus: %s\nrevision: %d\n", handoff.ID, handoff.Kind, handoff.Status, handoff.Revision)
	return nil
}
