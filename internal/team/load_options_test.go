package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExplicitSkillSearchPathsExcludeAmbientSkills(t *testing.T) {
	root := t.TempDir()
	ambient := filepath.Join(root, "ambient")
	local := filepath.Join(root, "local")
	teamDir := filepath.Join(root, "team")
	for _, dir := range []string{filepath.Join(ambient, ".agents", "skills", "ambient"), filepath.Join(local, "chosen"), teamDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeLintTestFile(t, filepath.Join(ambient, ".agents", "skills", "ambient", "SKILL.md"), "---\nname: ambient\ndescription: ambient skill\n---\nAmbient content.\n")
	writeLintTestFile(t, filepath.Join(local, "chosen", "SKILL.md"), "---\nname: chosen\ndescription: explicit skill\n---\nExplicit content.\n")
	writeLintTestFile(t, filepath.Join(teamDir, "team.yaml"), "name: hermetic\nagents: {worker: readonly}\n")
	t.Setenv("HOME", ambient)
	legacy, err := LoadTeam(teamDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sk := range legacy.Skills {
		found = found || sk.Name == "ambient"
	}
	if !found {
		t.Fatal("default loader no longer includes ambient skills")
	}
	session, err := LoadTeamWithOptions(teamDir, nil, nil, nil, TeamLoadOptions{SkillSearchPaths: []string{local}})
	if err != nil || len(session.Skills) != 1 || session.Skills[0].Name != "chosen" {
		t.Fatalf("explicit boundary failed: session=%+v error=%v", session, err)
	}
	inspection, err := InspectTeamWithOptions(teamDir, nil, nil, nil, TeamCompileLint, TeamLoadOptions{SkillSearchPaths: []string{}})
	if err != nil || !inspection.Complete || len(inspection.Session.Skills) != 0 {
		t.Fatalf("disabled discovery failed: inspection=%+v error=%v", inspection, err)
	}
}
