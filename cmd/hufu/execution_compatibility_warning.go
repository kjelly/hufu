package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// executionCompatibilityWarningState scopes both compatibility warnings to a
// single CLI invocation. The observer attached to each coordinator is
// separate: it records an event for each actual run/branch after the durable
// event store is ready.
type executionCompatibilityWarningState struct {
	workspaceArgument string
	workspaceWarned   bool
	authoredWarned    bool
}

func newExecutionCompatibilityWarningState(workspaceArgument string) *executionCompatibilityWarningState {
	return &executionCompatibilityWarningState{workspaceArgument: strings.TrimSpace(workspaceArgument)}
}

func (s *executionCompatibilityWarningState) observeTeam(ctx context.Context, tc *teamContext) {
	if s == nil || tc == nil || tc.session == nil || tc.coordinator == nil {
		return
	}
	if team.HasAuthoredLegacyLocalExecutionBackend(tc.session) {
		s.warnAuthoredAlias()
	}
	// The inspector is intentionally the only detection implementation. It
	// opens no writer and changes no workspace state. A scan failure is not a
	// task-admission failure; normal runtime durability checks retain their
	// existing behavior and the compatibility telemetry is simply unavailable.
	report, err := team.InspectExecutionCompatibility(ctx, tc.session.Workspace, "")
	if err != nil {
		return
	}
	observer := team.NewExecutionCompatibilityObserver(report)
	tc.coordinator.SetExecutionCompatibilityObserver(observer)
	if observer.HasActionableState() {
		s.warnWorkspaceState()
	}
}

func (s *executionCompatibilityWarningState) warnWorkspaceState() {
	if s == nil || s.workspaceWarned {
		return
	}
	s.workspaceWarned = true
	for _, line := range s.workspaceWarningLines() {
		fmt.Fprintln(os.Stderr, line)
	}
}

func (s *executionCompatibilityWarningState) warnAuthoredAlias() {
	if s == nil || s.authoredWarned {
		return
	}
	s.authoredWarned = true
	fmt.Fprintln(os.Stderr, "warning: execution backend alias `local` is deprecated; use `ollama`")
}

func (s *executionCompatibilityWarningState) workspaceWarningLines() []string {
	inspect := "hufu migrate inspect-execution"
	apply := "hufu migrate apply-execution"
	if s != nil && s.workspaceArgument != "" {
		inspect += " --workspace " + s.workspaceArgument
		apply += " --workspace " + s.workspaceArgument
	}
	apply += " --apply"
	return []string{
		"warning: workspace contains deprecated execution identity state; run",
		"`" + inspect + "` and then",
		"`" + apply + "`",
	}
}
