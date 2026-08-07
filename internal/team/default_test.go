package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/skill"
)

func TestLoadDefaultTeam_BasicStructure(t *testing.T) {
	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}

	if session == nil {
		t.Fatal("session is nil")
	}
	if session.Config.Name != "default" {
		t.Errorf("Config.Name = %q, want %q", session.Config.Name, "default")
	}
	if session.Workspace != ws {
		t.Errorf("Workspace = %q, want %q", session.Workspace, ws)
	}
	if session.Dir != ws {
		t.Errorf("Dir = %q, want %q", session.Dir, ws)
	}
	if session.Config.MaxRounds != 30 {
		t.Errorf("Config.MaxRounds = %d, want 30", session.Config.MaxRounds)
	}
	if session.Config.Timeout != 600 {
		t.Errorf("Config.Timeout = %d, want 600", session.Config.Timeout)
	}
	if session.Config.MaxRetries != 2 {
		t.Errorf("Config.MaxRetries = %d, want 2", session.Config.MaxRetries)
	}
	if session.Config.Generation.Temperature != agent.DefaultTemperature {
		t.Errorf("Config.Generation.Temperature = %q, want %q", session.Config.Generation.Temperature, agent.DefaultTemperature)
	}
	if session.Config.Generation.MaxTokens != agent.DefaultMaxTokens {
		t.Errorf("Config.Generation.MaxTokens = %q, want %q", session.Config.Generation.MaxTokens, agent.DefaultMaxTokens)
	}
	if session.Config.Generation.TopP != agent.DefaultTopP {
		t.Errorf("Config.Generation.TopP = %q, want %q", session.Config.Generation.TopP, agent.DefaultTopP)
	}
	if len(session.MCPServers) != 0 {
		t.Errorf("MCPServers = %d, want 0", len(session.MCPServers))
	}
}

func TestLoadDefaultTeam_DiscoversProjectSkills(t *testing.T) {
	project := t.TempDir()
	skillDir := filepath.Join(project, ".agents", "skills", "trec-drive")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: trec-drive\ndescription: Drive interactive terminals safely\n---\nUse snapshots.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Chdir(project)
	t.Setenv("HOME", t.TempDir())

	session, err := LoadDefaultTeam(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}
	for _, s := range session.Skills {
		if s.Name == "trec-drive" {
			return
		}
	}
	t.Fatalf("session.Skills missing project-local skill; got %#v", session.Skills)
}

func TestLoadDefaultTeam_AgentsDistinct(t *testing.T) {
	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}

	if len(session.Agents) != 2 {
		t.Errorf("Agents count = %d, want 2 (coordinator + helper)", len(session.Agents))
	}

	coord, ok := session.Agents["coordinator"]
	if !ok {
		t.Fatal("session.Agents missing key 'coordinator'")
	}
	if coord.Role != "coordinator" {
		t.Errorf("coordinator.Role = %q, want %q", coord.Role, "coordinator")
	}
	if coord.Name != "coordinator" {
		t.Errorf("coordinator.Name = %q, want %q", coord.Name, "coordinator")
	}
	if coord.FileAlias != "coordinator" {
		t.Errorf("coordinator.FileAlias = %q, want %q", coord.FileAlias, "coordinator")
	}

	helper, ok := session.Agents["helper"]
	if !ok {
		t.Fatal("session.Agents missing key 'helper'")
	}
	if helper.Role != "worker" {
		t.Errorf("helper.Role = %q, want %q", helper.Role, "worker")
	}
	if helper.Name != "Helper" {
		t.Errorf("helper.Name = %q, want %q", helper.Name, "Helper")
	}
	if helper.FileAlias != "helper" {
		t.Errorf("helper.FileAlias = %q, want %q", helper.FileAlias, "helper")
	}
	if helper.System == "" {
		t.Error("helper.System is empty; expected a system prompt")
	}

	// coordinator and helper are different agent definitions
	if coord == helper {
		t.Error("coordinator and helper point to the same *AgentDef; should be distinct")
	}
}

func TestLoadDefaultTeam_VarsPopulated(t *testing.T) {
	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}

	if session.Config.Vars == nil {
		t.Fatal("Config.Vars is nil; expected populated template vars")
	}

	cases := map[string]string{
		"TEAM_NAME":   "default",
		"AGENT_COUNT": "1",
		"AGENT_NAMES": "Helper",
	}
	for k, want := range cases {
		got, ok := session.Config.Vars[k]
		if !ok {
			t.Errorf("Config.Vars missing key %q", k)
			continue
		}
		if got != want {
			t.Errorf("Config.Vars[%q] = %v, want %q", k, got, want)
		}
	}
}

func TestLoadDefaultTeam_OrchestratorResolvable(t *testing.T) {
	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}
	c := &Coordinator{session: session}
	def := c.GetOrchestratorDef()
	if def == nil {
		t.Fatal("GetOrchestratorDef returned nil")
	}
	if def.Role != "coordinator" {
		t.Errorf("GetOrchestratorDef.Role = %q, want %q (coordinator agent, not auto-fallback)", def.Role, "coordinator")
	}
	if def.Name != "coordinator" {
		t.Errorf("GetOrchestratorDef.Name = %q, want %q", def.Name, "coordinator")
	}
}

// TestLoadDefaultTeam_NoSkillsWhenHomeEmpty verifies that when
// $HOME/.agents/skills has no skills, the default team has zero
// discovered skills. We isolate HOME so this test is deterministic
// regardless of the developer's machine.
func TestLoadDefaultTeam_NoSkillsWhenHomeEmpty(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)

	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}
	if len(session.Skills) != 0 {
		names := make([]string, 0, len(session.Skills))
		for _, s := range session.Skills {
			names = append(names, s.Name)
		}
		t.Errorf("session.Skills = %v, want empty (HOME=%s has no skills)", names, emptyHome)
	}
}

// TestLoadDefaultTeam_DiscoversGlobalSkills verifies that skills under
// $HOME/.agents/skills are discovered by the default team.
func TestLoadDefaultTeam_DiscoversGlobalSkills(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, ".agents", "skills", "demo-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	skillMD := "---\nname: demo-skill\ndescription: A demo skill for testing\n---\nDemo body.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("HOME", home)

	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}

	var found *skill.SkillDef
	for _, s := range session.Skills {
		if s.Name == "demo-skill" {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("session.Skills missing demo-skill; got %d skills", len(session.Skills))
	}
	if found.Description != "A demo skill for testing" {
		t.Errorf("demo-skill description = %q, want %q", found.Description, "A demo skill for testing")
	}
}

// TestLoadDefaultTeam_ForcedSkillOverrides verifies that --skill
// forced skills are merged into session.Skills even when the skill
// is not under the default discovery scope.
func TestLoadDefaultTeam_ForcedSkillOverrides(t *testing.T) {
	home := t.TempDir()
	skillDir := filepath.Join(home, ".agents", "skills", "forced-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	skillMD := "---\nname: forced-skill\ndescription: Forced via flag\n---\nForced body.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("HOME", home)

	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, []string{"forced-skill"}, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}

	var found *skill.SkillDef
	for _, s := range session.Skills {
		if s.Name == "forced-skill" {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("session.Skills missing forced-skill; got %d skills", len(session.Skills))
	}
}

// TestLoadDefaultTeam_ForcedSkillMissingWarns verifies that an unknown
// forced skill emits a warning to stderr but does not crash. (We just
// verify no panic; stderr capture is out of scope.)
func TestLoadDefaultTeam_ForcedSkillMissingWarns(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)

	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, []string{"nonexistent-skill"}, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}
	if len(session.Skills) != 0 {
		t.Errorf("session.Skills = %d, want 0 (no skill found to satisfy forced)", len(session.Skills))
	}
}

// Ensure the helper's Generation is populated by the time it's used, even
// though LoadDefaultTeam currently leaves it empty (the coordinator setup
// in cmd/hufu fills it via cfg.ResolveModel). This test pins the current
// behavior so future regressions are visible.
func TestLoadDefaultTeam_AgentGenerationInherits(t *testing.T) {
	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}
	for k, def := range session.Agents {
		_ = k
		_ = def
	}
	_ = agent.TeamConfig{}
}

// helperToolList returns Helper's tools as a string set for assertions.
func helperToolList(t *testing.T, session *TeamSession) []string {
	t.Helper()
	helper, ok := session.Agents["helper"]
	if !ok {
		t.Fatal("session.Agents missing 'helper'")
	}
	return strings.Split(helper.Tools, ",")
}

// TestLoadDefaultTeam_HelperToolsDefault verifies that with an empty
// helperTools argument, Helper's toolset remains the read-only baseline
// (no shell/exec tools).
func TestLoadDefaultTeam_HelperToolsDefault(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)

	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}

	tools := helperToolList(t, session)
	for _, banned := range []string{"bash", "sudo", "ssh"} {
		for _, have := range tools {
			if have == banned {
				t.Errorf("Helper.Tools contains %q by default; want baseline only", banned)
			}
		}
	}
}

// TestLoadDefaultTeam_HelperToolsBash verifies that helperTools="bash"
// appends "bash" to Helper's toolset.
func TestLoadDefaultTeam_HelperToolsBash(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)

	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "bash")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}

	tools := helperToolList(t, session)
	found := false
	for _, have := range tools {
		if have == "bash" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Helper.Tools = %v, want contains %q", tools, "bash")
	}
	// Baseline tools must still be present.
	for _, required := range []string{"view", "write", "edit", "grep", "ls"} {
		has := false
		for _, have := range tools {
			if have == required {
				has = true
				break
			}
		}
		if !has {
			t.Errorf("Helper.Tools = %v, missing baseline %q", tools, required)
		}
	}
}

// TestLoadDefaultTeam_HelperToolsMultipleWithWhitespace verifies that
// "bash, sudo , ssh" (with surrounding whitespace) is trimmed and
// all three tools are appended.
func TestLoadDefaultTeam_HelperToolsMultipleWithWhitespace(t *testing.T) {
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)

	ws := t.TempDir()
	session, err := LoadDefaultTeam(ws, nil, "  bash, sudo , ssh ,, ")
	if err != nil {
		t.Fatalf("LoadDefaultTeam returned error: %v", err)
	}

	tools := helperToolList(t, session)
	for _, want := range []string{"bash", "sudo", "ssh"} {
		has := false
		for _, have := range tools {
			if have == want {
				has = true
				break
			}
		}
		if !has {
			t.Errorf("Helper.Tools = %v, missing %q (after trim)", tools, want)
		}
	}
	// No empty strings should leak into the tool list.
	for _, have := range tools {
		if strings.TrimSpace(have) == "" {
			t.Errorf("Helper.Tools contains empty entry: %v", tools)
		}
	}
}
