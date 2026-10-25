package team

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// writeDefaultTeamSkill creates <root>/.agents/skills/<name>/SKILL.md.
func writeDefaultTeamSkill(t *testing.T, root, name, description string) {
	t.Helper()
	dir := filepath.Join(root, ".agents", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\nBody.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func defaultTeamSkillNames(session *TeamSession) []string {
	names := make([]string, 0, len(session.Skills))
	for _, s := range session.Skills {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	return names
}

func TestLoadDefaultTeam(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}

	cfg := session.Config
	if cfg.Name != "default" || session.Workspace != ws || session.Dir != ws {
		t.Errorf("identity = name %q workspace %q dir %q, want default/%s/%s", cfg.Name, session.Workspace, session.Dir, ws, ws)
	}
	if cfg.MaxRounds != 30 || cfg.Timeout != 600 || cfg.MaxRetries != 2 {
		t.Errorf("limits = rounds %d timeout %d retries %d, want 30/600/2", cfg.MaxRounds, cfg.Timeout, cfg.MaxRetries)
	}
	if g := cfg.Generation; g.Temperature != agent.DefaultTemperature || g.MaxTokens != agent.DefaultMaxTokens || g.TopP != agent.DefaultTopP {
		t.Errorf("generation = %+v, want package defaults", g)
	}
	if len(session.MCPServers) != 0 {
		t.Errorf("MCPServers = %d, want 0", len(session.MCPServers))
	}
	for k, want := range map[string]string{"TEAM_NAME": "default", "AGENT_COUNT": "1", "AGENT_NAMES": "Helper"} {
		if got := cfg.Vars[k]; got != want {
			t.Errorf("Config.Vars[%q] = %v, want %q", k, got, want)
		}
	}

	if len(session.Agents) != 2 {
		t.Errorf("Agents count = %d, want 2 (coordinator + helper)", len(session.Agents))
	}
	coord, helper := session.Agents["coordinator"], session.Agents["helper"]
	if coord == nil || coord.Role != "coordinator" || coord.Name != "coordinator" || coord.FileAlias != "coordinator" {
		t.Errorf("coordinator = %+v, want role/name/alias coordinator", coord)
	}
	if helper == nil || helper.Role != "worker" || helper.Name != "Helper" || helper.FileAlias != "helper" || helper.System == "" {
		t.Errorf("helper = %+v, want worker named Helper with a system prompt", helper)
	}
	if coord == helper {
		t.Error("coordinator and helper share one *AgentDef")
	}
	if def := (&Coordinator{session: session}).GetOrchestratorDef(); def == nil || def.Role != "coordinator" || def.Name != "coordinator" {
		t.Errorf("GetOrchestratorDef = %+v, want the coordinator agent, not an auto-fallback", def)
	}
}

func TestLoadDefaultTeamSkillDiscovery(t *testing.T) {
	tests := []struct {
		name         string
		homeSkill    string
		projectSkill string
		forced       []string
		wantSkills   []string
		wantDesc     string
	}{
		{name: "empty HOME yields no skills"},
		{name: "global skills under HOME are discovered", homeSkill: "demo-skill", wantSkills: []string{"demo-skill"}, wantDesc: "demo-skill description"},
		{name: "project-local skills are discovered", projectSkill: "trec-drive", wantSkills: []string{"trec-drive"}},
		{name: "forced skills are merged", homeSkill: "forced-skill", forced: []string{"forced-skill"}, wantSkills: []string{"forced-skill"}},
		{name: "unknown forced skill warns without failing", forced: []string{"nonexistent-skill"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Chdir(project)
			if tt.homeSkill != "" {
				writeDefaultTeamSkill(t, home, tt.homeSkill, tt.homeSkill+" description")
			}
			if tt.projectSkill != "" {
				writeDefaultTeamSkill(t, project, tt.projectSkill, tt.projectSkill+" description")
			}
			session, err := LoadDefaultTeam(t.TempDir(), tt.forced, "")
			if err != nil {
				t.Fatalf("LoadDefaultTeam returned error: %v", err)
			}
			if got := defaultTeamSkillNames(session); !slices.Equal(got, tt.wantSkills) && !(len(got) == 0 && len(tt.wantSkills) == 0) {
				t.Fatalf("skills = %v, want %v", got, tt.wantSkills)
			}
			if tt.wantDesc != "" && session.Skills[0].Description != tt.wantDesc {
				t.Errorf("description = %q, want %q", session.Skills[0].Description, tt.wantDesc)
			}
		})
	}
}

func TestLoadDefaultTeamHelperTools(t *testing.T) {
	baseline := []string{"view", "write", "edit", "grep", "ls"}
	tests := []struct {
		name        string
		helperTools string
		wantPresent []string
		wantAbsent  []string
	}{
		{name: "default keeps the read-only baseline", wantPresent: baseline, wantAbsent: []string{"bash", "sudo", "ssh"}},
		{name: "bash is appended to the baseline", helperTools: "bash", wantPresent: append([]string{"bash"}, baseline...)},
		{name: "whitespace and empty entries are trimmed", helperTools: "  bash, sudo , ssh ,, ", wantPresent: []string{"bash", "sudo", "ssh"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			session, err := LoadDefaultTeam(t.TempDir(), nil, tt.helperTools)
			if err != nil {
				t.Fatalf("LoadDefaultTeam returned error: %v", err)
			}
			helper := session.Agents["helper"]
			if helper == nil {
				t.Fatal("session.Agents missing 'helper'")
			}
			tools := strings.Split(helper.Tools, ",")
			for _, want := range tt.wantPresent {
				if !slices.Contains(tools, want) {
					t.Errorf("Helper.Tools = %v, missing %q", tools, want)
				}
			}
			for _, banned := range tt.wantAbsent {
				if slices.Contains(tools, banned) {
					t.Errorf("Helper.Tools = %v, must not contain %q", tools, banned)
				}
			}
			if slices.ContainsFunc(tools, func(s string) bool { return strings.TrimSpace(s) == "" }) {
				t.Errorf("Helper.Tools contains an empty entry: %v", tools)
			}
		})
	}
}
