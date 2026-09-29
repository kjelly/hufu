package teampkg

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func packageForInstall(t *testing.T, project string) (string, string) {
	t.Helper()
	dir := basicTeam(t, `name: installed-team
max-rounds: 7
skills: install-owned
`)
	writeFixture(t, dir, "skills/install-owned/SKILL.md", "---\nname: install-owned\ndescription: Install fixture.\n---\nVerify install.\n")
	output := filepath.Join(project, "installed-team.hufu")
	if _, err := Pack(PackOptions{TeamDir: dir, Version: "v4", Output: output}); err != nil {
		t.Fatal(err)
	}
	return dir, output
}

func TestInstallProjectPublishesCompleteCompiledTeam(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	source, packagePath := packageForInstall(t, project)
	sourceSpec, err := team.CompileTeam(source, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Install(InstallOptions{Package: packagePath})
	if err != nil {
		t.Fatal(err)
	}
	wantTarget := filepath.Join(project, ".agent-teams", "installed-team")
	if result.Target != wantTarget || result.DryRun {
		t.Fatalf("install result = %#v, want target %q", result, wantTarget)
	}
	for _, path := range []string{"team.yaml", "worker.md", "skills/install-owned/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(result.Target, filepath.FromSlash(path))); err != nil {
			t.Fatalf("installed %s: %v", path, err)
		}
	}
	for _, path := range []string{ManifestFilename, LockFilename} {
		if _, err := os.Stat(filepath.Join(result.Target, path)); !os.IsNotExist(err) {
			t.Fatalf("package metadata %s was installed: %v", path, err)
		}
	}
	installedSpec, err := team.CompileTeam(result.Target, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sourceSpec.RuntimeSession().Config, installedSpec.RuntimeSession().Config) {
		t.Fatalf("installed normalized config differs\nsource: %#v\ninstalled: %#v", sourceSpec.RuntimeSession().Config, installedSpec.RuntimeSession().Config)
	}
	if _, err := os.Stat(filepath.Join(project, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("install compile created a workspace: %v", err)
	}
}

func TestInstallDryRunValidatesWithoutPersistentMutation(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	_, packagePath := packageForInstall(t, project)
	result, err := Install(InstallOptions{Package: packagePath, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.DryRun || result.Target != filepath.Join(project, ".agent-teams", "installed-team") {
		t.Fatalf("dry-run result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(project, ".agent-teams")); !os.IsNotExist(err) {
		t.Fatalf("dry run created install root: %v", err)
	}
}

func TestInstallGlobalUsesHomeTeamRoot(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	t.Chdir(project)
	t.Setenv("HOME", home)
	_, packagePath := packageForInstall(t, project)
	result, err := Install(InstallOptions{Package: packagePath, Global: true})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".agent-teams", "installed-team")
	if result.Target != want {
		t.Fatalf("global target = %q, want %q", result.Target, want)
	}
	if _, err := os.Stat(filepath.Join(want, "team.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestInstallCollisionPreservesExistingTarget(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	_, packagePath := packageForInstall(t, project)
	target := filepath.Join(project, ".agent-teams", "installed-team")
	writeFixture(t, target, "marker.txt", "keep\n")
	if _, err := Install(InstallOptions{Package: packagePath}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("collision error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep\n" {
		t.Fatalf("existing target changed to %q", data)
	}
}

func TestInstallPublishFailureRemovesOnlyStaging(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	_, packagePath := packageForInstall(t, project)
	operations := defaultInstallOperations()
	var staging string
	operations.publish = func(source, _ string) error {
		staging = source
		return errors.New("injected publish failure")
	}
	result, err := installWithOperations(InstallOptions{Package: packagePath}, operations)
	if err == nil || !strings.Contains(err.Error(), "injected publish failure") {
		t.Fatalf("publish error = %v", err)
	}
	if _, err := os.Stat(result.Target); !os.IsNotExist(err) {
		t.Fatalf("failed publish created target: %v", err)
	}
	if staging == "" {
		t.Fatal("publish was not attempted")
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("failed publish left staging directory: %v", err)
	}
}

func TestInstallInterruptedBeforePublishLeavesNoPartialTeam(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	_, packagePath := packageForInstall(t, project)
	operations := defaultInstallOperations()
	operations.syncTree = func(string) error { return errors.New("injected interruption") }
	published := false
	operations.publish = func(_, _ string) error {
		published = true
		return nil
	}
	result, err := installWithOperations(InstallOptions{Package: packagePath}, operations)
	if err == nil || !strings.Contains(err.Error(), "injected interruption") {
		t.Fatalf("interruption error = %v", err)
	}
	if published {
		t.Fatal("interrupted install attempted publication")
	}
	if _, err := os.Stat(result.Target); !os.IsNotExist(err) {
		t.Fatalf("interrupted install left partial target: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(result.Target))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hufu-team-install-") {
			t.Fatalf("interrupted install left staging directory %q", entry.Name())
		}
	}
}

func TestPublishDirectoryNoReplacePreservesCollision(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	target := filepath.Join(parent, "target")
	writeFixture(t, source, "new.txt", "new\n")
	writeFixture(t, target, "old.txt", "old\n")
	if err := publishDirectoryNoReplace(source, target); err == nil {
		t.Fatal("no-replace publication overwrote an existing target")
	}
	if _, err := os.Stat(filepath.Join(source, "new.txt")); err != nil {
		t.Fatalf("source was lost after collision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "old.txt")); err != nil {
		t.Fatalf("target was changed after collision: %v", err)
	}
}
