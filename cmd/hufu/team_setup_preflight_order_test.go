package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// TestLoadTeamCommonStaticFailureRunsBeforeWorkspaceVersioning holds the
// required-mode project lock from another owner. Binding workspace
// versioning would fail on that lock, so a static configuration error that
// surfaces instead proves the static gate ran first: no project lock taken
// and no workspace recovery attempted.
func TestLoadTeamCommonStaticFailureRunsBeforeWorkspaceVersioning(t *testing.T) {
	const lockConflict = "required mode allows one coordinator per project"
	tests := []struct {
		name    string
		prepare func(session *team.TeamSession)
		wantErr string
	}{
		{
			name:    "role target resolution",
			prepare: func(session *team.TeamSession) { session.Config.WorkerModel = "" },
			wantErr: "no worker execution target specified",
		},
		{
			name:    "execution target preflight",
			prepare: func(session *team.TeamSession) { session.Config.WorkerModel = "nope/fixture-model" },
			wantErr: `unknown execution backend "nope"`,
		},
		{
			name: "effective contract lint",
			prepare: func(session *team.TeamSession) {
				session.Config.Requirements.Environment = []string{"HUFU_TEST_PREFLIGHT_ORDER_UNSET"}
			},
			wantErr: "effective team contract validation failed",
		},
		{
			// Control: a valid team does reach versioning binding and the lock.
			name:    "valid configuration reaches versioning binding",
			prepare: func(*team.TeamSession) {},
			wantErr: lockConflict,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalOpts := opts
			t.Cleanup(func() { opts = originalOpts })
			opts = runOptions{}
			f := newVersionedCLIFixture(t)
			t.Chdir(f.subject)
			stateRoot := os.Getenv("HUFU_STATE_HOME")
			registry, err := workspacepkg.OpenReadOnly(stateRoot)
			if err != nil {
				t.Fatal(err)
			}
			managed, err := registry.GetWorkspaceByControlRoot(context.Background(), f.control)
			_ = registry.Close()
			if err != nil {
				t.Fatal(err)
			}
			lock, err := versionstore.TryLockProject(f.state, versionstore.LockOwner{WorkspaceID: "ws_other_team", Operation: "run"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lock.Close() })

			lease := &commandWorkspaceLease{stateRoot: stateRoot, workspaceID: managed.ID}
			session := &team.TeamSession{
				Dir: t.TempDir(), Workspace: f.control, WorkspaceLease: lease,
				Config: agent.TeamConfig{
					Name:             "preflight-order",
					WorkerModel:      "ollama/fixture-model",
					CoordinatorModel: "ollama/fixture-model",
				},
				Agents: map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker"}},
			}
			tt.prepare(session)

			_, err = loadTeamCommon(t.Context(), "preflight-order", session, "", "", nil, nil, nil, false, false, false)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("loadTeamCommon error = %v, want it to contain %q", err, tt.wantErr)
			}
			if tt.wantErr != lockConflict && strings.Contains(err.Error(), lockConflict) {
				t.Fatalf("loadTeamCommon error = %v; versioning binding ran before the static gate", err)
			}
			if lease.versionLock != nil || session.WorkspaceVersion.Required() {
				t.Fatalf("versioning bound after a failed startup: lock=%v version=%#v", lease.versionLock, session.WorkspaceVersion)
			}
		})
	}
}
