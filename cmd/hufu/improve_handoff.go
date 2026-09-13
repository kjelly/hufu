package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/improve"
	"github.com/kjelly/hufu/internal/promotion"
	"github.com/spf13/cobra"
)

var (
	improveHandoffProject        string
	improveHandoffPolicy         string
	improveHandoffTeamSearch     string
	improveHandoffJSON           bool
	improveHandoffMemory         string
	improveHandoffConsolidate    string
	improveHandoffPromotion      string
	improveHandoffBaselineTeam   string
	improveHandoffBaselinePolicy string
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

func init() {
	improveCmd.AddCommand(improveHandoffCmd)
	improveHandoffCmd.AddCommand(improveHandoffCreateCmd, improveHandoffShowCmd, improveHandoffPrepareCmd)
	flags := improveHandoffCmd.PersistentFlags()
	flags.StringVar(&improveHandoffProject, "project", "", "Project scope for the handoff (required)")
	flags.StringVar(&improveHandoffPolicy, "policy-version", "", "Optional memory policy version in the handoff scope")
	flags.StringVar(&improveHandoffTeamSearch, "team-search-path", "", "Reserved team search path metadata")
	flags.BoolVar(&improveHandoffJSON, "json", false, "Print the handoff as JSON")
	improveHandoffPrepareCmd.Flags().StringVar(&improveHandoffBaselineTeam, "baseline-team", "", "Baseline team snapshot ID (required for skill preparation)")
	improveHandoffPrepareCmd.Flags().StringVar(&improveHandoffBaselinePolicy, "baseline-policy", "", "Baseline memory policy snapshot ID (required for memory policy preparation)")
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
	if _, err := handoffScope(); err != nil {
		return err
	}
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	handoff, err := improve.NewHandoffStore(workspace).Get(args[0])
	if err != nil {
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
	if handoff.Kind != improve.HandoffSkill {
		return fmt.Errorf("handoff %q is %s; skill preparation requires kind skill", handoff.ID, handoff.Kind)
	}
	if handoff.Status == improve.HandoffCandidateReady {
		if handoff.Candidate == nil {
			return fmt.Errorf("handoff %q is candidate_ready without a candidate ref", handoff.ID)
		}
		if _, _, err := improve.LoadCandidateSnapshot(workspace, handoff.Candidate.ID); err != nil {
			return fmt.Errorf("load existing skill candidate: %w", err)
		}
		return printHandoff(handoff)
	}
	if handoff.Status != improve.HandoffProposed {
		return fmt.Errorf("handoff %q is %s; only proposed handoffs can be prepared", handoff.ID, handoff.Status)
	}
	ctx := cmd.Context()
	if handoff.Kind == improve.HandoffMemoryPolicy {
		return prepareMemoryPolicyHandoff(ctx, workspace, store, handoff)
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
	if err := promotion.ValidateDraft(promotion.TypeSkill, proposal.Draft, skillName, skillDraftStepsForHandoff(proposal.Draft)); err != nil {
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
	next.StatusReason = "isolated skill candidate snapshot prepared"
	updated, err := store.Transition(ctx, handoff.ID, handoff.Revision, next)
	if err != nil {
		return err
	}
	return printHandoff(updated)
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
	next.StatusReason = "immutable memory policy candidate validated; active policy unchanged"
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
	sources := make([]improve.SourceBinding, 0, len(proposal.SourceIDs))
	for _, sourceID := range proposal.SourceIDs {
		hash := strings.TrimSpace(proposal.SourceRevisions[sourceID])
		if hash == "" {
			return improve.ImprovementHandoff{}, fmt.Errorf("consolidation proposal source %q has no content revision", sourceID)
		}
		aggregate := proposal.AggregateRevisions[sourceID]
		sources = append(sources, improve.SourceBinding{Ref: improve.ArtifactRef{Kind: "context_item", ID: sourceID, Revision: hash}, ContentHash: hash, AggregateRevision: aggregate, ProjectID: scope.ProjectID, TeamID: scope.TeamID})
	}
	revision := digestRevision(struct {
		ID         string
		Sources    map[string]string
		Aggregates map[string]int64
		Candidate  string
	}{proposal.ID, proposal.SourceRevisions, proposal.AggregateRevisions, proposal.CandidateContextItemID})
	return improve.NewImprovementHandoff(improve.HandoffConsolidation, scope, improve.ArtifactRef{Kind: "consolidation_proposal", ID: proposal.ID, Revision: revision}, sources)
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

func skillDraftStepsForHandoff(draft string) []string {
	var steps []string
	for _, line := range strings.Split(draft, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "1. ") || strings.HasPrefix(line, "2. ") {
			steps = append(steps, line)
		}
	}
	return steps
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
