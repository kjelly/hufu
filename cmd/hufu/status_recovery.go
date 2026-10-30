package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
)

type workspaceRecovery struct {
	State    string                         `json:"state"`
	Blockers []operatorpkg.DiagnosticView   `json:"blockers"`
	Actions  []operatorpkg.ActionSuggestion `json:"actions"`
}

// Status shares the verified overview's scope and actions. It does not infer
// repair permission from a cached checkpoint or run the recovery assessment.
func workspaceRecoveryFromSnapshot(snapshot operatorpkg.OperatorSnapshot) *workspaceRecovery {
	if len(snapshot.Blockers) == 0 && snapshot.Activity.State != operatorpkg.ActivityInterrupted &&
		(snapshot.PrimaryAction == nil || snapshot.PrimaryAction.ID != operatorpkg.ActionInspectTaskRecovery) {
		return nil
	}
	recovery := &workspaceRecovery{State: "assessment_required", Blockers: snapshot.Blockers, Actions: []operatorpkg.ActionSuggestion{}}
	if recovery.Blockers == nil {
		recovery.Blockers = []operatorpkg.DiagnosticView{}
	}
	for _, action := range snapshot.SecondaryActions {
		if action.Kind == operatorpkg.ActionKindInspect {
			recovery.Actions = append(recovery.Actions, action)
		}
	}
	if snapshot.PrimaryAction != nil && snapshot.PrimaryAction.Kind == operatorpkg.ActionKindInspect {
		recovery.Actions = append([]operatorpkg.ActionSuggestion{*snapshot.PrimaryAction}, recovery.Actions...)
	}
	return recovery
}

func writeWorkspaceRecovery(out io.Writer, recovery *workspaceRecovery) {
	if recovery == nil {
		return
	}
	_, _ = fmt.Fprintln(out, "\nRecovery:   assessment required; no repair authorized by status")
	for _, blocker := range recovery.Blockers {
		_, _ = fmt.Fprintf(out, "  Task %s: %s (%s)\n", safeOverviewValue(blocker.Ref), safeOverviewValue(blocker.Message), safeOverviewValue(blocker.Code))
	}
	shell := filepath.Base(os.Getenv("SHELL"))
	if shell == "." || shell == "" {
		shell = "sh"
		if runtime.GOOS == "windows" {
			shell = "powershell"
		}
	}
	for _, action := range recovery.Actions {
		_, _ = fmt.Fprintf(out, "  %s: %s (%s)\n", safeOverviewValue(action.ID), safeOverviewValue(action.Availability), safeOverviewValue(action.ReasonCode))
		if action.Availability != operatorpkg.ActionAvailabilityAvailable || len(action.Argv) == 0 {
			continue
		}
		command, renderable := operatorpkg.RenderArgv(action.Argv, shell)
		if !renderable {
			_, _ = fmt.Fprintln(out, "    Command is not safe to render; inspect scoped JSON argv.")
			continue
		}
		if action.CWD != "" {
			_, _ = fmt.Fprintf(out, "    Directory: %s\n", safeOverviewValue(action.CWD))
		}
		_, _ = fmt.Fprintf(out, "    %s\n", command)
		if action.ID == operatorpkg.ActionAssessResultRepair {
			_, _ = fmt.Fprintln(out, "    Read-only preview; repair may still be blocked. Use the original project directory and run configuration/model overrides.")
		}
	}
}
