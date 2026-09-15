package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

type TeamCheckDocument struct {
	SchemaVersion     int                           `json:"schema_version"`
	Kind              string                        `json:"kind"`
	Team              string                        `json:"team"`
	Operation         inspectpkg.OperationView      `json:"operation"`
	StaticReadiness   string                        `json:"static_readiness"`
	OnlineReadiness   string                        `json:"online_readiness"`
	LearningReadiness string                        `json:"learning_readiness"`
	Checks            []TeamCheckItem               `json:"checks"`
	NextAction        *operatorpkg.ActionSuggestion `json:"next_action"`
	Error             *inspectpkg.OperatorErrorView `json:"error"`
}

type TeamCheckItem struct {
	ID         string `json:"id"`
	Category   string `json:"category"`
	Status     string `json:"status"`
	Required   bool   `json:"required"`
	ReasonCode string `json:"reason_code"`
	Message    string `json:"message"`
}

type teamCheckOptions struct {
	online, json bool
	output       string
	searchPath   string
}

type teamCheckExitError struct {
	code int
	err  error
}

func (e *teamCheckExitError) Error() string        { return e.err.Error() }
func (e *teamCheckExitError) Unwrap() error        { return e.err }
func (e *teamCheckExitError) ProcessExitCode() int { return e.code }

func newTeamCheckCommand() *cobra.Command {
	options := new(teamCheckOptions)
	command := &cobra.Command{
		Use:               "check <team>",
		Short:             "Check deterministic team readiness without running it",
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, args []string) error {
			format, err := resolveTeamCheckOutput(command, options)
			if err != nil {
				return finishTeamCheckUsage(command, format, "", err)
			}
			if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
				return finishTeamCheckUsage(command, format, "", fmt.Errorf("team check requires exactly one team name"))
			}
			return runTeamCheck(command, strings.TrimPrefix(strings.TrimSpace(args[0]), "@"), options, format)
		},
	}
	flags := command.Flags()
	flags.BoolVar(&options.online, "online", false, "Opt in to bounded provider model-list checks")
	flags.StringVar(&options.output, "output", "text", "Output format: text or json")
	flags.BoolVar(&options.json, "json", false, "Legacy alias for --output json")
	flags.StringVar(&options.searchPath, "agent-team-search-path", "", "Comma-separated team search paths")
	registerStaticFlagCompletion(command, "output", []string{"text", "json"})
	command.SetFlagErrorFunc(func(command *cobra.Command, err error) error {
		format := "text"
		if options.json || options.output == "json" {
			format = "json"
		}
		return finishTeamCheckUsage(command, format, "", err)
	})
	return command
}

func resolveTeamCheckOutput(command *cobra.Command, options *teamCheckOptions) (string, error) {
	format, err := resolveOutputAlias("output", options.output, flagChanged(command, "output"), "json", outputForJSONAlias(options.json), flagChanged(command, "json"), "text", []string{"text", "json"})
	if err != nil && options.json {
		return "json", err
	}
	return format, err
}

func runTeamCheck(command *cobra.Command, name string, options *teamCheckOptions, format string) error {
	paths := resolveSearchPaths()
	if strings.TrimSpace(options.searchPath) != "" {
		paths = strings.Split(options.searchPath, ",")
	}
	registry := team.NewTeamRegistry(paths)
	if err := registry.Discover(); err != nil {
		return finishTeamCheckUsage(command, format, name, fmt.Errorf("discover teams: %w", err))
	}
	teamDir, err := registry.Resolve(name)
	if err != nil {
		return finishTeamCheckUsage(command, format, name, err)
	}

	document := TeamCheckDocument{
		SchemaVersion: 1, Kind: "team_check", Team: name,
		Operation:       inspectpkg.OperationView{Status: "succeeded"},
		StaticReadiness: "passed", OnlineReadiness: "not_checked", LearningReadiness: "not_applicable",
		Checks: []TeamCheckItem{},
	}
	lintResult, err := team.LintTeamWithOptions(teamDir, team.TeamLintOptions{Registry: team.DefaultProviderRegistry})
	if err != nil {
		return finishTeamCheckUsage(command, format, name, fmt.Errorf("load team: %w", err))
	}
	document.Team = lintResult.Team
	document.Checks = append(document.Checks, TeamCheckItem{ID: "static.compiler", Category: "static", Status: "passed", Required: true, ReasonCode: "team_compiled", Message: "Team compiler and schema checks completed."})
	if len(lintResult.Findings) == 0 {
		document.Checks = append(document.Checks, TeamCheckItem{ID: "static.lint", Category: "static", Status: "passed", Required: true, ReasonCode: "no_findings", Message: "Deterministic team lint found no issues."})
	}
	for index, finding := range lintResult.Findings {
		status := "warning"
		required := false
		if finding.Severity == team.FindingSeverityError {
			status, required = "failed", true
		}
		document.Checks = append(document.Checks, TeamCheckItem{
			ID: fmt.Sprintf("static.lint.%s.%03d", finding.Code, index+1), Category: "static", Status: status,
			Required: required, ReasonCode: finding.Code, Message: finding.Message,
		})
	}

	session, err := team.LoadTeam(teamDir, nil, nil, team.DefaultProviderRegistry)
	if err != nil {
		return finishTeamCheckUsage(command, format, name, fmt.Errorf("load team targets: %w", err))
	}
	cfg := config.LoadConfig()
	roles, roleErr := resolveExecutionRoleModels(session, cfg)
	if roleErr != nil {
		document.Checks = append(document.Checks, TeamCheckItem{ID: "static.role_targets", Category: "static", Status: "failed", Required: true, ReasonCode: "role_target_invalid", Message: roleErr.Error()})
	} else {
		document.Checks = append(document.Checks, roleTargetCheckItems(session, roles)...)
		if err := validateDoctorExecutionTargets(session, cfg, nil); err != nil {
			document.Checks = append(document.Checks, TeamCheckItem{ID: "static.execution_preflight", Category: "static", Status: "failed", Required: true, ReasonCode: "execution_target_invalid", Message: err.Error()})
		} else {
			document.Checks = append(document.Checks, TeamCheckItem{ID: "static.execution_preflight", Category: "static", Status: "passed", Required: true, ReasonCode: "execution_targets_resolved", Message: "All configured execution targets resolve without starting a provider."})
		}
	}

	if options.online {
		document.Checks = append(document.Checks, onlineTeamCheckItems(command.Context(), session, cfg, roles, roleErr)...)
	} else {
		document.Checks = append(document.Checks, TeamCheckItem{ID: "online.provider_models", Category: "online", Status: "skipped", Required: false, ReasonCode: "online_not_requested", Message: "Online provider checks require --online."})
	}

	slices.SortFunc(document.Checks, func(left, right TeamCheckItem) int {
		if compared := strings.Compare(left.Category, right.Category); compared != 0 {
			return compared
		}
		return strings.Compare(left.ID, right.ID)
	})
	for index := range document.Checks {
		document.Checks[index].Message = operatorpkg.SafeDisplayText(document.Checks[index].Message, 400)
	}
	document.Team = operatorpkg.SafeDisplayText(document.Team, 120)
	document.StaticReadiness = readinessFor(document.Checks, "static")
	if options.online {
		document.OnlineReadiness = readinessFor(document.Checks, "online")
	}
	return finishTeamCheck(command, format, document)
}

type onlineProviderTarget struct {
	name, url, key string
	required       bool
}

func onlineTeamCheckItems(parent context.Context, session *team.TeamSession, cfg *config.Config, roles team.RoleModels, roleErr error) []TeamCheckItem {
	targets := onlineProviderTargets(session, cfg, roles)
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	modelsByBackend := make(map[string][]string, len(targets))
	items := make([]TeamCheckItem, 0, len(targets)+6)
	for _, target := range targets {
		if strings.TrimSpace(target.url) == "" {
			items = append(items, TeamCheckItem{ID: "online.provider." + target.name, Category: "online", Status: "unknown", Required: target.required, ReasonCode: "provider_url_missing", Message: fmt.Sprintf("Provider %s has no introspection URL.", target.name)})
			continue
		}
		models, err := fetchModelsContext(ctx, target.url, target.key)
		if err != nil {
			items = append(items, TeamCheckItem{ID: "online.provider." + target.name, Category: "online", Status: "unknown", Required: target.required, ReasonCode: "provider_unreachable", Message: fmt.Sprintf("Provider %s model list could not be verified within the bounded check.", target.name)})
			continue
		}
		modelsByBackend[target.name] = models
		items = append(items, TeamCheckItem{ID: "online.provider." + target.name, Category: "online", Status: "passed", Required: target.required, ReasonCode: "provider_models_verified", Message: fmt.Sprintf("Provider %s reported %d model(s).", target.name, len(models))})
	}
	if roleErr != nil {
		return items
	}
	for _, role := range resolvedRoleTargets(session, roles) {
		selector, err := execution.ParseExecutionSelector(role.target)
		if err != nil || selector.Model == "" {
			continue
		}
		backend := selector.Backend
		if backend == "" {
			backend = session.Config.DefaultLLMBackend
			if backend == "" {
				backend = execution.OllamaBackendName
			}
		}
		available, checked := modelsByBackend[backend]
		if !checked {
			continue
		}
		status, reason := "passed", "role_model_available"
		if !modelAvailable(selector.Model, available) {
			status, reason = "failed", "role_model_missing"
		}
		items = append(items, TeamCheckItem{ID: "online.role." + role.role, Category: "online", Status: status, Required: role.required, ReasonCode: reason, Message: fmt.Sprintf("%s effective target: %s", role.role, role.target)})
	}
	return items
}

func onlineProviderTargets(session *team.TeamSession, cfg *config.Config, roles team.RoleModels) []onlineProviderTarget {
	required := map[string]bool{}
	for _, role := range resolvedRoleTargets(session, roles) {
		selector, err := execution.ParseExecutionSelector(role.target)
		if err != nil {
			continue
		}
		backend := selector.Backend
		if backend == "" {
			backend = session.Config.DefaultLLMBackend
			if backend == "" {
				backend = execution.OllamaBackendName
			}
		}
		if role.required {
			required[backend] = true
		}
	}
	targets := []onlineProviderTarget{{
		name: execution.OllamaBackendName, url: config.ResolveProviderURL("", session.Config.ProviderURL, ""),
		key: config.ResolveProviderAPIKey("", session.Config.ProviderAPIKey), required: required[execution.OllamaBackendName],
	}}
	for name, provider := range session.Config.Providers {
		if name == execution.OllamaBackendName {
			if provider.ProviderURL != "" {
				targets[0].url = provider.ProviderURL
			}
			if provider.ProviderAPIKey != "" {
				targets[0].key = provider.ProviderAPIKey
			}
			continue
		}
		targets = append(targets, onlineProviderTarget{name: name, url: provider.ProviderURL, key: provider.ProviderAPIKey, required: required[name]})
	}
	slices.SortFunc(targets, func(left, right onlineProviderTarget) int { return strings.Compare(left.name, right.name) })
	return targets
}

type resolvedRoleTarget struct {
	role, target string
	required     bool
}

func resolvedRoleTargets(session *team.TeamSession, roles team.RoleModels) []resolvedRoleTarget {
	return []resolvedRoleTarget{
		{role: "worker", target: session.Config.WorkerModel, required: true},
		{role: "coordinator", target: session.Config.CoordinatorModel, required: true},
		{role: "sidecar", target: roles.Sidecar},
		{role: "guard", target: roles.Guard},
		{role: "judge", target: roles.Judge},
		{role: "plan_reviewer", target: roles.PlanReviewer},
	}
}

func roleTargetCheckItems(session *team.TeamSession, roles team.RoleModels) []TeamCheckItem {
	values := []struct{ role, target string }{
		{"worker", session.Config.WorkerModel}, {"coordinator", session.Config.CoordinatorModel},
		{"sidecar", roles.Sidecar}, {"guard", roles.Guard}, {"judge", roles.Judge}, {"plan_reviewer", roles.PlanReviewer},
	}
	items := make([]TeamCheckItem, 0, len(values))
	for _, value := range values {
		status, required, reason := "passed", value.role == "worker" || value.role == "coordinator", "role_target_resolved"
		if strings.TrimSpace(value.target) == "" {
			status, reason = "not_applicable", "role_not_configured"
			if required {
				status, reason = "failed", "required_role_missing"
			}
		}
		items = append(items, TeamCheckItem{ID: "static.role." + value.role, Category: "static", Status: status, Required: required, ReasonCode: reason, Message: fmt.Sprintf("%s requested/effective target: %s", value.role, displayTarget(value.target))})
	}
	return items
}

func displayTarget(value string) string {
	if strings.TrimSpace(value) == "" {
		return "not configured"
	}
	return value
}

func readinessFor(checks []TeamCheckItem, category string) string {
	status := "passed"
	for _, check := range checks {
		if check.Category != category || !check.Required {
			continue
		}
		if check.Status == "failed" {
			return "failed"
		}
		if check.Status == "unknown" {
			return "failed"
		}
	}
	return status
}

func finishTeamCheckUsage(command *cobra.Command, format, name string, err error) error {
	safeMessage := operatorpkg.SafeDisplayText(err.Error(), 400)
	document := TeamCheckDocument{
		SchemaVersion: 1, Kind: "team_check", Team: operatorpkg.SafeDisplayText(name, 120), Operation: inspectpkg.OperationView{Status: "failed"},
		StaticReadiness: "unknown", OnlineReadiness: "not_checked", LearningReadiness: "not_applicable", Checks: []TeamCheckItem{},
		Error: &inspectpkg.OperatorErrorView{Kind: "usage", Code: "invalid_request", Message: safeMessage},
	}
	command.Root().SilenceErrors, command.Root().SilenceUsage = true, true
	if format == "json" {
		_ = writeTeamCheckJSON(command.OutOrStdout(), document)
	}
	return &teamCheckExitError{code: 2, err: errors.New(safeMessage)}
}

func finishTeamCheck(command *cobra.Command, format string, document TeamCheckDocument) error {
	if format == "json" {
		if err := writeTeamCheckJSON(command.OutOrStdout(), document); err != nil {
			return &teamCheckExitError{code: 2, err: err}
		}
	} else if err := writeTeamCheckText(command.OutOrStdout(), document); err != nil {
		return &teamCheckExitError{code: 2, err: err}
	}
	if document.StaticReadiness == "failed" || document.StaticReadiness == "unknown" || document.OnlineReadiness == "failed" || document.OnlineReadiness == "unknown" {
		command.Root().SilenceErrors, command.Root().SilenceUsage = true, true
		return &teamCheckExitError{code: 1, err: fmt.Errorf("team %s is not ready", document.Team)}
	}
	return nil
}

func writeTeamCheckJSON(writer io.Writer, document TeamCheckDocument) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

func writeTeamCheckText(writer io.Writer, document TeamCheckDocument) error {
	if _, err := fmt.Fprintf(writer, "Team: %s\nStatic readiness: %s\nOnline readiness: %s\nLearning readiness: %s\n", document.Team, document.StaticReadiness, document.OnlineReadiness, document.LearningReadiness); err != nil {
		return err
	}
	for _, check := range document.Checks {
		if _, err := fmt.Fprintf(writer, "%s [%s] %s: %s\n", check.Category, check.Status, check.ID, check.Message); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(writer, "Next: %s\n", teamCheckNextText(document)); err != nil {
		return err
	}
	return nil
}

func teamCheckNextText(document TeamCheckDocument) string {
	if document.StaticReadiness == "failed" {
		return "fix required static checks, then rerun team check"
	}
	if document.OnlineReadiness == "not_checked" {
		return fmt.Sprintf("hufu team check %s --online", document.Team)
	}
	if document.OnlineReadiness == "failed" {
		return "fix required online checks, then rerun team check --online"
	}
	return fmt.Sprintf("hufu run --team %s -- <task>", document.Team)
}
