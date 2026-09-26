package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/config"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

// Check IDs and reason codes shared with `hufu team check`.
const (
	staticCheckRoleTargets        = "static.role_targets"
	staticCheckExecutionPreflight = "static.execution_preflight"
	staticCheckEffectiveContract  = "static.effective_contract"
)

// startupStaticGate is what loadTeamCommon needs from its read-only setup
// gate once every check passed.
type startupStaticGate struct {
	RoleModels   team.RoleModels
	AllowedPaths []string
	ForceMCP     bool
	NoNet        bool
}

// runStartupStaticGate runs the read-only startup checks: role target
// resolution, execution-target preflight, and the effective-contract lint.
// Independent checks all run, so one failed startup reports every static
// problem at once. The target preflight needs resolved roles and is skipped
// when they fail. Nothing here opens a store, starts a process, or contacts a
// provider; checks with side effects keep their own gates.
func runStartupStaticGate(session *team.TeamSession, registry *team.TeamRegistry, cfg *config.Config, execProfile team.ExecutionProfile, planMode bool) (startupStaticGate, error) {
	var checks []staticCheckOutcome
	roleModels, err := resolveExecutionRoleModels(session, cfg)
	if err != nil {
		checks = append(checks, failedStaticCheck(staticCheckRoleTargets, "role_target_invalid", err))
		checks = append(checks, staticCheckOutcome{item: TeamCheckItem{
			ID: staticCheckExecutionPreflight, Category: "static", Status: "skipped", Required: true,
			ReasonCode: "dependency_unavailable", Message: "Execution targets need resolved role targets.",
		}})
	} else if err := preflightExecutionTargets(session, cfg, roleModels, nil); err != nil {
		checks = append(checks, failedStaticCheck(staticCheckExecutionPreflight, "execution_target_invalid", err))
	}

	gate := startupStaticGate{
		RoleModels:   roleModels,
		AllowedPaths: buildAllowedPaths(session, registry, cfg),
		ForceMCP:     opts.forceMCP || cfg.ForceMCP || session.Config.ForceMCP,
		NoNet:        opts.noNet || cfg.NoNet || session.Config.NoNet,
	}
	findings := team.LintEffectiveTeamContracts(session, team.EffectiveTeamContractContext{
		Unattended:        opts.unattended || session.Config.Unattended || execProfile.IsUnattended(),
		ForceMCP:          gate.ForceMCP,
		NoNet:             gate.NoNet,
		PlanFirst:         &planMode,
		AllowedPaths:      gate.AllowedPaths,
		EnvironmentLookup: os.LookupEnv,
	})
	if messages := contractErrorMessages(findings); len(messages) > 0 {
		err := fmt.Errorf("effective team contract validation failed: %s", strings.Join(messages, "; "))
		checks = append(checks, failedStaticCheck(staticCheckEffectiveContract, "effective_contract_invalid", err))
	}
	return gate, staticGateError(checks)
}

func contractErrorMessages(findings []team.ContractFinding) []string {
	messages := make([]string, 0, len(findings))
	for _, finding := range findings {
		if finding.Severity == team.FindingSeverityError {
			messages = append(messages, fmt.Sprintf("%s: %s (%s)", finding.Field, finding.Message, finding.Code))
		}
	}
	slices.Sort(messages)
	return messages
}

// staticCheckOutcome pairs a check result with the error that failed it.
type staticCheckOutcome struct {
	item TeamCheckItem
	err  error
}

func failedStaticCheck(id, reasonCode string, err error) staticCheckOutcome {
	return staticCheckOutcome{
		item: TeamCheckItem{ID: id, Category: "static", Status: "failed", Required: true, ReasonCode: reasonCode, Message: err.Error()},
		err:  err,
	}
}

// staticGateError returns nil when no required check failed. A single
// failure keeps its original error; several become one startupCheckError.
func staticGateError(checks []staticCheckOutcome) error {
	slices.SortStableFunc(checks, func(left, right staticCheckOutcome) int {
		return strings.Compare(left.item.ID, right.item.ID)
	})
	var failed []error
	for _, check := range checks {
		if check.err != nil {
			failed = append(failed, check.err)
		}
	}
	switch len(failed) {
	case 0:
		return nil
	case 1:
		return failed[0]
	}
	items := make([]TeamCheckItem, 0, len(checks))
	for _, check := range checks {
		items = append(items, check.item)
	}
	return &startupCheckError{checks: items, errs: failed}
}

// startupCheckError reports every failed startup check, ordered by check ID.
// It unwraps to each underlying error so errors.Is and errors.As still see
// them.
type startupCheckError struct {
	checks []TeamCheckItem
	errs   []error
}

func (e *startupCheckError) Error() string {
	var text strings.Builder
	fmt.Fprintf(&text, "%d startup checks failed:", len(e.errs))
	for _, check := range e.checks {
		fmt.Fprintf(&text, "\n  - %s [%s, %s]: %s", check.ID, check.Status, check.ReasonCode, operatorpkg.SafeDisplayText(check.Message, 400))
	}
	return text.String()
}

func (e *startupCheckError) Unwrap() []error { return e.errs }
