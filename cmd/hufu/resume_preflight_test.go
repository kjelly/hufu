package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func TestBindResumePreflightVersionRequiresManagedWorkspace(t *testing.T) {
	for _, name := range []string{"managed", "unregistered", "missing_registry"} {
		t.Run(name, func(t *testing.T) {
			f := newVersionedCLIFixture(t)
			t.Chdir(f.subject)
			workspace := f.control
			if name == "unregistered" {
				workspace = t.TempDir()
			}
			if name == "missing_registry" {
				t.Setenv("HUFU_STATE_HOME", filepath.Join(t.TempDir(), "absent"))
			}
			session := &team.TeamSession{Workspace: workspace}
			before := session.WorkspaceVersion
			err := bindResumePreflightVersion(t.Context(), session)
			if name != "managed" {
				if !errors.Is(err, versionstore.ErrVersionedWorkspaceUnresolved) || !reflect.DeepEqual(before, session.WorkspaceVersion) {
					t.Fatalf("missing registry admitted or changed version binding: version=%+v error=%v", session.WorkspaceVersion, err)
				}
				if name == "missing_registry" {
					if _, err := os.Stat(os.Getenv("HUFU_STATE_HOME")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("preflight created a missing registry")
					}
				}
				return
			}
			version := session.WorkspaceVersion
			if err != nil || version.ControlRoot != workspace || version.SubjectRoot != f.subject || version.WorkspaceID == "" || version.Mode == "" || version.HoldsProjectLock {
				t.Fatalf("managed preflight lost its read-only binding: version=%+v error=%v", version, err)
			}
		})
	}
}
