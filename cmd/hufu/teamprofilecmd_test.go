package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTeamProfileCommandsUseCompiledProjection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("decision:\n  profile: standard\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	teamProfileTeam = ""
	teamProfileFormat = "json"
	t.Cleanup(func() { teamProfileFormat = "text" })
	out := captureStdout(t, func() {
		if err := runTeamProfileList(nil, []string{dir}); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	if !strings.Contains(out, `"name": "standard"`) || !strings.Contains(out, `"origin": "builtin"`) {
		t.Fatalf("profile list output = %s", out)
	}
	teamProfileFormat = "text"
	out = captureStdout(t, func() {
		if err := runTeamProfilePlan(nil, []string{"standard", dir}); err != nil {
			t.Fatalf("plan: %v", err)
		}
	})
	if !strings.Contains(out, "profile: standard") || !strings.Contains(out, "plan:") {
		t.Fatalf("profile plan output = %s", out)
	}
}
