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

func runResumeSourcePreflight(cmd *cobra.Command, session *team.TeamSession, coordinator *team.Coordinator, source *team.ResumeSource) error {
	if err := bindResumePreflightVersion(cmd.Context(), session); err != nil {
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
