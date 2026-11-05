package main

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

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

func TestApplyConfiguredBackendsAPIProtocol(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend config.BackendConfig
		want    string
		invalid bool
	}{
		{name: "legacy", backend: config.BackendConfig{Kind: "llm"}},
		{name: "explicit chat", backend: config.BackendConfig{Kind: "llm", APIProtocol: " CHAT-COMPLETIONS "}, want: config.APIProtocolChatCompletions},
		{name: "responses", backend: config.BackendConfig{Kind: "llm", APIProtocol: " Responses "}, want: config.APIProtocolResponses},
		{name: "auto", backend: config.BackendConfig{Kind: "llm", APIProtocol: "auto"}, invalid: true},
		{name: "wrong llm field", backend: config.BackendConfig{Kind: "llm", Protocol: "responses"}, invalid: true},
		{name: "agent field", backend: config.BackendConfig{Kind: "agent", Type: "codex-app-server", APIProtocol: "responses"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &team.TeamSession{Config: agent.TeamConfig{Backends: map[string]config.BackendConfig{"remote": tc.backend}}}
			err := applyConfiguredBackends(session, &config.Config{})
			if (err != nil) != tc.invalid {
				t.Fatalf("error=%v", err)
			}
			if !tc.invalid && session.Config.Providers["remote"].APIProtocol != tc.want {
				t.Fatalf("provider=%+v", session.Config.Providers["remote"])
			}
		})
	}
	t.Run("explicit empty agent field", func(t *testing.T) {
		var backend config.BackendConfig
		if err := yaml.Unmarshal([]byte("kind: agent\ntype: codex-app-server\napi-protocol: ''\n"), &backend); err != nil {
			t.Fatal(err)
		}
		session := &team.TeamSession{Config: agent.TeamConfig{Backends: map[string]config.BackendConfig{"remote": backend}}}
		if err := applyConfiguredBackends(session, &config.Config{}); err == nil {
			t.Fatal("accepted explicit agent api-protocol")
		}
	})
}

func TestApplyConfiguredBackendsAPIProtocolOverrides(t *testing.T) {
	for _, canonical := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy providers", true: "canonical backends"}[canonical], func(t *testing.T) {
			global := &config.Config{}
			session := &team.TeamSession{}
			if canonical {
				global.Backends = map[string]config.BackendConfig{"remote": {Kind: "llm", BaseURL: "https://fixture.invalid/v1", APIProtocol: "responses"}}
				session.Config.Backends = map[string]config.BackendConfig{"remote": {Kind: "llm", APIProtocol: " CHAT-COMPLETIONS "}}
			} else {
				global.Providers = map[string]config.ProviderConfig{"remote": {ProviderURL: "https://fixture.invalid/v1", APIProtocol: "responses"}}
				session.Config.Providers = map[string]config.ProviderConfig{"remote": {APIProtocol: " CHAT-COMPLETIONS "}}
			}
			if err := applyConfiguredBackends(session, global); err != nil {
				t.Fatal(err)
			}
			got := session.Config.Providers["remote"]
			if got.APIProtocol != config.APIProtocolChatCompletions || got.ProviderURL != "" {
				t.Fatalf("override=%+v", got)
			}
		})
	}
}
