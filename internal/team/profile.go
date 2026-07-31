package team

import (
	"fmt"
	"strings"
)

// ExecutionProfileName represents the named execution profile.
type ExecutionProfileName string

const (
	ProfileDefault            ExecutionProfileName = "default"
	ProfileUnattended         ExecutionProfileName = "unattended"
	ProfileStrictVerification ExecutionProfileName = "strict-verification"
	ProfileFreshVerification  ExecutionProfileName = "fresh-verification"
)

// PolicyFailureMode determines how policy checks (guards, hooks) behave on failure/error.
type PolicyFailureMode string

const (
	PolicyFailOpen   PolicyFailureMode = "open"
	PolicyFailClosed PolicyFailureMode = "closed"
)

// AcceptanceMode specifies whether acceptance failure blocks execution or provides advice.
type AcceptanceMode string

const (
	AcceptanceAdvisory AcceptanceMode = "advisory"
	AcceptanceBlocking AcceptanceMode = "blocking"
)

// ExecutionProfile defines a named bundle of execution, verification, security,
// cache, and memory governance policies.
type ExecutionProfile struct {
	SchemaVersion int                  `json:"schema_version,omitempty" yaml:"schema-version,omitempty"`
	Name          ExecutionProfileName `json:"name,omitempty" yaml:"name,omitempty"`

	StrictPolicy      bool              `json:"strict_policy,omitempty" yaml:"strict-policy,omitempty"`
	PolicyFailureMode PolicyFailureMode `json:"policy_failure_mode,omitempty" yaml:"policy-failure-mode,omitempty"`
	HookFailureMode   PolicyFailureMode `json:"hook_failure_mode,omitempty" yaml:"hook-failure-mode,omitempty"`
	AcceptanceMode    AcceptanceMode    `json:"acceptance_mode,omitempty" yaml:"acceptance-mode,omitempty"`
	DefaultGoalMode   GoalMode          `json:"default_goal_mode,omitempty" yaml:"default-goal-mode,omitempty"`

	RequireLockedResources    bool `json:"require_locked_resources,omitempty" yaml:"require-locked-resources,omitempty"`
	RequireEvidenceManifest   bool `json:"require_evidence_manifest,omitempty" yaml:"require-evidence-manifest,omitempty"`
	RequireClosedTerminals    bool `json:"require_closed_terminals,omitempty" yaml:"require-closed-terminals,omitempty"`
	RequireWorkspaceIsolation bool `json:"require_workspace_isolation,omitempty" yaml:"require-workspace-isolation,omitempty"`

	DefaultCachePolicy    CachePolicy    `json:"default_cache_policy,omitempty" yaml:"default-cache-policy,omitempty"`
	DefaultRecoveryPolicy RecoveryPolicy `json:"default_recovery_policy,omitempty" yaml:"default-recovery-policy,omitempty"`

	DisableHistoricalTaskReuse bool `json:"disable_historical_task_reuse,omitempty" yaml:"disable-historical-task-reuse,omitempty"`
	DisableHistoricalMemory    bool `json:"disable_historical_memory,omitempty" yaml:"disable-historical-memory,omitempty"`
	DisableSemanticDedup       bool `json:"disable_semantic_dedup,omitempty" yaml:"disable-semantic-dedup,omitempty"`
	DisableTaskCache           bool `json:"disable_task_cache,omitempty" yaml:"disable-task-cache,omitempty"`
	DisableJournalRestore      bool `json:"disable_journal_restore,omitempty" yaml:"disable-journal-restore,omitempty"`

	FailOnUnknownState    bool `json:"fail_on_unknown_state,omitempty" yaml:"fail-on-unknown-state,omitempty"`
	AntiThrashingEnforced bool `json:"anti_thrashing_enforced,omitempty" yaml:"anti-thrashing-enforced,omitempty"`
}

// IsUnattended returns whether the profile is the unattended profile.
func (p ExecutionProfile) IsUnattended() bool {
	return p.Name == ProfileUnattended
}

// BuiltinProfiles returns all pre-defined execution profiles.
func BuiltinProfiles() map[ExecutionProfileName]ExecutionProfile {
	return map[ExecutionProfileName]ExecutionProfile{
		ProfileDefault: {
			SchemaVersion:              1,
			Name:                       ProfileDefault,
			StrictPolicy:               false,
			PolicyFailureMode:          PolicyFailOpen,
			HookFailureMode:            PolicyFailOpen,
			AcceptanceMode:             AcceptanceAdvisory,
			DefaultGoalMode:            GoalModeExploratory,
			RequireLockedResources:     false,
			RequireEvidenceManifest:    false,
			RequireClosedTerminals:     false,
			RequireWorkspaceIsolation:  false,
			DefaultCachePolicy:         CacheUse,
			DefaultRecoveryPolicy:      RecoveryRetry,
			DisableHistoricalTaskReuse: false,
			DisableHistoricalMemory:    false,
			DisableSemanticDedup:       false,
			DisableTaskCache:           false,
			DisableJournalRestore:      false,
			FailOnUnknownState:         false,
			AntiThrashingEnforced:      false,
		},
		ProfileUnattended: {
			SchemaVersion:              1,
			Name:                       ProfileUnattended,
			StrictPolicy:               false,
			PolicyFailureMode:          PolicyFailClosed,
			HookFailureMode:            PolicyFailClosed,
			AcceptanceMode:             AcceptanceBlocking,
			DefaultGoalMode:            GoalModeOutcome,
			RequireLockedResources:     false,
			RequireEvidenceManifest:    false,
			RequireClosedTerminals:     false,
			RequireWorkspaceIsolation:  false,
			DefaultCachePolicy:         CacheUse,
			DefaultRecoveryPolicy:      RecoveryReconcile,
			DisableHistoricalTaskReuse: false,
			DisableHistoricalMemory:    false,
			DisableSemanticDedup:       false,
			DisableTaskCache:           false,
			DisableJournalRestore:      false,
			FailOnUnknownState:         false,
			AntiThrashingEnforced:      true,
		},
		ProfileStrictVerification: {
			SchemaVersion:              1,
			Name:                       ProfileStrictVerification,
			StrictPolicy:               true,
			PolicyFailureMode:          PolicyFailClosed,
			HookFailureMode:            PolicyFailClosed,
			AcceptanceMode:             AcceptanceBlocking,
			DefaultGoalMode:            GoalModeOutcome,
			RequireLockedResources:     true,
			RequireEvidenceManifest:    true,
			RequireClosedTerminals:     true,
			RequireWorkspaceIsolation:  true,
			DefaultCachePolicy:         CacheUse,
			DefaultRecoveryPolicy:      RecoveryReconcile,
			DisableHistoricalTaskReuse: false,
			DisableHistoricalMemory:    false,
			DisableSemanticDedup:       false,
			DisableTaskCache:           false,
			DisableJournalRestore:      false,
			FailOnUnknownState:         true,
			AntiThrashingEnforced:      true,
		},
		ProfileFreshVerification: {
			SchemaVersion:              1,
			Name:                       ProfileFreshVerification,
			StrictPolicy:               true,
			PolicyFailureMode:          PolicyFailClosed,
			HookFailureMode:            PolicyFailClosed,
			AcceptanceMode:             AcceptanceBlocking,
			DefaultGoalMode:            GoalModeOutcome,
			RequireLockedResources:     true,
			RequireEvidenceManifest:    true,
			RequireClosedTerminals:     true,
			RequireWorkspaceIsolation:  true,
			DefaultCachePolicy:         CacheBypass,
			DefaultRecoveryPolicy:      RecoveryReconcile,
			DisableHistoricalTaskReuse: true,
			DisableHistoricalMemory:    true,
			DisableSemanticDedup:       true,
			DisableTaskCache:           true,
			DisableJournalRestore:      true,
			FailOnUnknownState:         true,
			AntiThrashingEnforced:      true,
		},
	}
}

// GetBuiltinProfile looks up a builtin profile by name (case-insensitive).
func GetBuiltinProfile(name string) (ExecutionProfile, bool) {
	lowerName := strings.ToLower(strings.TrimSpace(name))
	if lowerName == "" {
		lowerName = string(ProfileDefault)
	}
	for k, v := range BuiltinProfiles() {
		if strings.ToLower(string(k)) == lowerName {
			return v, true
		}
	}
	return ExecutionProfile{}, false
}

// ResolveExecutionProfile resolves the active profile using precedence:
// 1. Explicit CLI profile flag (cliProfile)
// 2. Team configuration execution-profile (teamProfile)
// 3. Builtin default profile ("default")
func ResolveExecutionProfile(cliProfile, teamProfile string) (ExecutionProfile, error) {
	chosen := strings.TrimSpace(cliProfile)
	source := "CLI flag"
	if chosen == "" {
		chosen = strings.TrimSpace(teamProfile)
		source = "team.yml"
	}
	if chosen == "" {
		chosen = string(ProfileDefault)
		source = "default"
	}

	p, ok := GetBuiltinProfile(chosen)
	if !ok {
		return ExecutionProfile{}, fmt.Errorf("unknown execution profile %q specified via %s (available: default, unattended, strict-verification, fresh-verification)", chosen, source)
	}
	return p, nil
}

// ResolveEffectiveGoalMode applies the same precedence as coordinator setup:
// an explicit goal mode wins; otherwise the selected execution profile's
// default goal mode is used. This must be resolved before validating an
// acceptance contract, because an omitted mode is not implicitly outcome.
func ResolveEffectiveGoalMode(goalMode, executionProfile string) (GoalMode, error) {
	if strings.TrimSpace(goalMode) != "" {
		return ParseGoalMode(goalMode)
	}
	profile, err := ResolveExecutionProfile("", executionProfile)
	if err != nil {
		return "", err
	}
	if profile.DefaultGoalMode != "" {
		return profile.DefaultGoalMode, nil
	}
	return GoalModeOutcome, nil
}
