package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestTeamCreateWizardPreviewsValidatesThenWrites(t *testing.T) {
	t.Chdir(t.TempDir())
	teamCreatePreset, teamCreateModel = "", ""
	var output bytes.Buffer
	input := bufio.NewReader(strings.NewReader("\n\n\n\n\n\nwrite\n"))
	if err := runTeamCreateWizardSession(nil, "demo", input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Preview (validated; no target files written)") || !strings.Contains(output.String(), "Next: hufu team check demo") {
		t.Fatalf("wizard output omitted preview or single next action:\n%s", output.String())
	}
	teamYAML, err := os.ReadFile(filepath.Join(".agent-teams", "demo", "team.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(teamYAML), "mode: observe") {
		t.Fatalf("wizard did not use the suggested observe starting mode: %s", teamYAML)
	}
	worker, err := os.ReadFile(filepath.Join(".agent-teams", "demo", "worker.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(worker), "preset: readonly") {
		t.Fatalf("wizard default was not conservative: %s", worker)
	}
	if _, err = os.Stat("workspace"); !os.IsNotExist(err) {
		t.Fatalf("wizard automatically ran the team or created a workspace: %v", err)
	}
}

func TestTeamCreateWizardCancelAndExistingTeamNeverOverwrite(t *testing.T) {
	t.Chdir(t.TempDir())
	teamCreatePreset, teamCreateModel = "", ""
	var output bytes.Buffer
	input := bufio.NewReader(strings.NewReader("\n\n\n\n\n\n\n"))
	if err := runTeamCreateWizardSession(nil, "cancelled", input, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(".agent-teams", "cancelled")); !os.IsNotExist(err) {
		t.Fatalf("cancelled wizard wrote target files: %v", err)
	}

	existing := filepath.Join(".agent-teams", "existing")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "keep.md"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runTeamCreateWizardSession(nil, "existing", bufio.NewReader(strings.NewReader("")), &output)
	if err == nil || !strings.Contains(err.Error(), "keep.md") || !strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("existing-team error = %v", err)
	}
	content, readErr := os.ReadFile(filepath.Join(existing, "keep.md"))
	if readErr != nil || string(content) != "keep" {
		t.Fatalf("wizard changed existing team: content=%q err=%v", content, readErr)
	}
}

func TestTeamCreateWizardRefusesNonTTYBeforeWrite(t *testing.T) {
	t.Chdir(t.TempDir())
	previousWizard, previousForce, previousFrom, previousExpanded := teamCreateWizard, teamCreateForce, teamCreateFrom, teamCreateExpanded
	teamCreateWizard, teamCreateForce, teamCreateFrom, teamCreateExpanded = true, false, "", false
	t.Cleanup(func() {
		teamCreateWizard, teamCreateForce, teamCreateFrom, teamCreateExpanded = previousWizard, previousForce, previousFrom, previousExpanded
	})
	command := &cobra.Command{}
	command.SetIn(strings.NewReader("write\n"))
	if err := runTeamCreate(command, []string{"piped"}); err == nil || !strings.Contains(err.Error(), "interactive TTY") || !strings.Contains(err.Error(), "--preset readonly") {
		t.Fatalf("non-TTY wizard error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(".agent-teams", "piped")); !os.IsNotExist(err) {
		t.Fatalf("non-TTY wizard wrote a team: %v", err)
	}
}
