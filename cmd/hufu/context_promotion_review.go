package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/promotion"
	"github.com/kjelly/hufu/internal/utils"
)

type promotionReviewSession struct {
	input  *bufio.Reader
	output io.Writer
}

func runPromotionReview(cmd *cobra.Command, args []string) error {
	if promotionJSON {
		return fmt.Errorf("interactive promotion review does not support --json; use list/show for machine-readable output")
	}
	if opts.unattended || promotionReviewUnattended {
		return fmt.Errorf("promotion review requires a human operator; --unattended cannot approve or apply")
	}
	input := cmd.InOrStdin()
	file, ok := input.(*os.File)
	if !ok || !term.IsTerminal(file.Fd()) {
		return fmt.Errorf("promotion review requires an interactive TTY; use list/show for read-only automation")
	}
	return runPromotionReviewSession(cmd.Context(), promotionReviewSession{input: bufio.NewReader(input), output: cmd.OutOrStdout()}, args)
}

func runPromotionReviewSession(ctx context.Context, session promotionReviewSession, args []string) error {
	if err := promotionScopeValid(); err != nil {
		return err
	}
	repo, err := openPromotionReadOnly()
	if err != nil {
		return err
	}
	id := ""
	if len(args) == 1 {
		id = strings.TrimSpace(args[0])
	} else {
		proposals, listErr := repo.ListPromotions(ctx, promotionProject, promotionTeam)
		if listErr != nil {
			_ = repo.Close()
			return listErr
		}
		if len(proposals) == 0 {
			_ = repo.Close()
			_, err = fmt.Fprintln(session.output, "No promotion proposals are available for this scope. Run context promotion analyze explicitly when evidence is sufficient.")
			return err
		}
		for _, proposal := range proposals {
			if _, err = fmt.Fprintf(session.output, "%s\t%s\t%s\t%s\n", proposal.ID, proposal.Type, proposal.Status, proposal.TargetPath); err != nil {
				_ = repo.Close()
				return err
			}
		}
		if _, err = fmt.Fprint(session.output, "Proposal ID (blank to skip): "); err != nil {
			_ = repo.Close()
			return err
		}
		id, err = readPromotionReviewLine(session.input)
		if err != nil {
			_ = repo.Close()
			return err
		}
		if id == "" {
			_ = repo.Close()
			return nil
		}
	}
	proposal, err := repo.GetPromotion(ctx, id, promotionProject, promotionTeam)
	if err != nil {
		_ = repo.Close()
		return err
	}
	revision := promotionRecordRevision(proposal)
	if proposal.Status == promotion.StatusProposed || proposal.Status == promotion.StatusApproved {
		revision, err = promotionReviewRevision(ctx, repo, proposal)
		if err != nil {
			_ = repo.Close()
			return err
		}
	}
	if err = renderPromotionReview(ctx, session.output, repo, proposal); err != nil {
		_ = repo.Close()
		return err
	}
	if err = repo.Close(); err != nil {
		return err
	}

	if proposal.Status == promotion.StatusApproved {
		return promptPromotionApply(ctx, session, proposal)
	}
	if proposal.Status != promotion.StatusProposed {
		_, err = fmt.Fprintf(session.output, "Proposal is %s; no review mutation is available.\n", proposal.Status)
		return err
	}
	if _, err = fmt.Fprint(session.output, "Action [a]pprove, [e]dit from file, [r]eject, [s]kip: "); err != nil {
		return err
	}
	action, err := readPromotionReviewLine(session.input)
	if err != nil {
		return err
	}
	switch strings.ToLower(action) {
	case "a", "approve":
		approved, err := mutateReviewedPromotion(ctx, proposal.ID, revision, func(ctx context.Context, service promotion.Service) (promotion.Proposal, error) {
			return service.Approve(ctx, proposal.ID, promotionProject, promotionTeam)
		})
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(session.output, "Status: approved-not-applied. The target file is unchanged."); err != nil {
			return err
		}
		return promptPromotionApply(ctx, session, approved)
	case "e", "edit":
		if _, err = fmt.Fprint(session.output, "Draft file path (blank to skip): "); err != nil {
			return err
		}
		path, readErr := readPromotionReviewLine(session.input)
		if readErr != nil || path == "" {
			return readErr
		}
		_, err = mutateReviewedPromotion(ctx, proposal.ID, revision, func(ctx context.Context, service promotion.Service) (promotion.Proposal, error) {
			return service.Edit(ctx, proposal.ID, promotionProject, promotionTeam, path)
		})
		if err == nil {
			_, err = fmt.Fprintln(session.output, "Draft updated. Re-run review to inspect the new revision before approval.")
		}
		return err
	case "r", "reject":
		if _, err = fmt.Fprint(session.output, "Rejection reason (blank to skip): "); err != nil {
			return err
		}
		reason, readErr := readPromotionReviewLine(session.input)
		if readErr != nil || reason == "" {
			return readErr
		}
		_, err = mutateReviewedPromotion(ctx, proposal.ID, revision, func(ctx context.Context, service promotion.Service) (promotion.Proposal, error) {
			return service.Reject(ctx, proposal.ID, promotionProject, promotionTeam, reason)
		})
		return err
	case "", "s", "skip":
		return nil
	default:
		return fmt.Errorf("unknown review action %q; no change was made", action)
	}
}

func mutateReviewedPromotion(ctx context.Context, id, expectedRevision string, mutate func(context.Context, promotion.Service) (promotion.Proposal, error)) (promotion.Proposal, error) {
	repo, service, err := openPromotionContext(ctx)
	if err != nil {
		return promotion.Proposal{}, err
	}
	defer func() { _ = repo.Close() }()
	fresh, err := service.Get(ctx, id, promotionProject, promotionTeam)
	if err != nil {
		return promotion.Proposal{}, err
	}
	freshRevision, err := promotionReviewRevision(ctx, repo, fresh)
	if err != nil {
		return promotion.Proposal{}, err
	}
	if freshRevision != expectedRevision {
		return promotion.Proposal{}, fmt.Errorf("promotion changed during review; refresh and review the current revision")
	}
	updated, err := mutate(ctx, service)
	if err != nil {
		return promotion.Proposal{}, err
	}
	if err = finishPromotion(ctx, repo); err != nil {
		return promotion.Proposal{}, err
	}
	return updated, nil
}

func promptPromotionApply(ctx context.Context, session promotionReviewSession, proposal promotion.Proposal) error {
	readOnly, err := openPromotionReadOnly()
	if err != nil {
		return err
	}
	fresh, err := readOnly.GetPromotion(ctx, proposal.ID, promotionProject, promotionTeam)
	if err != nil {
		_ = readOnly.Close()
		return err
	}
	expectedRevision, err := promotionReviewRevision(ctx, readOnly, fresh)
	if closeErr := readOnly.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(session.output, "To apply, type exactly: apply %s\nApply confirmation (blank to leave approved): ", proposal.ID); err != nil {
		return err
	}
	confirmation, err := readPromotionReviewLine(session.input)
	if err != nil {
		return err
	}
	if confirmation == "" {
		return nil
	}
	if confirmation != "apply "+proposal.ID {
		return fmt.Errorf("apply confirmation did not match; proposal remains approved-not-applied")
	}
	repo, service, err := openPromotionContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	fresh, err = service.Get(ctx, proposal.ID, promotionProject, promotionTeam)
	if err != nil {
		return err
	}
	freshRevision, err := promotionReviewRevision(ctx, repo, fresh)
	if err != nil {
		return err
	}
	if freshRevision != expectedRevision {
		return fmt.Errorf("promotion changed before apply; refresh and review the current revision")
	}
	result, err := service.Apply(ctx, proposal.ID, promotionProject, promotionTeam, promotionRegistry())
	if flushErr := finishPromotion(ctx, repo); flushErr != nil && err == nil {
		err = flushErr
	}
	if err != nil {
		return err
	}
	if result.AlreadyApplied {
		_, err = fmt.Fprintln(session.output, "Status: already-applied.")
	} else {
		_, err = fmt.Fprintln(session.output, "Status: applied.")
	}
	return err
}

func renderPromotionReview(ctx context.Context, output io.Writer, repo contextstore.ReadOnlyRepository, proposal promotion.Proposal) error {
	if _, err := fmt.Fprintf(output, "\nProposal %s\nstatus: %s\ntype: %s\ntarget: %s\npolicy: %s\n", proposal.ID, proposal.Status, proposal.Type, proposal.TargetPath, proposal.PolicyVersion); err != nil {
		return err
	}
	if proposal.Type == promotion.TypeTeamPolicy {
		if _, err := fmt.Fprintln(output, "scope: coordinator/orchestrator policy only (not every worker)"); err != nil {
			return err
		}
	}
	for _, source := range proposal.Sources {
		scope := contextstore.Scope{ProjectID: proposal.ProjectID, TeamID: proposal.TeamID, AgentID: proposal.AgentID}
		item, err := repo.GetScoped(ctx, source.ContextItemID, contextstore.ScopedReadOptions{Scope: scope, IncludeContent: true})
		if err != nil {
			return fmt.Errorf("review source %s: %w", source.ContextItemID, err)
		}
		summary := boundedPromotionText(utils.RedactSecrets(item.Content), 240)
		if _, err = fmt.Fprintf(output, "source %s (%s, evidence=%d): %s\n", item.ID, item.Kind, len(item.Evidence), summary); err != nil {
			return err
		}
	}
	draft := boundedPromotionText(utils.RedactSecrets(proposal.Draft), 8192)
	if _, err := fmt.Fprintf(output, "change preview:\n--- %s\n+++ proposed\n", proposal.TargetPath); err != nil {
		return err
	}
	lines := strings.Split(draft, "\n")
	if len(lines) > 80 {
		lines = append(slices.Clone(lines[:80]), "…")
	}
	for _, line := range lines {
		if _, err := fmt.Fprintf(output, "+ %s\n", line); err != nil {
			return err
		}
	}
	return nil
}

func promotionReviewRevision(ctx context.Context, repo contextstore.ReadOnlyRepository, proposal promotion.Proposal) (string, error) {
	if contextstore.HashPromotionContent(proposal.Draft) != proposal.DraftHash || utils.RedactSecrets(proposal.Draft) != proposal.Draft {
		return "", fmt.Errorf("promotion draft changed or contains secret-like material; edit and review a safe revision")
	}
	for _, source := range proposal.Sources {
		item, err := repo.GetScoped(ctx, source.ContextItemID, contextstore.ScopedReadOptions{
			Scope: contextstore.Scope{ProjectID: proposal.ProjectID, TeamID: proposal.TeamID, AgentID: proposal.AgentID},
		})
		if err != nil {
			return "", fmt.Errorf("promotion source %s is unavailable: %w", source.ContextItemID, err)
		}
		if item.Lifecycle != contextstore.LifecycleConfirmed || item.SupersededBy != "" || item.ContentHash != source.ContentHash {
			return "", fmt.Errorf("promotion source %s changed; analyze and review a fresh proposal", source.ContextItemID)
		}
		aggregate, err := repo.ExperienceAggregate(ctx, source.ContextItemID, proposal.PolicyVersion)
		if err != nil || aggregate.Revision != source.AggregateRevision {
			return "", fmt.Errorf("promotion source %s outcome evidence changed; analyze and review a fresh proposal", source.ContextItemID)
		}
	}
	targetHash, err := promotion.CurrentTargetHash(proposal, promotionRegistry())
	if err != nil {
		return "", fmt.Errorf("promotion target preflight: %w", err)
	}
	if targetHash != proposal.TargetBaseHash {
		return "", fmt.Errorf("promotion target changed; refresh and review a fresh proposal")
	}
	return promotionRecordRevision(proposal), nil
}

func promotionRecordRevision(proposal promotion.Proposal) string {
	var value strings.Builder
	fmt.Fprintf(&value, "%s\x00%s\x00%s\x00%s\x00%s\x00%d", proposal.ID, proposal.Status, proposal.DraftHash, proposal.TargetBaseHash, proposal.PolicyVersion, proposal.UpdatedAt.UnixNano())
	for _, source := range proposal.Sources {
		fmt.Fprintf(&value, "\x00%s\x00%s\x00%d", source.ContextItemID, source.ContentHash, source.AggregateRevision)
	}
	return contextstore.HashPromotionContent(value.String())
}

func readPromotionReviewLine(input *bufio.Reader) (string, error) {
	line, err := input.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func boundedPromotionText(value string, limit int) string {
	value = strings.TrimSpace(utils.SanitizeTerminalText(value))
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}
