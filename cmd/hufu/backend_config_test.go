package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

func TestApplyConfiguredBackendsMapsLLMAndCodex(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{Backends: map[string]config.BackendConfig{
		"lemonade": {Kind: "llm", Type: "openai-compatible", BaseURL: "http://127.0.0.1:8000/v1"},
		"codex":    {Kind: "agent", Type: "codex-app-server"},
	}}}
	if err := applyConfiguredBackends(session, &config.Config{}); err != nil {
		t.Fatalf("applyConfiguredBackends() error = %v", err)
	}
	if got := session.Config.Providers["lemonade"].ProviderURL; got != "http://127.0.0.1:8000/v1" {
		t.Errorf("lemonade URL = %q", got)
	}
	if got := session.Config.SubagentProviders["codex"].Type; got != "codex-app-server" {
		t.Errorf("codex type = %q", got)
	}
}

func TestApplyConfiguredBackendsRejectsLegacyNamespaceCollision(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{
		Providers: map[string]config.ProviderConfig{"lemonade": {}},
		Backends:  map[string]config.BackendConfig{"lemonade": {Kind: "llm", Type: "openai-compatible"}},
	}}
	err := applyConfiguredBackends(session, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "both backends and legacy providers") {
		t.Fatalf("applyConfiguredBackends() error = %v, want collision", err)
	}
}

func TestApplyConfiguredBackendsRejectsReservedLocalAgent(t *testing.T) {
	t.Run("legacy subagent provider", func(t *testing.T) {
		session := &team.TeamSession{Config: agent.TeamConfig{SubagentProviders: map[string]agent.SubagentProviderConfig{
			"local": {Type: "codex-app-server", Command: []string{"codex", "app-server"}},
		}}}
		err := applyConfiguredBackends(session, &config.Config{})
		if err == nil || !strings.Contains(err.Error(), "reserved backend name") {
			t.Fatalf("applyConfiguredBackends() error = %v, want reserved local rejection", err)
		}
	})
	t.Run("canonical backend", func(t *testing.T) {
		session := &team.TeamSession{Config: agent.TeamConfig{Backends: map[string]config.BackendConfig{
			"local": {Kind: "agent", Type: "codex-app-server", Command: []string{"codex", "app-server"}},
		}}}
		err := applyConfiguredBackends(session, &config.Config{})
		if err == nil || !strings.Contains(err.Error(), "reserved backend name") {
			t.Fatalf("applyConfiguredBackends() error = %v, want reserved local rejection", err)
		}
	})
}

func TestApplyConfiguredBackendsCodexUsesPresenceAwareBuiltInOverlay(t *testing.T) {
	var parsed struct {
		Backends map[string]config.BackendConfig `yaml:"backends"`
	}
	if err := yaml.Unmarshal([]byte(`backends:
  codex:
    kind: agent
    type: codex-app-server
    command: [hufu-coding, app-server]
    inherit-env: []
    startup-timeout: ""
`), &parsed); err != nil {
		t.Fatal(err)
	}
	session := &team.TeamSession{Config: agent.TeamConfig{Backends: parsed.Backends}}
	if err := applyConfiguredBackends(session, &config.Config{}); err != nil {
		t.Fatalf("applyConfiguredBackends() error = %v", err)
	}
	codex := session.Config.SubagentProviders["codex"]
	if got, want := strings.Join(codex.Command, " "), "hufu-coding app-server"; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
	if codex.InheritEnv == nil || len(codex.InheritEnv) != 0 {
		t.Errorf("inherit env = %#v, want explicit empty allowlist", codex.InheritEnv)
	}
	if codex.Protocol != "app-server-v2" || codex.ExecutionWorld != "local-sandbox" {
		t.Errorf("omitted scalars did not inherit built-ins: %#v", codex)
	}
	if codex.StartupTimeout != "" {
		t.Errorf("explicit empty startup-timeout = %q, want empty replacement", codex.StartupTimeout)
	}
}

func TestApplyConfiguredBackendsCodexRejectsExplicitEmptyCommand(t *testing.T) {
	var parsed struct {
		Backends map[string]config.BackendConfig `yaml:"backends"`
	}
	if err := yaml.Unmarshal([]byte("backends:\n  codex:\n    kind: agent\n    type: codex-app-server\n    command: []\n"), &parsed); err != nil {
		t.Fatal(err)
	}
	err := applyConfiguredBackends(&team.TeamSession{Config: agent.TeamConfig{Backends: parsed.Backends}}, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "command must contain") {
		t.Fatalf("applyConfiguredBackends() error = %v, want explicit command failure", err)
	}
}

func TestApplyConfiguredBackendsCanonicalizesAliasesAndRejectsDuplicates(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{Backends: map[string]config.BackendConfig{
		"ollama": {Kind: "llm", Type: "ollama"},
	}}}
	if err := applyConfiguredBackends(session, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := session.Config.Providers["ollama"]; !ok {
		t.Fatalf("canonical Ollama provider missing: %#v", session.Config.Providers)
	}
	err := applyConfiguredBackends(&team.TeamSession{Config: agent.TeamConfig{Backends: map[string]config.BackendConfig{
		"ollama": {Kind: "llm", Type: "ollama"},
		"local":  {Kind: "llm", Type: "ollama"},
	}}}, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "aliases resolve") {
		t.Fatalf("applyConfiguredBackends() error = %v, want alias collision", err)
	}
}

func TestApplyConfiguredBackendsIncludesGlobalLegacyProvidersInUnifiedNamespace(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{Backends: map[string]config.BackendConfig{
		"lemonade": {Kind: "llm", Type: "openai-compatible"},
	}}}
	err := applyConfiguredBackends(session, &config.Config{Providers: map[string]config.ProviderConfig{"lemonade": {}}})
	if err == nil || !strings.Contains(err.Error(), "both backends and legacy providers") {
		t.Fatalf("applyConfiguredBackends() error = %v, want global legacy collision", err)
	}
}

func TestApplyConfiguredBackendsRejectsLegacyAliasCollision(t *testing.T) {
	err := applyConfiguredBackends(&team.TeamSession{Config: agent.TeamConfig{Providers: map[string]config.ProviderConfig{
		"ollama": {}, "local": {},
	}}}, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "aliases resolve") {
		t.Fatalf("applyConfiguredBackends() error = %v, want alias collision", err)
	}
}
