package workspace

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagerResolveManagedModes(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(stateRoot, WithIDGenerator(NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x11}, 48)))))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "Dev", Mode: ResolvePreview})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.WouldCreate || preview.SubjectRoot != subjectRoot || preview.TeamName != "dev" || preview.ProjectID != "" || preview.WorkspaceID != "" || preview.ControlRoot != "" {
		t.Fatalf("unregistered preview = %+v", preview)
	}
	if _, err = os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("preview created state root: %v", err)
	}
	if _, err = manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "dev", Mode: ResolveExisting}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("existing missing error = %v", err)
	}

	ensured, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "dev", Mode: ResolveEnsure})
	if err != nil {
		t.Fatal(err)
	}
	if !ensured.Managed || ensured.WouldCreate || ensured.ProjectID == "" || ensured.WorkspaceID == "" || ensured.ContextScopeID != subjectRoot {
		t.Fatalf("ensured resolution = %+v", ensured)
	}
	existing, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: filepath.Join(subjectRoot, ".git"), TeamName: "dev", Mode: ResolveExisting})
	if err != nil {
		t.Fatal(err)
	}
	if existing != ensured {
		t.Fatalf("existing = %+v, want %+v", existing, ensured)
	}

	missingTeam, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "qa", Mode: ResolvePreview})
	if err != nil {
		t.Fatal(err)
	}
	if !missingTeam.Managed || !missingTeam.WouldCreate || missingTeam.ProjectID != ensured.ProjectID || missingTeam.WorkspaceID != "" || missingTeam.ContextScopeID != subjectRoot || !strings.HasSuffix(missingTeam.ControlRoot, filepath.Join("teams", "qa")) {
		t.Fatalf("registered preview = %+v", missingTeam)
	}
}

func TestManagerResolveActiveSelectsSoleNamedTeam(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	want, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "hufu-code-review", Mode: ResolveEnsure})
	if err != nil {
		t.Fatal(err)
	}

	got, err := manager.ResolveActive(t.Context(), subjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("active resolution = %+v, want %+v", got, want)
	}
}

func TestManagerResolveActivePrefersDefaultAndRejectsAmbiguousTeams(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defaultWorkspace, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "default", Mode: ResolveEnsure})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "review", Mode: ResolveEnsure}); err != nil {
		t.Fatal(err)
	}
	got, err := manager.ResolveActive(t.Context(), subjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultWorkspace {
		t.Fatalf("active resolution = %+v, want default workspace %+v", got, defaultWorkspace)
	}

	otherRoot := filepath.Join(root, "other-project")
	if err = os.MkdirAll(filepath.Join(otherRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	otherManager, err := NewManager(filepath.Join(root, "other-state"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = otherManager.Resolve(t.Context(), ResolveRequest{StartDir: otherRoot, TeamName: "review", Mode: ResolveEnsure}); err != nil {
		t.Fatal(err)
	}
	if _, err = otherManager.Resolve(t.Context(), ResolveRequest{StartDir: otherRoot, TeamName: "ops", Mode: ResolveEnsure}); err != nil {
		t.Fatal(err)
	}
	if _, err = otherManager.ResolveActive(t.Context(), otherRoot); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous active resolution error = %v, want ErrAmbiguous", err)
	}
}

func TestManagerResolveExplicitAndTemporaryNeverOpenRegistry(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "missing-state")
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	exact := filepath.Join(root, "exact")
	resolution, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "dev", ExplicitExact: exact, Mode: ResolveEnsure})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Managed || resolution.ControlRoot != exact || resolution.ContextScopeID != subjectRoot {
		t.Fatalf("exact resolution = %+v", resolution)
	}
	workspaceRoot := filepath.Join(root, "root")
	resolution, err = manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "dev", ExplicitRoot: workspaceRoot, Mode: ResolveEnsure})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.ControlRoot != filepath.Join(workspaceRoot, "dev") {
		t.Fatalf("root resolution = %+v", resolution)
	}
	temporary := filepath.Join(root, "temporary")
	resolution, err = manager.Resolve(t.Context(), ResolveRequest{
		StartDir: subjectRoot, TeamName: "dev", TemporaryRoot: temporary,
		ExplicitExact: exact, ExplicitRoot: workspaceRoot, Mode: ResolveEnsure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.ControlRoot != temporary {
		t.Fatalf("temporary precedence resolution = %+v", resolution)
	}
	if _, err = os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("unmanaged resolution created state root: %v", err)
	}
}

func TestManagerResolveRejectsIncompleteWorkspace(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop")
	manager, err := NewManager(stateRoot, WithCreateHook(func(stage CreateStage) error {
		if stage == CreateStageReserved {
			return stop
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "dev", Mode: ResolveEnsure}); !errors.Is(err, stop) {
		t.Fatalf("ensure interruption = %v", err)
	}
	if _, err = manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "dev", Mode: ResolvePreview}); !errors.Is(err, ErrConflict) {
		t.Fatalf("preview incomplete error = %v", err)
	}
}

func TestWorkspaceManagerRejectsLegacyBeforeCreatingState(t *testing.T) {
	root := t.TempDir()
	subjectRoot := filepath.Join(root, "project")
	legacy := filepath.Join(subjectRoot, "workspace", "dev")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "state")
	manager, err := NewManager(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []ResolveMode{ResolvePreview, ResolveExisting, ResolveEnsure} {
		_, err = manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "dev", Mode: mode})
		var legacyErr *LegacyWorkspaceError
		if !errors.As(err, &legacyErr) || !strings.Contains(err.Error(), "hufu workspace migrate --team dev") {
			t.Fatalf("mode %s error = %v", mode, err)
		}
	}
	if _, err = os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("legacy preflight created state root: %v", err)
	}
}

func TestWorkspaceManagerExistingManagedWorkspaceWinsOverLegacy(t *testing.T) {
	root := t.TempDir()
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "dev", Mode: ResolveEnsure})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(subjectRoot, "workspace", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "dev", Mode: ResolveEnsure})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.WorkspaceID != created.WorkspaceID || resolved.ControlRoot != created.ControlRoot {
		t.Fatalf("managed workspace did not win: created=%+v resolved=%+v", created, resolved)
	}
}
