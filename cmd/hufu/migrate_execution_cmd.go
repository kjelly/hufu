package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/executioncompat"
	"github.com/kjelly/hufu/internal/team"
)

var migrateInspectExecutionBranch string
var migrateInspectExecutionJSON bool

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Inspect and migrate durable compatibility state",
	Args:  cobra.NoArgs,
}

var migrateInspectExecutionCmd = &cobra.Command{
	Use:   "inspect-execution",
	Short: "Read-only inventory of deprecated durable execution identity",
	Long: `Inspect durable execution identity without loading a team, opening an
EventStore writer, or changing the workspace. Findings contain identifiers and
classification only; task content, models, provider URLs, and credentials are
never printed.`,
	Args: cobra.NoArgs,
	RunE: runMigrateInspectExecution,
}

func init() {
	migrateInspectExecutionCmd.Flags().StringVar(&migrateInspectExecutionBranch, "branch", "", "Inspect only one session branch (ID or name)")
	migrateInspectExecutionCmd.Flags().BoolVar(&migrateInspectExecutionJSON, "json", false, "Write the stable JSON inspection report to stdout")
	migrateCmd.AddCommand(migrateInspectExecutionCmd)
}

func runMigrateInspectExecution(cmd *cobra.Command, _ []string) error {
	report, err := team.InspectExecutionCompatibility(context.Background(), getWorkspace(), migrateInspectExecutionBranch)
	if err != nil {
		return fmt.Errorf("hufu migrate inspect-execution: %w", err)
	}
	if migrateInspectExecutionJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
	}
	return renderExecutionCompatibilityInspection(cmd.OutOrStdout(), report)
}

func renderExecutionCompatibilityInspection(writer io.Writer, report *executioncompat.InspectionReport) error {
	if report == nil {
		return fmt.Errorf("execution compatibility inspection report is nil")
	}
	lines := []struct {
		name  string
		value int
	}{
		{"legacy_local_alias_events", report.LegacyLocalAliasEvents},
		{"legacy_provider_shadow_events", report.LegacyProviderShadowEvents},
		{"legacy_execution_event_providers", report.LegacyExecutionEventProviders},
		{"legacy_provider_bindings", report.LegacyProviderBindings},
		{"legacy_provider_session_events", report.LegacyProviderSessionEvents},
		{"legacy_receipt_providers", report.LegacyReceiptProviders},
		{"legacy_policy_routes", report.LegacyPolicyRoutes},
		{"canonical_tasks", report.CanonicalTasks},
		{"migrated_tasks", report.MigratedTasks},
		{"migratable_tasks", report.MigratableTasks},
		{"ambiguous_tasks", report.AmbiguousTasks},
		{"unmigratable_tasks", report.UnmigratableTasks},
		{"canonical_policy_snapshots", report.CanonicalPolicySnapshots},
		{"migrated_policy_snapshots", report.MigratedPolicySnapshots},
		{"migratable_policy_snapshots", report.MigratablePolicySnapshots},
		{"ambiguous_policy_snapshots", report.AmbiguousPolicySnapshots},
		{"unmigratable_policy_snapshots", report.UnmigratablePolicySnapshots},
	}
	if _, err := fmt.Fprintf(writer, "schema_version: %d\nscope: %s\n", report.SchemaVersion, report.Scope); err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintf(writer, "%s: %d\n", line.name, line.value); err != nil {
			return err
		}
	}
	for _, finding := range report.Findings {
		id := finding.TaskID
		if id == "" {
			id = finding.SourceEventID
		}
		if _, err := fmt.Fprintf(writer, "%s %s %s %s %s\n", finding.SubjectKind, finding.BranchID, id, finding.Classification, finding.ReasonCode); err != nil {
			return err
		}
	}
	return nil
}
