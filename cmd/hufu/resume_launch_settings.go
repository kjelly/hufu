package main

import (
	"cmp"
	"context"
	"slices"

	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type resumeLaunchPolicyKey struct{}

func withResumeLaunchPolicy(ctx context.Context, source *team.ResumeSource) context.Context {
	return context.WithValue(ctx, resumeLaunchPolicyKey{}, source.Session.ExecutionPolicySnapshot)
}

func restoreResumeLaunchSettings(ctx context.Context, session *team.TeamSession) error {
	policy, _ := ctx.Value(resumeLaunchPolicyKey{}).(*team.ExecutionPolicySnapshot)
	return team.RestoreLaunchSettings(session, policy)
}

// A selected profile is applied before these defaults. Explicit CLI values
// (including false/zero) win. No saved shell command or environment is run.
func restoreResumeLaunchDefaults(cmd *cobra.Command, source *team.ResumeSource) {
	policy := source.Session.ExecutionPolicySnapshot
	if policy == nil || policy.Launch == nil {
		return
	}
	saved := policy.Launch.Options
	changed := func(name string) bool {
		flag := cmd.Flags().Lookup(name)
		return flag != nil && flag.Changed
	}
	for _, entry := range []struct {
		name  string
		value *bool
		saved bool
	}{
		{"no-net", &opts.noNet, saved.NoNet},
		{"force-mcp", &opts.forceMCP, saved.ForceMCP},
		{"rbash", &opts.rbashMode, saved.RestrictedBash},
		{"plan", &opts.planMode, saved.PlanFirst},
		{"auto-skills", &opts.autoSkills, saved.AutoSkills},
		{"unattended", &opts.unattended, saved.Unattended},
		{"auto-approve", &opts.autoApprove, saved.AutoApprove},
		{"no-journal", &opts.noJournal, saved.NoJournal},
	} {
		if !changed(entry.name) && !*entry.value {
			*entry.value = entry.saved
		}
	}
	if !changed("execution-profile") && opts.executionProfile == "" {
		opts.executionProfile = saved.ExecutionProfile
	}
	if !changed("decision-profile") && opts.decisionProfile == "" {
		opts.decisionProfile = saved.DecisionProfile
	}
	if !changed("skill") && len(opts.forcedSkills) == 0 {
		opts.forcedSkills = slices.Clone(saved.ForcedSkills)
	}
	if !changed("max-duration") && opts.maxDuration == 0 {
		opts.maxDuration = saved.MaxDuration
	}
	if !changed("max-total-tokens") && opts.maxTotalTokens == 0 {
		opts.maxTotalTokens = saved.MaxTotalTokens
	}
}

func captureLaunchOptions(session *team.TeamSession, profile team.ExecutionProfile, gate startupStaticGate, planMode, autoSkills bool) *team.LaunchOptions {
	return &team.LaunchOptions{
		ExecutionProfile: string(profile.Name), NoNet: gate.NoNet, ForceMCP: gate.ForceMCP,
		RestrictedBash: opts.rbashMode, PlanFirst: planMode, AutoSkills: autoSkills,
		ForcedSkills: slices.Clone(opts.forcedSkills),
		Unattended:   opts.unattended || session.Config.Unattended || profile.IsUnattended(),
		AutoApprove:  opts.autoApprove || session.Config.AutoApprove, NoJournal: opts.noJournal,
		DecisionProfile: opts.decisionProfile,
		MaxDuration:     cmp.Or(opts.maxDuration, session.Config.MaxWallClock),
		MaxTotalTokens:  cmp.Or(opts.maxTotalTokens, session.Config.MaxTotalTokens),
	}
}

func registerResumeLaunchFlags(f *pflag.FlagSet) {
	f.StringVar(&opts.temperatureOverride, "temperature", "", "Override sampling temperature (subject to frozen policy)")
	f.StringVar(&opts.maxTokensOverride, "max-tokens", "", "Override output limit (subject to frozen policy)")
	f.StringVar(&opts.topPOverride, "top-p", "", "Override sampling top-p")
	f.StringVar(&opts.topKOverride, "top-k", "", "Override sampling top-k")
	f.StringVar(&opts.reasoningEffortOverride, "reasoning-effort", "", "Override model reasoning effort")
	f.StringVar(&opts.sidecarModelOverride, "sidecar-model", "", "Override sidecar target")
	f.StringVar(&opts.guardModelOverride, "guard-model", "", "Override guard target")
	f.StringVar(&opts.judgeModelOverride, "judge-model", "", "Override judge target")
	f.StringVar(&opts.planReviewerModelOverride, "plan-reviewer-model", "", "Override plan reviewer target")
	f.Int64Var(&opts.verifyTimeoutOverride, "verify-timeout", 0, "Override verification timeout in seconds")
	f.IntVar(&opts.maxRoundsOverride, "max-rounds", 0, "Override coordinator round limit")
	f.IntVar(&opts.maxConcurrentOverride, "max-concurrent", 0, "Override worker concurrency")
	f.IntVar(&opts.maxStepsOverride, "max-steps", 0, "Override worker step budget")
	f.Int64Var(&opts.maxDuration, "max-duration", 0, "Override run duration budget in seconds (excluding browser dwell and verification waits)")
	f.Int64Var(&opts.maxTotalTokens, "max-total-tokens", 0, "Override total generation budget")
	f.BoolVar(&opts.rbashMode, "rbash", false, "Use restricted bash")
	f.BoolVar(&opts.planMode, "plan", false, "Require a plan before execution")
	f.BoolVar(&opts.autoSkills, "auto-skills", false, "Enable automatic skill matching")
	f.BoolVar(&opts.noJournal, "no-journal", false, "Disable task-result journaling")
}
