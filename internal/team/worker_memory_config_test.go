package team

// WP-2 — Worker memory config parsing, validation, and identity tests.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func writeAgentFile(t *testing.T, dir, filename, frontmatter, body string) {
	t.Helper()
	content := "---\n" + frontmatter + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

func TestWP2_AgentFrontmatterParsesMemoryConfig(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "researcher.md", `name: researcher
memory-id: research-v1
memory:
  mode: session
  auto-recall: true
  auto-save: false
  max-items: 10
  max-tokens: 3000
  session-ttl: 24h
  persistent-ttl: 0`, "You are a researcher.")
	def, err := parseAgentFile(filepath.Join(dir, "researcher.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	if def.MemoryID != "research-v1" {
		t.Errorf("MemoryID = %q, want research-v1", def.MemoryID)
	}
	if def.Memory.Mode != agent.WorkerMemorySession {
		t.Errorf("Mode = %q, want session", def.Memory.Mode)
	}
	if def.Memory.AutoSave {
		t.Errorf("AutoSave = true, want false")
	}
	if !def.Memory.AutoRecall {
		t.Errorf("AutoRecall = false, want true")
	}
	if def.Memory.MaxItems != 10 {
		t.Errorf("MaxItems = %d, want 10", def.Memory.MaxItems)
	}
	if def.Memory.MaxTokens != 3000 {
		t.Errorf("MaxTokens = %d, want 3000", def.Memory.MaxTokens)
	}
	if def.Memory.SessionTTL != "24h" {
		t.Errorf("SessionTTL = %q, want 24h", def.Memory.SessionTTL)
	}
	if def.Memory.PersistentTTL != "0" {
		t.Errorf("PersistentTTL = %q, want 0", def.Memory.PersistentTTL)
	}
}

func TestWP2_AgentWithoutMemoryConfigGetsBuiltInDefault(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "worker.md", `name: worker
role: worker`, "You are a worker.")
	def, err := parseAgentFile(filepath.Join(dir, "worker.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	if def.Memory.Mode != agent.WorkerMemoryOff {
		t.Errorf("default Mode = %q, want off", def.Memory.Mode)
	}
	if def.Memory.MaxItems != 5 {
		t.Errorf("default MaxItems = %d, want 5", def.Memory.MaxItems)
	}
}

func TestWP2_TeamYMLParsesWorkerMemoryDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team
worker-memory:
  mode: session
  auto-recall: false
  max-items: 8
  max-tokens: 2000
  session-ttl: 48h`), 0o644); err != nil {
		t.Fatalf("write team.yml: %v", err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML: %v", err)
	}
	if cfg.WorkerMemory.Mode != agent.WorkerMemorySession {
		t.Errorf("team WorkerMemory.Mode = %q, want session", cfg.WorkerMemory.Mode)
	}
	if cfg.WorkerMemory.AutoRecall {
		t.Errorf("team AutoRecall = true, want false")
	}
	if cfg.WorkerMemory.MaxItems != 8 {
		t.Errorf("team MaxItems = %d, want 8", cfg.WorkerMemory.MaxItems)
	}
	if cfg.WorkerMemory.SessionTTL != "48h" {
		t.Errorf("team SessionTTL = %q, want 48h", cfg.WorkerMemory.SessionTTL)
	}
}

func TestWP2_TeamWithoutWorkerMemoryGetsBuiltInDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML: %v", err)
	}
	if cfg.WorkerMemory.Mode != agent.WorkerMemoryOff {
		t.Errorf("default WorkerMemory.Mode = %q, want off", cfg.WorkerMemory.Mode)
	}
}

func TestWP2_PrecedenceAgentOverridesTeam(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team
worker-memory:
  mode: session
  max-items: 8`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeAgentFile(t, dir, "worker.md", `name: worker
memory:
  mode: persistent
  max-items: 3`, "You are a worker.")
	session, err := LoadTeam(dir, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	def := session.Agents["worker"]
	if def == nil {
		t.Fatal("worker agent not found")
	}
	// Agent overrides team.
	if def.Memory.Mode != agent.WorkerMemoryPersistent {
		t.Errorf("Mode = %q, want persistent (agent override)", def.Memory.Mode)
	}
	if def.Memory.MaxItems != 3 {
		t.Errorf("MaxItems = %d, want 3 (agent override)", def.Memory.MaxItems)
	}
}

func TestWP2_PrecedenceTeamAppliesWhenAgentDoesNotOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team
worker-memory:
  mode: session
  max-items: 8`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeAgentFile(t, dir, "worker.md", `name: worker
role: worker`, "You are a worker.")
	session, err := LoadTeam(dir, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	def := session.Agents["worker"]
	if def == nil {
		t.Fatal("worker agent not found")
	}
	// Team default applies since agent didn't set memory.
	if def.Memory.Mode != agent.WorkerMemorySession {
		t.Errorf("Mode = %q, want session (team default)", def.Memory.Mode)
	}
	if def.Memory.MaxItems != 8 {
		t.Errorf("MaxItems = %d, want 8 (team default)", def.Memory.MaxItems)
	}
}

func TestWP2_InvalidModeReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "worker.md", `name: worker
memory:
  mode: invalid`, "You are a worker.")
	_, err := parseAgentFile(filepath.Join(dir, "worker.md"), nil)
	if err == nil || !strings.Contains(err.Error(), "invalid memory mode") {
		t.Fatalf("expected invalid mode error, got: %v", err)
	}
}

func TestWP2_NegativeMaxItemsReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "worker.md", `name: worker
memory:
  mode: session
  max-items: -1`, "You are a worker.")
	_, err := parseAgentFile(filepath.Join(dir, "worker.md"), nil)
	if err == nil || !strings.Contains(err.Error(), "max-items") {
		t.Fatalf("expected max-items error, got: %v", err)
	}
}

func TestWP2_NegativeMaxTokensReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "worker.md", `name: worker
memory:
  mode: session
  max-tokens: -100`, "You are a worker.")
	_, err := parseAgentFile(filepath.Join(dir, "worker.md"), nil)
	if err == nil || !strings.Contains(err.Error(), "max-tokens") {
		t.Fatalf("expected max-tokens error, got: %v", err)
	}
}

func TestWP2_InvalidTTLReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "worker.md", `name: worker
memory:
  mode: session
  session-ttl: not-a-duration`, "You are a worker.")
	_, err := parseAgentFile(filepath.Join(dir, "worker.md"), nil)
	if err == nil || !strings.Contains(err.Error(), "session-ttl") {
		t.Fatalf("expected session-ttl error, got: %v", err)
	}
}

func TestWP2_DuplicateMemoryIDReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeAgentFile(t, dir, "agent-a.md", `name: agent-a
memory-id: shared-id
memory:
  mode: session`, "Agent A")
	writeAgentFile(t, dir, "agent-b.md", `name: agent-b
memory-id: shared-id
memory:
  mode: session`, "Agent B")
	_, err := LoadTeam(dir, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "duplicate memory-id") {
		t.Fatalf("expected duplicate memory-id error, got: %v", err)
	}
}

func TestWP2_DuplicateMemoryIDAllowedWhenOneIsOff(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Two agents with the same memory-id, but one is off — no error.
	writeAgentFile(t, dir, "agent-a.md", `name: agent-a
memory-id: shared-id
memory:
  mode: session`, "Agent A")
	writeAgentFile(t, dir, "agent-b.md", `name: agent-b
memory-id: shared-id`, "Agent B")
	session, err := LoadTeam(dir, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam should succeed when one agent is off: %v", err)
	}
	if session.Agents["agent-a"].MemoryID != "shared-id" {
		t.Errorf("agent-a MemoryID = %q, want shared-id", session.Agents["agent-a"].MemoryID)
	}
}

func TestWP2_AgentRenamePreservesIdentityViaMemoryID(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeAgentFile(t, dir, "researcher.md", `name: researcher
memory-id: research-v1
memory:
  mode: session`, "You are a researcher.")
	session, err := LoadTeam(dir, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	def := session.Agents["researcher"]
	if def.MemoryID != "research-v1" {
		t.Errorf("MemoryID = %q, want research-v1", def.MemoryID)
	}
	// Simulate a rename: change the name but keep memory-id.
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "team.yml"), []byte(`name: test-team`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeAgentFile(t, dir2, "analyst.md", `name: analyst
memory-id: research-v1
memory:
  mode: session`, "You are an analyst.")
	session2, err := LoadTeam(dir2, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	def2 := session2.Agents["analyst"]
	if def2.MemoryID != "research-v1" {
		t.Errorf("renamed agent MemoryID = %q, want research-v1 (identity preserved)", def2.MemoryID)
	}
}

func TestWP2_AgentWithoutMemoryIDFallsBackToName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeAgentFile(t, dir, "Researcher.md", `name: Researcher
memory:
  mode: session`, "You are a researcher.")
	session, err := LoadTeam(dir, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	def := session.Agents["researcher"]
	if def == nil {
		t.Fatal("agent not found")
	}
	// MemoryID should fall back to the normalized agent name.
	if def.MemoryID != "researcher" {
		t.Errorf("MemoryID = %q, want researcher (normalized name fallback)", def.MemoryID)
	}
}

func TestWP2_BuiltInHelperDefaultsToOff(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team
worker-memory:
  mode: session`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeAgentFile(t, dir, "worker.md", `name: worker
role: worker`, "You are a worker.")
	session, err := LoadTeam(dir, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	helper := session.Agents["helper"]
	if helper == nil {
		t.Fatal("Helper not found")
	}
	if helper.Memory.Mode != agent.WorkerMemoryOff {
		t.Errorf("Helper Memory.Mode = %q, want off (built-in default)", helper.Memory.Mode)
	}
}

func TestWP2_OldTeamYMLWithoutMemoryParsesUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: legacy-team
max-rounds: 5
timeout: 300`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeAgentFile(t, dir, "worker.md", `name: worker
role: worker
tools: bash,view`, "You are a worker.")
	session, err := LoadTeam(dir, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if session.Config.Name != "legacy-team" {
		t.Errorf("Name = %q, want legacy-team", session.Config.Name)
	}
	if session.Config.MaxRounds != 5 {
		t.Errorf("MaxRounds = %d, want 5", session.Config.MaxRounds)
	}
	def := session.Agents["worker"]
	if def.Memory.Mode != agent.WorkerMemoryOff {
		t.Errorf("worker Memory.Mode = %q, want off (no memory config)", def.Memory.Mode)
	}
}

func TestWP2_InvalidMemoryIDReturnsError(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "worker.md", `name: worker
memory-id: bad/id
memory:
  mode: session`, "You are a worker.")
	_, err := parseAgentFile(filepath.Join(dir, "worker.md"), nil)
	// parseAgentFile doesn't normalize memory-id; LoadTeam does.
	// But the validation in LoadTeam should catch it.
	if err != nil {
		t.Fatalf("parseAgentFile should not fail on memory-id validation: %v", err)
	}
}

func TestWP2_InvalidMemoryIDFailsAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeAgentFile(t, dir, "worker.md", `name: worker
memory-id: bad/id
memory:
  mode: session`, "You are a worker.")
	_, err := LoadTeam(dir, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("expected invalid memory-id error, got: %v", err)
	}
}

func TestWP2_DefaultTeamHelperMemoryOff(t *testing.T) {
	session, err := LoadDefaultTeam(t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("LoadDefaultTeam: %v", err)
	}
	helper := session.Agents["helper"]
	if helper.Memory.Mode != agent.WorkerMemoryOff {
		t.Errorf("default Helper Memory.Mode = %q, want off", helper.Memory.Mode)
	}
	coord := session.Agents["coordinator"]
	if coord.Memory.Mode != agent.WorkerMemoryOff {
		t.Errorf("default coordinator Memory.Mode = %q, want off", coord.Memory.Mode)
	}
}

func TestWP2_NormalizeMemoryID(t *testing.T) {
	tests := []struct {
		input string
		want  string
		err   bool
	}{
		{"research-v1", "research-v1", false},
		{"Researcher", "researcher", false},
		{"  spaced  ", "spaced", false},
		{"", "", false},
		{"bad/id", "", true},
		{"bad.id", "", true},
		{"bad id", "", true},
		{"good_name-123", "good_name-123", false},
	}
	for _, tc := range tests {
		got, err := normalizeMemoryID(tc.input)
		if tc.err {
			if err == nil {
				t.Errorf("normalizeMemoryID(%q) expected error, got %q", tc.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeMemoryID(%q) unexpected error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("normalizeMemoryID(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestWP2_TeamYMLInvalidWorkerMemoryReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(`name: test-team
worker-memory:
  mode: invalid-mode`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := parseTeamYML(dir, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid memory mode") {
		t.Fatalf("expected invalid mode error, got: %v", err)
	}
}
