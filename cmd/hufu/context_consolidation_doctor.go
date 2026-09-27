package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	contextstore "github.com/kjelly/hufu/internal/context"
)

var contextConsolidationCheck bool

func init() {
	contextLearningDoctorCmd.Flags().BoolVar(&contextConsolidationCheck, "consolidation", false, "Check consolidation proposals, candidates, and source freshness (read-only)")
	contextLearningDoctorCmd.Flags().StringVar(&contextProject, "project", "", "Canonical project ID (required with --consolidation)")
	contextLearningDoctorCmd.Flags().StringVar(&contextTeam, "team", "", "Canonical team ID; omitted checks every team in the project")
}

// runContextConsolidationDoctor prints IDs, states, and reason codes for every
// consolidation proposal in scope. It never outputs memory content and never
// writes; it exits non-zero only when the report cannot be produced.
func runContextConsolidationDoctor(cmd *cobra.Command) error {
	if strings.TrimSpace(contextProject) == "" {
		return fmt.Errorf("--project is required with --consolidation")
	}
	repo, err := openExistingContextRepository(getContextWorkspace())
	if err != nil {
		return fmt.Errorf("consolidation database: %w", err)
	}
	defer func() { _ = repo.Close() }()
	report, err := repo.ConsolidationDoctor(cmd.Context(), contextProject, contextTeam, strings.TrimSpace(contextTeam) == "", contextPolicyVersion)
	if err != nil {
		return fmt.Errorf("consolidation doctor: %w", err)
	}
	status := "ok"
	if !report.Healthy() {
		status = "attention"
	}
	if contextQueryJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			SchemaVersion int    `json:"schema_version"`
			Status        string `json:"status"`
			contextstore.ConsolidationDoctorReport
		}{SchemaVersion: 1, Status: status, ConsolidationDoctorReport: report})
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "context doctor --consolidation: %s fresh=%d stale=%d blocked=%d invalid=%d orphans=%d\n", status,
		report.Counts[contextstore.ConsolidationFresh], report.Counts[contextstore.ConsolidationStale], report.Counts[contextstore.ConsolidationBlocked], report.Counts[contextstore.ConsolidationInvalid], len(report.Orphans)); err != nil {
		return err
	}
	for _, proposal := range report.Proposals {
		if proposal.State == contextstore.ConsolidationFresh {
			continue
		}
		if _, err := fmt.Fprintf(out, "proposal=%s status=%s candidate=%s state=%s reasons=%s\n", proposal.ProposalID, proposal.Status, proposal.CandidateID, proposal.State, consolidationReasonList(proposal.Reasons)); err != nil {
			return err
		}
	}
	for _, orphan := range report.Orphans {
		if _, err := fmt.Fprintf(out, "orphan=%s reason=%s\n", orphan.ItemID, orphan.Reason); err != nil {
			return err
		}
	}
	return nil
}

func consolidationReasonList(reasons []contextstore.ConsolidationReason) string {
	parts := make([]string, len(reasons))
	for i, reason := range reasons {
		parts[i] = string(reason)
	}
	return strings.Join(parts, ",")
}
