package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func TestManagedResumeBindingAndActivation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		readOnly   bool
		busy       bool
		wrongTeam  bool
		staticFail bool
		mode       versionstore.Mode
	}{
		{name: "explicit registered control root"},
		{name: "off keeps metadata-only behavior", mode: versionstore.ModeOff},
		{name: "observe keeps metadata-only behavior", mode: versionstore.ModeObserve},
		{name: "preview takes no writer lock", readOnly: true},
		{name: "project conflict precedes activation", busy: true},
		{name: "foreign team refused", wrongTeam: true},
		{name: "static gate precedes activation", staticFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldOpts := opts
			t.Cleanup(func() { opts = oldOpts })
			opts = runOptions{}
			f := newVersionedCLIFixture(t)
			t.Chdir(f.subject)
			if tc.mode != "" {
				path := filepath.Join(os.Getenv("HOME"), ".config", "hufu", "hufu.yaml")
				if err := os.WriteFile(path, []byte("workspace-versioning:\n  mode: "+string(tc.mode)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f.write(t, "version.txt", "A")
			es, err := team.NewEventStore(f.control, "run-resume", "session-resume")
			if err != nil {
				t.Fatal(err)
			}
			err = es.Append(team.RunEvent{BranchID: "main", Type: string(team.EventTaskCompleted), TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","desc":"accepted work","status":"done"}`)})
			_ = es.Close()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runCLI(t, "session", "fork", "--name", "b", "--workspace", f.control); err != nil {
				t.Fatal(err)
			}
			f.write(t, "version.txt", "B")
			requested := "dev"
			if tc.wrongTeam {
				requested = "other"
			}
			binding, name, err := bindManagedResumeWorkspace(t.Context(), &commandWorkspaceBinding{Resolution: workspacepkg.Resolution{ControlRoot: f.control}}, requested, tc.readOnly)
			if tc.wrongTeam {
				if err == nil {
					t.Fatal("foreign team accepted")
				}
				return
			}
			if err != nil || name != "dev" || !binding.Resolution.Managed || binding.Resolution.SubjectRoot != f.subject {
				t.Fatalf("managed resolution: binding=%+v name=%s err=%v", binding, name, err)
			}
			if tc.readOnly {
				if binding.Lease != nil {
					t.Fatal("preview acquired a writer lease")
				}
				return
			}
			t.Cleanup(func() { _ = binding.Lease.Close() })
			locks, err := workspacepkg.AcquireWorkspaceLocks(os.Getenv("HUFU_STATE_HOME"), []string{binding.Resolution.WorkspaceID})
			if err == nil {
				_ = locks.Close()
				t.Fatal("managed resume did not retain the team lock")
			}
			source, err := team.ReadResumeSource(f.control, "main", "run-resume")
			if err != nil {
				t.Fatal(err)
			}
			ctx := withResumeActivation(t.Context(), source)
			session := &team.TeamSession{Dir: t.TempDir(), Config: agent.TeamConfig{Name: "dev"}, Agents: map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker"}}}
			if err := applyWorkspaceResolution(session, binding.Resolution, binding.Lease); err != nil {
				t.Fatal(err)
			}
			if tc.busy {
				lock, err := versionstore.TryLockProject(f.state, versionstore.LockOwner{WorkspaceID: "other", Operation: "run"})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
			}
			if tc.staticFail {
				_, err = loadTeamCommon(ctx, "dev", session, "", "", nil, nil, nil, false, false, false)
				if err == nil || session.WorkspaceVersion.Required() {
					t.Fatalf("static failure activated versioning: %v", err)
				}
			} else {
				err = bindRunWorkspaceVersioning(ctx, session, config.LoadConfig())
				if tc.busy {
					if !errors.Is(err, versionstore.ErrVersionOperationInProgress) {
						t.Fatalf("project conflict error = %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				} else if err = activateResumeSession(ctx, session); err != nil {
					t.Fatal(err)
				}
			}
			tree, err := team.LoadSessionTree(f.control)
			if err != nil {
				t.Fatal(err)
			}
			wantBranch, wantFile := "main", "A"
			if tc.mode != "" {
				wantFile = "B"
			}
			if tc.busy || tc.staticFail {
				wantBranch, wantFile = "b", "B"
			}
			if tree.ActiveBranch != wantBranch || f.read(t, "version.txt") != wantFile {
				t.Fatalf("branch/files = %s/%s, want %s/%s", tree.ActiveBranch, f.read(t, "version.txt"), wantBranch, wantFile)
			}
			if !tc.busy && !tc.staticFail && tc.mode == "" {
				lock, err := versionstore.TryLockProject(f.state, versionstore.LockOwner{WorkspaceID: "other", Operation: "run"})
				if err == nil {
					_ = lock.Close()
					t.Fatal("project lock was released before coordinator lifetime ended")
				}
			}
		})
	}
}
