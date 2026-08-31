package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
)

var (
	targetedRecoveryTaskID   string
	targetedRecoveryTeamName string
	targetedRecoverySearch   string
	targetedRecoveryJSON     bool
)

var reconcileCmd = &cobra.Command{
	Use:   "reconcile --task <task-id>",
	Short: "Reconcile one task without replaying its worker",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return runTargetedRecoveryCommand(team.TargetedRecoveryReconcile)
	},
}

var retryCmd = &cobra.Command{
	Use:   "retry --task <task-id>",
	Short: "Retry one failed or blocked task when policy permits",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return runTargetedRecoveryCommand(team.TargetedRecoveryRetry)
	},
}

func runTargetedRecoveryCommand(action team.TargetedRecoveryAction) error {
	workspace := getWorkspace()
	teamName := strings.ToLower(strings.TrimSpace(targetedRecoveryTeamName))
	if teamName != "" && opts.workspace == "" {
		workspace = filepath.Join(workspace, teamName)
	}
	if teamName == "" {
		base := filepath.Base(filepath.Clean(workspace))
		if base == "." || base == "workspace" || base == "" {
			return fmt.Errorf("cannot infer team from workspace %q; pass --agent-team", workspace)
		}
		teamName = strings.ToLower(base)
	}

	searchPaths := resolveSearchPaths()
	if strings.TrimSpace(targetedRecoverySearch) != "" {
		searchPaths = strings.Split(targetedRecoverySearch, ",")
	}
	registry := team.NewTeamRegistry(searchPaths)
	if err := registry.Discover(); err != nil {
		return fmt.Errorf("failed to discover teams: %w", err)
	}
	if !registry.HasTeam(teamName) {
		return fmt.Errorf("team %q not found; pass --agent-team-search-path or run hufu list", teamName)
	}

	vars, err := buildVars()
	if err != nil {
		return err
	}
	tc, err := loadTeamByNameAtWorkspace(context.Background(), teamName, workspace, registry, opts.providerURL, opts.providerAPIKey, newPathConsent(), vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
	if err != nil {
		return fmt.Errorf("failed to load team %q: %w", teamName, err)
	}

	var report team.TargetedRecoveryReport
	switch action {
	case team.TargetedRecoveryReconcile:
		report, err = tc.coordinator.ReconcileTask(context.Background(), targetedRecoveryTaskID)
	case team.TargetedRecoveryRetry:
		report, err = tc.coordinator.RetryTask(context.Background(), targetedRecoveryTaskID)
	default:
		return fmt.Errorf("unsupported targeted recovery action %q", action)
	}
	printTargetedRecoveryReport(report)
	return err
}

func printTargetedRecoveryReport(report team.TargetedRecoveryReport) {
	if targetedRecoveryJSON {
		_ = json.NewEncoder(os.Stdout).Encode(report)
		return
	}
	_, _ = fmt.Fprintf(os.Stdout, "Action:         %s\n", report.Action)
	_, _ = fmt.Fprintf(os.Stdout, "Task:           %s\n", report.TaskID)
	_, _ = fmt.Fprintf(os.Stdout, "Status:         %s\n", report.Status)
	if report.RecoveryState != "" {
		_, _ = fmt.Fprintf(os.Stdout, "Recovery state: %s\n", report.RecoveryState)
	}
	if report.Retries > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "Retries:        %d\n", report.Retries)
	}
	if report.RunOutcome != "" {
		_, _ = fmt.Fprintf(os.Stdout, "Run outcome:    %s\n", report.RunOutcome)
	}
	if report.Detail != "" {
		_, _ = fmt.Fprintf(os.Stdout, "Detail:         %s\n", report.Detail)
	}
}

func init() {
	for _, cmd := range []*cobra.Command{reconcileCmd, retryCmd} {
		cmd.Flags().StringVar(&targetedRecoveryTaskID, "task", "", "Runtime task ID to reconcile or retry")
		cmd.Flags().StringVar(&targetedRecoveryTeamName, "agent-team", "", "Agent team name (inferred from --workspace when omitted)")
		cmd.Flags().StringVar(&targetedRecoverySearch, "agent-team-search-path", "", "Comma-separated team search paths")
		cmd.Flags().BoolVar(&targetedRecoveryJSON, "json", false, "Write machine-readable JSON to stdout")
		_ = cmd.MarkFlagRequired("task")
	}
}
