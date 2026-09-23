package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/consolidation"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/sidecar"
)

// consolidationDraftFactory builds the drafting model; tests replace it.
var consolidationDraftFactory = func(ctx context.Context, teamDir, workspace string) (consolidation.TextGenerator, string, func(), error) {
	return newMaintenanceTextGenerator(ctx, maintenanceGeneratorOptions{
		TeamDir: teamDir, Workspace: workspace, ModelOverride: contextConsolidateModel,
		Purpose: "consolidation_draft", Profile: sidecar.CompactorProfile,
	})
}

func validateConsolidateDraftFlags() error {
	if !contextConsolidateDraft {
		return nil
	}
	switch {
	case !contextApplyProposal:
		return fmt.Errorf("--draft requires --apply-proposal")
	case strings.TrimSpace(contextProposalText) != "":
		return fmt.Errorf("--draft and --proposal-text are mutually exclusive")
	case strings.TrimSpace(contextProposalSources) == "":
		return fmt.Errorf("--draft requires --source")
	case strings.TrimSpace(contextTeam) == "":
		return fmt.Errorf("--draft requires --team")
	}
	return nil
}

// runContextConsolidateDraft validates the sources first, reuses a pending
// proposal for the same sources, and only then asks the model for text. A
// model or validation failure persists nothing.
func runContextConsolidateDraft(cmd *cobra.Command, repo *contextstore.SQLiteRepository) error {
	ctx := cmd.Context()
	sources, ids, err := loadConsolidationSources(cmd, repo)
	if err != nil {
		return err
	}
	draftSources := make([]consolidation.DraftSource, 0, len(sources))
	for _, source := range sources {
		draftSources = append(draftSources, consolidation.DraftSource{ID: source.ID, Kind: string(source.Kind), Content: source.Content})
	}
	sort.Slice(draftSources, func(i, j int) bool { return draftSources[i].ID < draftSources[j].ID })
	if consolidation.SourceRunes(draftSources) > consolidation.MaxSourceRunes {
		return fmt.Errorf("consolidation sources are too large for drafting; use --proposal-text")
	}
	existing, found, err := repo.FindProposedConsolidation(ctx, contextProject, contextTeam, ids)
	if err != nil {
		return err
	}
	if found {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "context consolidate: proposal=%s candidate=%s status=proposed (already pending; explicit approval required)\n", existing.ID, existing.CandidateContextItemID)
		return err
	}
	registry := teamRegistryFromSearchPath(contextConsolidateSearchPath)
	if err = registry.Discover(); err != nil {
		return err
	}
	teamDir, err := registry.Resolve(contextTeam)
	if err != nil {
		return err
	}
	generator, model, release, err := consolidationDraftFactory(ctx, teamDir, getContextWorkspace())
	if err != nil {
		return err
	}
	defer release()
	result, err := consolidation.JSONDrafter{Generator: generator}.Draft(ctx, draftSources)
	if err != nil {
		return fmt.Errorf("draft consolidation: %w", err)
	}
	if err = consolidation.ValidateDraft(result, draftSources); err != nil {
		return err
	}
	return persistConsolidationProposal(cmd, repo, sources, ids, strings.TrimSpace(result.Text), "model", model)
}
