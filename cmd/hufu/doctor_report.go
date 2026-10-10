package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

type doctorCheck struct {
	ID         string `json:"id"`
	Subject    string `json:"subject,omitempty"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	Count      *int   `json:"count,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	HTTPStatus int    `json:"http_status,omitzero"`
	ModelState string `json:"model_state,omitempty"`
}

type doctorReport struct {
	SchemaVersion int           `json:"schema_version"`
	Status        string        `json:"status"`
	Checks        []doctorCheck `json:"checks"`
}

func (report *doctorReport) add(id, subject, status, message string) {
	report.Checks = append(report.Checks, doctorCheck{ID: id, Subject: subject, Status: status, Message: message})
}

func (report *doctorReport) addCount(id, status, message string, count int) {
	report.Checks = append(report.Checks, doctorCheck{ID: id, Status: status, Message: message, Count: &count})
}

func (report *doctorReport) finish() {
	report.Status = "ready"
	for _, check := range report.Checks {
		if check.Status == "fail" {
			report.Status = "failed"
			break
		}
		if check.Status == "warning" || check.Status == "unknown" {
			report.Status = "degraded"
		}
	}
	slices.SortFunc(report.Checks, func(a, b doctorCheck) int {
		if order := cmp.Compare(a.ID, b.ID); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Subject, b.Subject); order != 0 {
			return order
		}
		return cmp.Compare(a.Message, b.Message)
	})
}

func runDoctorReport(cmd *cobra.Command, _ []string) error {
	asJSON, err := cmd.Flags().GetBool("json")
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	report := collectDoctorReport(ctx)
	if asJSON {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
			return err
		}
	} else if err := renderDoctorReportText(cmd.ErrOrStderr(), report); err != nil {
		return err
	}
	if report.Status == "failed" {
		return fmt.Errorf("doctor: preflight checks failed")
	}
	return nil
}

func renderDoctorReportText(writer io.Writer, report doctorReport) error {
	if _, err := fmt.Fprintln(writer, "─── hufu doctor ───"); err != nil {
		return err
	}
	for _, check := range report.Checks {
		marker := "✓"
		switch check.Status {
		case "fail":
			marker = "✗"
		case "warning", "unknown":
			marker = "⚠"
		}
		label := check.ID
		if check.Subject != "" {
			label += " (" + check.Subject + ")"
		}
		if _, err := fmt.Fprintf(writer, "  %s %s: %s\n", marker, label, check.Message); err != nil {
			return err
		}
	}
	var summary string
	switch report.Status {
	case "degraded":
		summary = "Ready with warnings."
	case "failed":
		summary = "Some checks failed — fix the items above before running a task."
	default:
		summary = "Ready to call agents."
	}
	_, err := fmt.Fprintln(writer, summary)
	return err
}

func collectDoctorReport(ctx context.Context) doctorReport {
	report := doctorReport{SchemaVersion: 1, Checks: []doctorCheck{}}
	cfg := config.LoadConfig()
	providerURL := config.ResolveProviderURL(opts.providerURL, "", "")
	models, err := fetchModelsContext(ctx, providerURL, config.ResolveProviderAPIKey(opts.providerAPIKey, ""))
	switch {
	case err != nil:
		report.add("provider.reachable", "", "fail", "configured provider is unreachable")
		if failure, ok := errors.AsType[*doctorProviderError](err); ok {
			check := &report.Checks[len(report.Checks)-1]
			check.Message, check.ReasonCode, check.HTTPStatus = failure.Message, failure.Code, failure.HTTPStatus
		}
	case len(models) == 0:
		report.add("provider.reachable", "", "warning", "provider is reachable but reports no models")
	default:
		report.add("provider.reachable", "", "pass", fmt.Sprintf("provider is reachable; %d model(s) available", len(models)))
	}
	collectDoctorModels(&report, cfg, models)
	collectDoctorWorkspace(ctx, &report, getWorkspace())
	collectDoctorTeams(&report, cfg)
	report.finish()
	return report
}

func collectDoctorModels(report *doctorReport, cfg *config.Config, available []string) {
	overrides, err := currentModelOverrides()
	if err != nil {
		report.add("models.configuration", "", "fail", "model override configuration is invalid")
		return
	}
	for _, role := range resolveRoleModelSources(agent.TeamConfig{}, "team.yaml", cfg, overrides) {
		subject := strings.ToLower(role.Role)
		check := doctorCheck{ID: "models.resolved", Subject: subject, Status: "pass", ModelState: "configured", Message: "model target is configured"}
		switch {
		case role.Target == "":
			check.ModelState = "deferred"
			check.Message = "model not set at this level; team or agent may select one"
		case len(available) > 0 && !modelAvailable(providerModelName(role.Target), available):
			check.Status = "warning"
			if role.Role == "Worker" || role.Role == "Coordinator" {
				check.Status = "fail"
			}
			check.Message = "configured model is not in the provider model list"
		}
		report.Checks = append(report.Checks, check)
	}
}

func collectDoctorTeams(report *doctorReport, cfg *config.Config) {
	registry := team.NewTeamRegistry(resolveSearchPaths())
	if err := registry.Discover(); err != nil {
		report.add("teams.discovery", "", "warning", "team discovery did not complete")
		return
	}
	if registry.TeamCount() == 0 {
		report.add("teams.discovery", "", "warning", "no teams found; use --default or hufu init")
		return
	}
	report.add("teams.discovery", "", "pass", fmt.Sprintf("%d team(s) found", registry.TeamCount()))
	projectDir, projectErr := os.Getwd()
	if projectErr != nil {
		report.add("teams.contract", "", "fail", "runtime project directory is unavailable")
		return
	}
	findings := 0
	for _, name := range registry.ListTeams() {
		dir, err := registry.Resolve(name)
		if err != nil {
			report.add("teams.contract", name, "fail", "team location could not be resolved")
			findings++
			continue
		}
		session, err := team.LoadTeam(dir, nil, nil, team.DefaultProviderRegistry)
		if err != nil {
			report.add("teams.contract", name, "fail", "team contract could not be loaded")
			findings++
			continue
		}
		if team.HasAuthoredLegacyLocalExecutionBackend(session) {
			report.add("teams.contract", name, "warning", "legacy local execution backend alias is in use")
			findings++
		}
		if err := validateDoctorExecutionTargets(session, cfg, nil); err != nil {
			report.add("teams.contract", name, "fail", "execution target preflight failed")
			findings++
			continue
		}
		for _, finding := range collectDoctorContractFindings(session, projectDir) {
			status := "warning"
			if finding.Finding.Severity == team.FindingSeverityError {
				status = "fail"
			}
			report.add("teams.contract", name+":"+finding.Location, status, "contract finding: "+finding.Finding.Code)
			findings++
		}
	}
	if findings == 0 {
		report.add("teams.contract", "", "pass", "all team verifier contracts are valid and asserting")
	}
}

func collectDoctorWorkspace(ctx context.Context, report *doctorReport, workspace string) {
	if workspace == "" {
		report.add("workspace.writable", "", "fail", "no active managed workspace")
		report.add("events.integrity", "", "unknown", "no active workspace to inspect")
		report.add("recovery.unresolved", "", "unknown", "no active workspace to inspect")
		return
	}
	if err := checkWritable(workspace); err != nil {
		report.add("workspace.writable", "", "fail", "workspace is not writable")
	} else {
		report.add("workspace.writable", "", "pass", "workspace is writable")
	}
	var events []team.RunEvent
	if err := team.StreamValidatedRunEvents(ctx, workspace, func(event team.RunEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		report.add("events.integrity", "", "fail", "event history failed complete integrity validation")
		report.add("recovery.unresolved", "", "unknown", "recovery count unavailable because event history is invalid")
		return
	}
	if len(events) == 0 {
		report.add("events.integrity", "", "pass", "no recorded event history")
	} else {
		report.add("events.integrity", "", "pass", fmt.Sprintf("%d event(s) passed complete integrity validation", len(events)))
	}
	session, exists, err := team.LoadSessionReadOnly(workspace)
	if err != nil {
		report.add("recovery.unresolved", "", "fail", "session checkpoint is unreadable or malformed")
		return
	}
	tree, err := loadDoctorSessionTree(workspace)
	if err != nil {
		report.add("recovery.unresolved", "", "fail", "active branch lineage is invalid")
		return
	}
	lineage, err := team.ProjectValidatedEventsForBranch(events, tree, tree.ActiveBranch)
	if err != nil {
		report.add("recovery.unresolved", "", "fail", "active branch lineage is invalid")
		return
	}
	if !exists {
		if len(events) == 0 {
			report.add("recovery.unresolved", "", "pass", "no prior session")
		} else {
			report.add("recovery.unresolved", "", "unknown", "event history exists but session checkpoint is absent")
		}
		return
	}
	items := session.Tasks
	if len(events) > 0 {
		items, err = team.ReplayTodoList(lineage)
		if err != nil {
			report.add("recovery.unresolved", "", "fail", "active branch task projection is invalid")
			return
		}
		if len(items) == 0 && len(session.Tasks) > 0 {
			report.add("recovery.unresolved", "", "unknown", "active branch has no replayable task projection")
			return
		}
	}
	count := 0
	for _, item := range items {
		if item == nil || item.Status == team.TaskDone || item.Status == team.TaskError || item.Status == team.TaskSkipped {
			continue
		}
		switch item.SideEffect {
		case team.SideEffectExternalWrite, team.SideEffectInfraMutation, team.SideEffectCredential, team.SideEffectUnknown:
			if item.RecoveryState == team.RecoveryStatePartial || item.RecoveryState == team.RecoveryStateUnknown {
				count++
			}
		}
	}
	if count > 0 {
		report.addCount("recovery.unresolved", "warning", fmt.Sprintf("%d unresolved high-risk task(s)", count), count)
	} else {
		report.addCount("recovery.unresolved", "pass", "0 unresolved high-risk tasks", 0)
	}
}

func loadDoctorSessionTree(workspace string) (*team.SessionTree, error) {
	data, err := os.ReadFile(filepath.Join(workspace, "session_tree.json"))
	if os.IsNotExist(err) {
		return team.NewSessionTree(), nil
	}
	if err != nil {
		return nil, err
	}
	var tree team.SessionTree
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, err
	}
	if tree.ActiveBranch == "" || tree.Branches[tree.ActiveBranch] == nil {
		return nil, fmt.Errorf("active branch is absent")
	}
	return &tree, nil
}
