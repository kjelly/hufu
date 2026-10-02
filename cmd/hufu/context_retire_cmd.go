package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var contextRetireReason string

var contextRetireCmd = &cobra.Command{
	Use:   "retire <id...>",
	Short: "Withdraw current confirmed records that have no replacement",
	Long: `Withdraw current confirmed records an operator found wrong or stale.

A retired record stops being current knowledge at once: it leaves prompts,
promotion, and consolidation sources, and consolidations derived from it are
demoted. The record and the reason stay for audit. Use supersede instead when
a confirmed replacement exists.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runContextRetire,
}

func init() {
	contextRetireCmd.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace directory containing context.sqlite")
	contextRetireCmd.Flags().StringVar(&contextProject, "project", "", "Canonical project ID (required)")
	contextRetireCmd.Flags().StringVar(&contextTeam, "team", "", "Optional exact team scope")
	contextRetireCmd.Flags().StringVar(&contextRetireReason, "reason", "", "Why the records are wrong or stale (required)")
	contextCmd.AddCommand(contextRetireCmd)
}

type contextRetirer interface {
	RetireConfirmed(ctx context.Context, ids []string, reason string) error
}

func runContextRetire(cmd *cobra.Command, ids []string) error {
	if strings.TrimSpace(contextRetireReason) == "" {
		return fmt.Errorf("--reason is required")
	}
	repo, err := openContextMutationRepo()
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	retirer, ok := repo.(contextRetirer)
	if !ok {
		return fmt.Errorf("context store does not support retiring records")
	}
	items, err := loadExactMutationItems(cmd, repo, ids)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("retire: context item not found: %w", err)
	}
	if err != nil {
		return err
	}
	if err := retirer.RetireConfirmed(cmd.Context(), ids, contextRetireReason); err != nil {
		return err
	}
	if err := rebuildMutationProjections(cmd, repo, items); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context retire: %d item(s) retired\n", len(items))
	return err
}
