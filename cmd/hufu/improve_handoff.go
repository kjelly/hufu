package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/improve"
	"github.com/spf13/cobra"
)

var (
	improveHandoffProject     string
	improveHandoffPolicy      string
	improveHandoffTeamSearch  string
	improveHandoffJSON        bool
	improveHandoffMemory      string
	improveHandoffConsolidate string
	improveHandoffPromotion   string
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

func init() {
	improveCmd.AddCommand(improveHandoffCmd)
	improveHandoffCmd.AddCommand(improveHandoffCreateCmd, improveHandoffShowCmd)
	flags := improveHandoffCmd.PersistentFlags()
	flags.StringVar(&improveHandoffProject, "project", "", "Project scope for the handoff (required)")
	flags.StringVar(&improveHandoffPolicy, "policy-version", "", "Optional memory policy version in the handoff scope")
	flags.StringVar(&improveHandoffTeamSearch, "team-search-path", "", "Reserved team search path metadata")
	flags.BoolVar(&improveHandoffJSON, "json", false, "Print the handoff as JSON")
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
