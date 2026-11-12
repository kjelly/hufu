package main

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
	"github.com/spf13/cobra"
)

func validateResumeRepairTarget(source *team.ResumeSource, taskID string) error {
	if taskID == "" {
		return nil
	}
	for _, item := range source.Session.Tasks {
		if item != nil && item.ID == taskID && item.FailureEvent != nil &&
			((item.Status == team.TaskBlocked && item.FailureEvent.FailureClass == team.FailureProtocol) ||
				(item.Status == team.TaskError && item.FailureEvent.FailureClass == team.FailureCancelled && item.ExecutionReceipt != nil && item.ExecutionReceipt.ResultCollection != nil)) {
			return nil
		}
	}
	return fmt.Errorf("branch %q has no blocked protocol task or cancelled collection %q", source.BranchID, taskID)
}

func runResumeSourcePreflight(cmd *cobra.Command, session *team.TeamSession, coordinator *team.Coordinator, source *team.ResumeSource) error {
	if err := bindResumePreflightVersion(cmd.Context(), session); err != nil {
		return err
	}
	if resumeRetryAcceptance {
		assessment, err := coordinator.AssessFinalAcceptanceRetry(cmd.Context(), source, resumeRun, resumeAcceptanceTimeout)
		if err != nil {
			return err
		}
		if opts.outputFormat == "json" {
			data, err := json.Marshal(assessment)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Final acceptance retry preflight passed: branch %s; source run %s; retained tasks %d; per-review timeout %ds; maximum review calls %d. Saved answer and frozen evidence are unchanged; no worker or collector dispatch. Declared team acceptance verifiers still run.\n", assessment.Branch, assessment.SourceRunID, assessment.Retained, assessment.TimeoutSeconds, assessment.MaxReviewCalls)
		return err
	}
	assessment, err := coordinator.AssessResumeSource(cmd.Context(), source)
	if err != nil {
		return err
	}
	if opts.outputFormat == "json" {
		data, err := json.Marshal(assessment)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Resume preflight passed: branch %s; accepted: %d; resumable: %d; terminal unresolved: %d\n", assessment.Branch, assessment.Accepted, assessment.Resumable, assessment.TerminalUnresolved)
	return err
}

func bindResumePreflightVersion(ctx context.Context, session *team.TeamSession) error {
	managed, registry, _, err := lookupManagedWorkspace(ctx, session.Workspace)
	if err != nil {
		return err
	}
	if registry == nil {
		return fmt.Errorf("%w: resume preflight requires a managed workspace", versionstore.ErrVersionedWorkspaceUnresolved)
	}
	defer func() { _ = registry.Close() }()
	version, err := resolveWorkspaceVersionContext(ctx, registry, managed, config.LoadConfig())
	if err != nil {
		return err
	}
	session.WorkspaceVersion = version
	return nil
}
