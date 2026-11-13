package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/workspace/versionstore"

	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
)

// Preview loads current schemas without activating the source or invoking a
// provider. Apply publishes a child only; ordinary resume remains a separate
// invocation with its full policy, evidence, verification and acceptance gates.
func runContractRecoveryCommand(cmd *cobra.Command, source *team.ResumeSource, name string, binding *commandWorkspaceBinding, registry *team.TeamRegistry, vars map[string]string) (runErr error) {
	recoveryCtx := withResumeLaunchPolicy(commandContext(cmd), source)
	if resumeRepairAction != "" {
		recoveryCtx = team.WithReadOnlyActionRepair(recoveryCtx, resumeRepairAction)
	}
	if resumeRebuildActionPayloads {
		recoveryCtx = team.WithActionPayloadReconstruction(recoveryCtx)
	}
	if resumeRepairResult != "" {
		recoveryCtx = team.WithContractResultRepair(recoveryCtx, resumeRepairResult)
	}
	if resumeRetryWork != "" {
		recoveryCtx = team.WithReadOnlyWorkRetry(recoveryCtx, resumeRetryWork)
	}
	if resumeMetadataOnly {
		recoveryCtx = team.WithMetadataOnlyContractRecovery(recoveryCtx)
	}
	if resumeUpgradeInputPolicy {
		recoveryCtx = team.WithFrozenInputPolicyRecovery(recoveryCtx)
	}
	setupCtx := context.WithValue(recoveryCtx, contractRecoverySetupKey{}, true)
	tc, err := loadTeamByNameWithWorkspaceBinding(setupCtx, name, binding, registry, opts.providerURL, opts.providerAPIKey, newPathConsent(), vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, tc.Close()) }()
	if err := bindContractRecoveryVersion(recoveryCtx, tc.session); err != nil {
		return err
	}
	plan, err := tc.coordinator.AssessContractRecovery(recoveryCtx, source)
	if err != nil {
		return err
	}
	if opts.dryRun {
		return writeContractRecoveryPreview(cmd.OutOrStdout(), plan, opts.outputFormat)
	}
	branch, err := tc.coordinator.ApplyContractRecovery(recoveryCtx, source, resumeRecoveryBranch, resumeRecoveryPlan)
	if err != nil {
		return err
	}
	if opts.outputFormat == "json" {
		data, err := json.Marshal(struct {
			Branch       string `json:"branch"`
			SourceBranch string `json:"source_branch"`
			PlanHash     string `json:"plan_hash"`
			MetadataOnly bool   `json:"metadata_only"`
			RepairResult string `json:"repair_result,omitempty"`
			RepairAction string `json:"repair_action,omitempty"`
		}{Branch: branch, SourceBranch: source.BranchID, PlanHash: plan.SHA256, MetadataOnly: plan.MetadataOnly, RepairResult: resumeRepairResult, RepairAction: resumeRepairAction})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}
	if plan.MetadataOnly {
		if _, err = fmt.Fprintln(cmd.OutOrStdout(), "Metadata-only recovery: session history is forked; project files remain shared."); err != nil {
			return err
		}
	}
	if plan.Repaired > 0 {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Recovery branch: %s; retained: %d; result-only repairs: %d; read-only retries: %d\nContinue with: hufu resume --team %q --execution-profile %q --workspace %q --branch %q --repair-result %q\n", branch, plan.Retained, plan.Repaired, plan.Retried, name, tc.coordinator.ExecutionProfile().Name, binding.Resolution.ControlRoot, branch, resumeRepairResult)
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Recovery branch: %s; retained: %d; read-only retries: %d\nContinue with: hufu resume --team %q --execution-profile %q --workspace %q --branch %q\n", branch, plan.Retained, plan.Retried, name, tc.coordinator.ExecutionProfile().Name, binding.Resolution.ControlRoot, branch)
	return err
}

type contractRecoverySetupKey struct{}

func contractRecoverySetup(ctx context.Context) bool {
	value, _ := ctx.Value(contractRecoverySetupKey{}).(bool)
	return value
}

// Take the existing team/project locks without startup recovery, migrations,
// MCP startup, acceptance changes or model initialization. Native preflight
// refuses unfinished operations before publishing a child.
func bindContractRecoveryVersion(ctx context.Context, session *team.TeamSession) error {
	lease, hasLease := session.WorkspaceLease.(*commandWorkspaceLease)
	if !opts.dryRun && (!hasLease || lease == nil) {
		return fmt.Errorf("contract recovery apply requires a managed workspace lease")
	}
	managed, registry, _, err := lookupManagedWorkspace(ctx, session.Workspace)
	if err != nil {
		return err
	}
	if registry == nil {
		return fmt.Errorf("contract recovery requires a managed workspace")
	}
	defer func() { _ = registry.Close() }()
	version, err := resolveWorkspaceVersionContext(ctx, registry, managed, config.LoadConfig())
	if err != nil {
		return err
	}
	if resumeMetadataOnly {
		session.WorkspaceVersion = version
		return nil
	}
	if !version.Required() {
		return fmt.Errorf("contract recovery requires required workspace versioning; an entirely read-only team with versioning off can explicitly preview --metadata-only")
	}
	if opts.dryRun {
		session.WorkspaceVersion = version
		return nil
	}
	lock, err := versionstore.TryLockProject(version.StateDir, versionstore.LockOwner{WorkspaceID: version.WorkspaceID, Operation: "contract-recovery"})
	if err != nil {
		return err
	}
	lease.versionLock = lock
	version.HoldsProjectLock = true
	session.WorkspaceVersion = version
	return nil
}
