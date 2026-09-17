package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func TestWorkspaceCommandGoldenOutput(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	var output strings.Builder
	appendGoldenSection(&output, "register-json", fixture.run(t, "register", "--output", "json"))
	appendGoldenSection(&output, "alias-text", fixture.run(t, "alias", "set", fixture.projectID, "Demo", "--output", "text"))

	registry, err := workspacepkg.OpenReadWrite(fixture.stateRoot,
		workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x22, 0x33)))),
		workspacepkg.WithClock(func() time.Time { return fixture.now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := registry.CreateWorkspace(t.Context(), fixture.projectID, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Close(); err != nil {
		t.Fatal(err)
	}

	appendGoldenSection(&output, "show-json", fixture.run(t, "show", "demo", "--output", "json"))
	appendGoldenSection(&output, "list-text", fixture.run(t, "list", "--output", "text"))

	crash := errors.New("stop after reservation")
	registry, err = workspacepkg.OpenReadWrite(fixture.stateRoot,
		workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x44, 0x55)))),
		workspacepkg.WithClock(func() time.Time { return fixture.now }),
		workspacepkg.WithCreateHook(func(stage workspacepkg.CreateStage) error {
			if stage == workspacepkg.CreateStageReserved {
				return crash
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.CreateWorkspace(t.Context(), fixture.projectID, "dev"); !errors.Is(err, crash) {
		t.Fatalf("crash fixture error = %v", err)
	}
	if err = registry.Close(); err != nil {
		t.Fatal(err)
	}
	appendGoldenSection(&output, "list-all-json", fixture.run(t, "list", "--all", "--output", "json"))
	appendGoldenSection(&output, "path", fixture.run(t, "path", "demo"))
	appendGoldenSection(&output, "subject-path", fixture.run(t, "subject-path"))
	appendGoldenSection(&output, "alias-clear-json", fixture.run(t, "alias", "clear", "demo", "--output", "json"))

	actual := strings.ReplaceAll(output.String(), fixture.root, "<ROOT>")
	want, err := os.ReadFile(filepath.Join("testdata", "workspace_cli.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if actual != string(want) {
		t.Fatalf("workspace CLI golden mismatch\n--- want ---\n%s\n--- got ---\n%s", want, actual)
	}
	if workspace.ControlRoot != filepath.Join(fixture.stateRoot, "projects", "project--1111111111111111", "teams", "default") {
		t.Fatalf("control root = %q", workspace.ControlRoot)
	}
}

func TestWorkspaceReadOnlyCommandsDoNotCreateRegistry(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "missing-state")
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	deps := workspaceCommandDeps{
		stateRoot: func() (string, error) { return stateRoot, nil },
		getwd:     func() (string, error) { return projectRoot, nil },
	}
	stdout, err := executeWorkspaceCommand(t, deps, "list", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"projects": []`) {
		t.Fatalf("empty list output = %q", stdout)
	}
	if _, statErr := os.Stat(stateRoot); !os.IsNotExist(statErr) {
		t.Fatalf("list created state root: %v", statErr)
	}

	stdout, err = executeWorkspaceCommand(t, deps, "show")
	if !errors.Is(err, workspacepkg.ErrNotFound) || stdout != "" {
		t.Fatalf("missing show = output %q, error %v", stdout, err)
	}
	if _, statErr := os.Stat(stateRoot); !os.IsNotExist(statErr) {
		t.Fatalf("show created state root: %v", statErr)
	}

	show, _, findErr := newWorkspaceCommand(deps).Find([]string{"show"})
	if findErr != nil {
		t.Fatal(findErr)
	}
	values, directive := show.ValidArgsFunction(show, nil, "")
	if len(values) != 0 || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("missing completion = %v, %v", values, directive)
	}
	if _, statErr := os.Stat(stateRoot); !os.IsNotExist(statErr) {
		t.Fatalf("completion created state root: %v", statErr)
	}
}

func TestWorkspaceReadOnlyCommandsPreserveRegistryBytesAndCompleteSelectors(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	fixture.run(t, "register")
	fixture.run(t, "alias", "set", fixture.projectID, "demo")
	registryPath := filepath.Join(fixture.stateRoot, "registry.sqlite")
	before, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeEntries := directoryNames(t, fixture.stateRoot)

	fixture.run(t, "show", "demo")
	fixture.run(t, "subject-path", "demo")
	fixture.run(t, "list", "--all")
	show, _, err := newWorkspaceCommand(fixture.deps).Find([]string{"show"})
	if err != nil {
		t.Fatal(err)
	}
	values, directive := show.ValidArgsFunction(show, nil, "")
	wantValues := []string{"demo", fixture.projectID, "project"}
	if !slices.Equal(values, wantValues) || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("completion = %v, %v; want %v", values, directive, wantValues)
	}

	after, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only commands changed registry bytes")
	}
	if afterEntries := directoryNames(t, fixture.stateRoot); !slices.Equal(beforeEntries, afterEntries) {
		t.Fatalf("read-only commands changed state entries: before=%v after=%v", beforeEntries, afterEntries)
	}
}

func TestWorkspaceCommandValidationAndRootRegistration(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	if output, err := executeWorkspaceCommand(t, fixture.deps, "register", "--output", "yaml"); err == nil || output != "" {
		t.Fatalf("invalid output = %q, %v", output, err)
	}
	root := newRootCommand()
	command, _, err := root.Find([]string{"workspace"})
	if err != nil || command.Name() != "workspace" {
		t.Fatalf("root workspace command = %v, %v", command, err)
	}
}

type workspaceCLIFixture struct {
	root        string
	stateRoot   string
	projectRoot string
	projectID   string
	now         time.Time
	deps        workspaceCommandDeps
}

func newWorkspaceCLIFixture(t *testing.T) workspaceCLIFixture {
	t.Helper()
	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "state")
	now := time.Date(2026, 9, 17, 13, 0, 0, 0, time.UTC)
	return workspaceCLIFixture{
		root: root, stateRoot: stateRoot, projectRoot: projectRoot,
		projectID: "prj_" + strings.Repeat("11", 16), now: now,
		deps: workspaceCommandDeps{
			stateRoot: func() (string, error) { return stateRoot, nil },
			getwd:     func() (string, error) { return projectRoot, nil },
			registryOptions: []workspacepkg.RegistryOption{
				workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x11)))),
				workspacepkg.WithClock(func() time.Time { return now }),
			},
		},
	}
}

func (f workspaceCLIFixture) run(t *testing.T, args ...string) string {
	t.Helper()
	stdout, err := executeWorkspaceCommand(t, f.deps, args...)
	if err != nil {
		t.Fatalf("workspace %s: %v", strings.Join(args, " "), err)
	}
	return stdout
}

func executeWorkspaceCommand(t *testing.T, deps workspaceCommandDeps, args ...string) (string, error) {
	t.Helper()
	command := newWorkspaceCommand(deps)
	command.SilenceErrors = true
	command.SilenceUsage = true
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs(args)
	err := command.Execute()
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	return stdout.String(), err
}

func appendGoldenSection(builder *strings.Builder, name, value string) {
	fmt.Fprintf(builder, "=== %s ===\n%s", name, value)
}

func fixtureIDs(values ...byte) []byte {
	var result []byte
	for _, value := range values {
		result = append(result, bytes.Repeat([]byte{value}, 16)...)
	}
	return result
}

func directoryNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
