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

func TestWorkspaceLifecycleCommandsRequireConfirmationAndRoundTrip(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	fixture.run(t, "register")
	registry, err := workspacepkg.OpenReadWrite(fixture.stateRoot,
		workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x22, 0x23)))),
		workspacepkg.WithClock(func() time.Time { return fixture.now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := registry.CreateWorkspace(t.Context(), fixture.projectID, "default")
	if err != nil {
		t.Fatal(err)
	}
	registry.Close()
	fixture.deps.registryOptions = []workspacepkg.RegistryOption{
		workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x31, 0x32, 0x33, 0x34, 0x35, 0x36)))),
		workspacepkg.WithClock(func() time.Time { return fixture.now }),
	}

	stdout, err := executeWorkspaceCommand(t, fixture.deps, "delete")
	if err == nil || stdout != "" || !pathExistsForCLI(workspace.ControlRoot) {
		t.Fatalf("unconfirmed delete = output %q, error %v", stdout, err)
	}
	stdout, err = executeWorkspaceCommand(t, fixture.deps, "delete", "--yes")
	if err != nil || !strings.HasPrefix(stdout, "delete outcome=complete\n") {
		t.Fatalf("delete output = %q, %v", stdout, err)
	}
	trashID := lifecycleOutputValue(stdout, "trash_id")
	if trashID == "" {
		t.Fatalf("delete output has no trash ID: %q", stdout)
	}
	stdout, err = executeWorkspaceCommand(t, fixture.deps, "restore", trashID, "--yes", "--output", "json")
	if err != nil || !strings.Contains(stdout, `"outcome": "complete"`) || !pathExistsForCLI(workspace.ControlRoot) {
		t.Fatalf("restore output = %q, %v", stdout, err)
	}
	stdout, err = executeWorkspaceCommand(t, fixture.deps, "delete", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	trashID = lifecycleOutputValue(stdout, "trash_id")
	stdout, err = executeWorkspaceCommand(t, fixture.deps, "purge", trashID)
	if err == nil || stdout != "" {
		t.Fatalf("unconfirmed purge = output %q, error %v", stdout, err)
	}
	stdout, err = executeWorkspaceCommand(t, fixture.deps, "purge", trashID, "--yes")
	if err != nil || !strings.HasPrefix(stdout, "purge outcome=complete\n") {
		t.Fatalf("purge output = %q, %v", stdout, err)
	}
}

func TestConfirmWorkspaceMutationUsesInjectedConfirmation(t *testing.T) {
	wantErr := errors.New("declined")
	calls := 0
	confirm := func(action string) error {
		calls++
		if action != "delete" {
			t.Fatalf("confirmation action = %q, want delete", action)
		}
		return wantErr
	}
	if err := confirmWorkspaceMutation("delete", false, confirm); !errors.Is(err, wantErr) {
		t.Fatalf("confirmation error = %v, want %v", err, wantErr)
	}
	if calls != 1 {
		t.Fatalf("confirmation calls = %d, want 1", calls)
	}
	if err := confirmWorkspaceMutation("delete", true, confirm); err != nil {
		t.Fatalf("--yes confirmation = %v", err)
	}
	if calls != 1 {
		t.Fatalf("--yes unexpectedly called confirmation; calls = %d", calls)
	}
	if err := confirmWorkspaceMutation("restore", false, nil); err == nil || !strings.Contains(err.Error(), "confirmation input is unavailable") {
		t.Fatalf("missing confirmation dependency error = %v", err)
	}
}

func TestWorkspaceLifecyclePostReservationFailureWritesPartialResult(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	fixture.run(t, "register")
	registry, err := workspacepkg.OpenReadWrite(fixture.stateRoot,
		workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x22, 0x23)))),
		workspacepkg.WithClock(func() time.Time { return fixture.now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.CreateWorkspace(t.Context(), fixture.projectID, "default"); err != nil {
		t.Fatal(err)
	}
	registry.Close()
	stop := errors.New("stop after rename")
	fixture.deps.registryOptions = []workspacepkg.RegistryOption{
		workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x31, 0x32)))),
		workspacepkg.WithClock(func() time.Time { return fixture.now }),
		workspacepkg.WithLifecycleHook(func(stage workspacepkg.LifecycleStage) error {
			if stage == workspacepkg.DeleteStageRenamed {
				return stop
			}
			return nil
		}),
	}
	stdout, err := executeWorkspaceCommand(t, fixture.deps, "delete", "--yes", "--output", "json")
	if !errors.Is(err, stop) || !strings.Contains(stdout, `"outcome": "partial"`) || !strings.Contains(stdout, `"operation_id"`) {
		t.Fatalf("partial lifecycle output = %q, %v", stdout, err)
	}
}

func TestWorkspaceGCDefaultsToReadOnlyAndApplyRequiresYes(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	fixture.run(t, "register")
	registry, err := workspacepkg.OpenReadWrite(fixture.stateRoot,
		workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x22, 0x23)))),
		workspacepkg.WithClock(func() time.Time { return fixture.now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := registry.CreateWorkspace(t.Context(), fixture.projectID, "default")
	if err != nil {
		t.Fatal(err)
	}
	registry.Close()
	identifierReader := bytes.NewReader(fixtureIDs(0x31, 0x32, 0x33, 0x34))
	clock := fixture.now
	fixture.deps.registryOptions = []workspacepkg.RegistryOption{
		workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(identifierReader)),
		workspacepkg.WithClock(func() time.Time { return clock }),
	}
	deletedOutput := fixture.run(t, "delete", "--yes")
	trashID := lifecycleOutputValue(deletedOutput, "trash_id")
	clock = fixture.now.Add(2 * time.Hour)
	registryPath := filepath.Join(fixture.stateRoot, "registry.sqlite")
	before, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := executeWorkspaceCommand(t, fixture.deps, "gc", "--trash-older-than", "1h")
	if err != nil || !strings.HasPrefix(stdout, "gc outcome=complete\nmode=dry-run\n") || !strings.Contains(stdout, trashID) {
		t.Fatalf("GC dry run = %q, %v", stdout, err)
	}
	after, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || pathExistsForCLI(workspace.ControlRoot) {
		t.Fatal("GC dry run changed state")
	}
	stdout, err = executeWorkspaceCommand(t, fixture.deps, "gc", "--apply", "--trash-older-than", "1h")
	if err == nil || stdout != "" {
		t.Fatalf("unconfirmed GC apply = %q, %v", stdout, err)
	}
	stdout, err = executeWorkspaceCommand(t, fixture.deps, "gc", "--apply", "--yes", "--trash-older-than", "1h")
	if err != nil || !strings.Contains(stdout, "purged\t"+trashID) {
		t.Fatalf("GC apply = %q, %v", stdout, err)
	}
}

func TestWorkspaceShellInitGoldenIsStatic(t *testing.T) {
	deps := workspaceCommandDeps{
		stateRoot: func() (string, error) { return "", errors.New("shell-init must not read state") },
		getwd:     func() (string, error) { return "", errors.New("shell-init must not read cwd") },
	}
	var output strings.Builder
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		fmt.Fprintf(&output, "=== %s ===\n", shell)
		stdout, err := executeWorkspaceCommand(t, deps, "shell-init", shell)
		if err != nil {
			t.Fatal(err)
		}
		output.WriteString(stdout)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "workspace_shell_init.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != string(want) {
		t.Fatalf("shell-init golden mismatch\n--- want ---\n%s\n--- got ---\n%s", want, output.String())
	}
	for _, forbidden := range []string{"$(touch", "malicious-alias", "/tmp/runtime-project"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("shell-init contains runtime data %q", forbidden)
		}
	}
}

func TestWorkspaceRebindCommandPreservesControlIdentity(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	fixture.run(t, "register")
	registry, err := workspacepkg.OpenReadWrite(fixture.stateRoot,
		workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x22, 0x33)))),
		workspacepkg.WithClock(func() time.Time { return fixture.now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := registry.CreateWorkspace(t.Context(), fixture.projectID, "default")
	if closeErr := registry.Close(); err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	newRoot := filepath.Join(fixture.root, "moved project")
	if err = os.Mkdir(newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	output := fixture.run(t, "rebind", fixture.projectID, newRoot, "--output", "json")
	if !strings.Contains(output, `"subject_root": "`+newRoot+`"`) || !strings.Contains(output, `"requires_fresh_session": true`) {
		t.Fatalf("rebind output = %s", output)
	}
	registry, err = workspacepkg.OpenReadOnly(fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	rebound, err := registry.GetWorkspaceByID(t.Context(), workspace.ID)
	if closeErr := registry.Close(); err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if rebound.ControlRoot != workspace.ControlRoot || rebound.ContextScopeID != workspace.ContextScopeID || !rebound.RequiresFreshSession {
		t.Fatalf("rebound workspace = %+v; before = %+v", rebound, workspace)
	}
}

func TestWorkspaceMigrateAndDoctorMachineOutput(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	legacy := filepath.Join(projectRoot, "workspace", "default")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "artifact.txt"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "state")
	deps := workspaceCommandDeps{
		stateRoot: func() (string, error) { return stateRoot, nil },
		getwd:     func() (string, error) { return projectRoot, nil },
		registryOptions: []workspacepkg.RegistryOption{
			workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x61}, 128)))),
		},
	}
	stdout, err := executeWorkspaceCommand(t, deps, "migrate", "--team", "default", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"outcome": "complete"`) || !strings.Contains(stdout, `"context_store_created"`) {
		t.Fatalf("migration JSON = %s", stdout)
	}
	stdout, err = executeWorkspaceCommand(t, deps, "doctor", "--output", "json")
	var outcomeErr *workspaceOutcomeError
	if !errors.As(err, &outcomeErr) || outcomeErr.outcome != "partial" {
		t.Fatalf("doctor error = %v", err)
	}
	if !strings.Contains(stdout, `"code": "legacy_source_present"`) {
		t.Fatalf("doctor JSON = %s", stdout)
	}
}

func TestWorkspaceMigratePreflightFailureWritesNoStdout(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	stdout, err := executeWorkspaceCommand(t, fixture.deps, "migrate", "--team", "missing", "--output", "json")
	if err == nil || stdout != "" {
		t.Fatalf("preflight output=%q error=%v", stdout, err)
	}
	if _, statErr := os.Stat(fixture.stateRoot); !os.IsNotExist(statErr) {
		t.Fatalf("preflight created state root: %v", statErr)
	}
}

func TestWorkspaceDoctorHardFailureWritesNoStdout(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	fixture.run(t, "register")
	stdout, err := executeWorkspaceCommand(t, fixture.deps, "doctor", "missing-project", "--output", "json")
	if !errors.Is(err, workspacepkg.ErrNotFound) || stdout != "" {
		t.Fatalf("doctor hard failure output=%q error=%v", stdout, err)
	}
}

func TestWorkspaceGCHardFailureWritesNoStdout(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "registry.sqlite"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := workspaceCommandDeps{
		stateRoot: func() (string, error) { return stateRoot, nil },
		getwd:     func() (string, error) { return root, nil },
	}
	stdout, err := executeWorkspaceCommand(t, deps, "gc", "--output", "json")
	if err == nil || stdout != "" {
		t.Fatalf("GC hard failure output=%q error=%v", stdout, err)
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

func lifecycleOutputValue(output, key string) string {
	prefix := key + "="
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

func pathExistsForCLI(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
