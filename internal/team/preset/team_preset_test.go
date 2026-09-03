package preset

import (
	"strings"
	"testing"
)

func TestRenderProducesParseableFrontmatter(t *testing.T) {
	team, ok := LookupTeam("coding-reviewed")
	if !ok {
		t.Fatal("coding-reviewed team preset not found")
	}
	files := team.Render()
	if len(files) != len(team.Agents) {
		t.Fatalf("Render() produced %d files, want %d", len(files), len(team.Agents))
	}
	for _, a := range team.Agents {
		content, ok := files[a.FileName]
		if !ok {
			t.Fatalf("Render() missing file %q", a.FileName)
		}
		if !strings.HasPrefix(content, "---\n") {
			t.Errorf("%s content does not start with frontmatter delimiter: %q", a.FileName, content)
		}
		if !strings.Contains(content, "preset: "+a.AgentPreset+"\n") {
			t.Errorf("%s content missing preset field for %q: %q", a.FileName, a.AgentPreset, content)
		}
		if !strings.Contains(content, a.System) {
			t.Errorf("%s content missing system prompt %q", a.FileName, a.System)
		}
	}
}

func TestTeamPresetsReferenceOnlyKnownAgentPresets(t *testing.T) {
	for _, name := range TeamNames() {
		team, _ := LookupTeam(name)
		for _, agent := range team.Agents {
			if _, ok := Lookup(agent.AgentPreset); !ok {
				t.Errorf("team preset %q references unknown agent preset %q", name, agent.AgentPreset)
			}
		}
	}
}

func TestTeamPresetsHaveUniqueFileNames(t *testing.T) {
	for _, name := range TeamNames() {
		team, _ := LookupTeam(name)
		seen := map[string]bool{}
		for _, agent := range team.Agents {
			if seen[agent.FileName] {
				t.Errorf("team preset %q has duplicate file name %q", name, agent.FileName)
			}
			seen[agent.FileName] = true
		}
	}
}

func TestLookupTeamIsCaseInsensitive(t *testing.T) {
	for _, input := range []string{"CODING-REVIEWED", " coding-reviewed ", "coding-reviewed"} {
		if _, ok := LookupTeam(input); !ok {
			t.Errorf("LookupTeam(%q) = not found, want coding-reviewed", input)
		}
	}
}

func TestLookupTeamUnknownNotFound(t *testing.T) {
	if _, ok := LookupTeam("does-not-exist"); ok {
		t.Fatal("LookupTeam(\"does-not-exist\") = found, want not found")
	}
}

func TestTeamNamesMatchSpecifiedInitialSet(t *testing.T) {
	got := TeamNames()
	want := []string{"coding-reviewed", "coding-single", "research", "safe-ops"}
	if len(got) != len(want) {
		t.Fatalf("TeamNames() = %v, want %v", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Fatalf("TeamNames()[%d] = %q, want %q (TeamNames() = %v)", i, got[i], name, got)
		}
	}
}
