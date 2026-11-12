package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func TestWorkspaceDistillationProbeHasBoundedCallsAndCannotBecomeAnApplyPlan(t *testing.T) {
	f := newWorkspaceCLIFixture(t)
	f.run(t, "register")
	f.deps.registryOptions = []workspacepkg.RegistryOption{workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38))))}
	registry, err := workspacepkg.OpenReadWrite(f.stateRoot, f.deps.registryOptions...)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := registry.CreateWorkspace(t.Context(), f.projectID, "default")
	_ = registry.Close()
	if err != nil {
		t.Fatal(err)
	}
	session := team.NewSession()
	session.Tasks = []*team.TodoItem{{ID: "1", Agent: "worker", Desc: "old completed work", Status: team.TaskDone, Output: strings.Repeat("evidence ", taskdistill.MaxInputBytes/4)}}
	if err := team.SaveSession(destination.ControlRoot, session); err != nil {
		t.Fatal(err)
	}
	search := t.TempDir()
	helperSetupTeam(t, search, "default")
	oldFactory := taskDistillationGeneratorFactory
	t.Cleanup(func() { taskDistillationGeneratorFactory = oldFactory })
	var requested []string
	var progress bytes.Buffer
	run := func(args ...string) (string, error) {
		command := newWorkspaceCommand(f.deps)
		command.SetContext(t.Context())
		command.SilenceErrors, command.SilenceUsage = true, true
		var output bytes.Buffer
		progress.Reset()
		command.SetOut(&output)
		command.SetErr(&progress)
		command.SetArgs(args)
		err := command.Execute()
		return output.String(), err
	}
	factories := 0
	taskDistillationGeneratorFactory = func(context.Context, string, string, string) (maintenanceTextGenerator, string, func(), error) {
		factories++
		g := identityTextGenerator{identity: "admitted-fixture-policy", generate: func(_ context.Context, prompt string) (string, error) {
			_, data, _ := strings.Cut(prompt, "\nDATA: ")
			var input struct {
				Sources []taskdistill.Source `json:"sources"`
			}
			if err := json.Unmarshal([]byte(data), &input); err != nil {
				t.Fatal(err)
			}
			draft := taskdistill.Draft{Decisions: []taskdistill.Decision{}, Knowledge: []taskdistill.Knowledge{}}
			for _, source := range input.Sources {
				requested = append(requested, source.ID)
				draft.Decisions = append(draft.Decisions, taskdistill.Decision{SourceID: source.ID, Action: "discard", Reason: "transient"})
			}
			raw, err := json.Marshal(draft)
			return string(raw), err
		}}
		return g, "fixture-model", func() {}, nil
	}
	if _, err := run("distill", "--team", "default", "--task", "1", "--probe-batch", "999", "--max-model-calls", "1"); err == nil || factories != 0 {
		t.Fatalf("invalid probe admitted a provider: %v", err)
	}
	output, err := run("distill", "--team", "default", "--task", "1", "--probe-batch", "2", "--max-model-calls", "1", "--team-search-path", search)
	if err != nil || !strings.Contains(progress.String(), "Distillation probe:") {
		t.Fatalf("probe failed: %v %s", err, progress.String())
	}
	var result distillationProbeResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Kind != "distillation_probe" || result.Batch != 2 || result.TotalBatches < 3 || len(requested) != 1 {
		t.Fatalf("probe scope or budget changed: %+v calls=%v", result, requested)
	}
	var doc map[string]any
	_ = json.Unmarshal([]byte(output), &doc)
	if _, exists := doc["id"]; exists {
		t.Fatal("probe returned an applicable plan ID")
	}
	if _, err := run("distill", "--team", "default", "--apply", result.Kind, "--yes"); err == nil {
		t.Fatal("probe was accepted as a saved apply plan")
	}
	if len(team.LoadSession(destination.ControlRoot).Tasks) != 1 {
		t.Fatal("probe removed a source task")
	}
	if _, err := os.Stat(filepath.Join(destination.ControlRoot, "context.sqlite")); !os.IsNotExist(err) {
		t.Fatal("probe confirmed knowledge")
	}
	probed := requested[0]
	requested = nil
	preview, err := run("distill", "--team", "default", "--task", "1", "--team-search-path", search)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(preview), []byte(`"id"`)) || len(requested) == 0 {
		t.Fatal("normal preview did not finish its remaining batches")
	}
	for _, id := range requested {
		if id == probed {
			t.Fatal("normal preview repeated the verified probe batch")
		}
	}
}
