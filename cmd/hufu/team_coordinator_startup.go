package main

import (
	"context"
	"fmt"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

// startTeamCoordinator runs the startup steps between coordinator
// construction and handing the team context to its caller. The caller closes
// the coordinator when this fails.
func startTeamCoordinator(ctx context.Context, coordinator *team.Coordinator, session *team.TeamSession, cfg *config.Config, execProfile team.ExecutionProfile, startsFresh bool, sessionData *team.SessionData, models []string) error {
	// The execution policy is durable before any provider profile/capability
	// probe. Those probes can open provider transports, so a legacy interrupted
	// session or configuration drift must fail here rather than after a model
	// boundary has already been crossed.
	coordinator.SetExecutionProfile(execProfile)
	coordinator.SetFreshSession(startsFresh)
	coordinator.SetSessionData(sessionData)
	if err := freezeStartupExecutionPolicy(coordinator); err != nil {
		return err
	}
	if err := completeManagedFreshSession(ctx, session); err != nil {
		return fmt.Errorf("complete rebound workspace fresh-session checkpoint: %w", err)
	}
	// Warm provider-bound profiles after the coordinator owns the exact
	// ProviderManager used for invocation. This covers configured agents,
	// extra models, model-list candidates, and all auxiliary role models.
	coordinator.WarmModelProfiles(ctx, models, session.Config.Generation.ContextWindow)
	modelCapabilityValidation := coordinator.ValidateModelCapabilities(ctx)
	for _, warning := range modelCapabilityValidation.Warnings {
		stderrLog("%s %s\n", errStyle.Render("⚠"), warning)
	}
	if err := modelCapabilityValidation.Err(); err != nil {
		return err
	}

	if stallThreshold := cfg.ResolveStallThreshold(session.Config.StallThreshold); stallThreshold > 0 {
		coordinator.SetStallWatchdog(stallThreshold, 0)
	}
	if err := applyUnattendedAndBudget(coordinator, session); err != nil {
		return err
	}
	return coordinator.SetPTYTerminalEnabled(opts.enablePTYTerminal)
}

// freezeStartupExecutionPolicy persists the execution policy and repeats the
// action catalog proposer check against resolved worker targets. Both run
// before any provider preflight.
func freezeStartupExecutionPolicy(coordinator *team.Coordinator) error {
	if err := coordinator.FreezeExecutionPolicyAtStartup(); err != nil {
		return fmt.Errorf("freeze execution policy before provider preflight: %w", err)
	}
	if err := coordinator.ValidateActionCatalogProposers(); err != nil {
		return fmt.Errorf("validate action catalog proposers: %w", err)
	}
	return nil
}
