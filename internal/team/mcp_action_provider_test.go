package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/mcp"
)

func mcpActionTestServers() map[string]mcp.MCPServerConfig {
	return map[string]mcp.MCPServerConfig{
		"diagnostics": {Type: "local", Command: []string{"diagnostics-mcp"}, AllowedTools: []string{"collect_debug"}},
		"excluding":   {Type: "local", Command: []string{"other-mcp"}, ExcludedTools: []string{"collect_debug"}},
		"streaming":   {Type: "sse", URL: "https://mcp.example.test"},
	}
}

func TestRegisterMCPActionProviderValidatesConfiguration(t *testing.T) {
	valid := agent.ActionProviderConfig{Runtime: "mcp", Server: "diagnostics", Tool: "collect_debug", Timeout: 30}
	with := func(mutate func(*agent.ActionProviderConfig)) agent.ActionProviderConfig {
		config := valid
		mutate(&config)
		return config
	}
	tests := []struct {
		name    string
		config  agent.ActionProviderConfig
		wantErr string
	}{
		{name: "valid", config: valid},
		{name: "runtime case and spaces", config: with(func(c *agent.ActionProviderConfig) {
			c.Runtime, c.Server, c.Tool = " MCP ", " diagnostics ", " collect_debug "
		})},
		{name: "missing server", config: with(func(c *agent.ActionProviderConfig) { c.Server = " " }), wantErr: "requires server and tool"},
		{name: "missing tool", config: with(func(c *agent.ActionProviderConfig) { c.Tool = "" }), wantErr: "requires server and tool"},
		{name: "command", config: with(func(c *agent.ActionProviderConfig) { c.Command = []string{"sh"} }), wantErr: "does not accept command"},
		{name: "dir", config: with(func(c *agent.ActionProviderConfig) { c.Dir = "." }), wantErr: "does not accept command"},
		{name: "source", config: with(func(c *agent.ActionProviderConfig) { c.Source = "./action" }), wantErr: "does not accept command"},
		{name: "mode", config: with(func(c *agent.ActionProviderConfig) { c.Mode = "trusted-static" }), wantErr: "does not accept command"},
		{name: "negative timeout", config: with(func(c *agent.ActionProviderConfig) { c.Timeout = -1 }), wantErr: "cannot be negative"},
		{name: "undeclared server", config: with(func(c *agent.ActionProviderConfig) { c.Server = "missing" }), wantErr: "mcp-servers does not declare"},
		{name: "server name is exact", config: with(func(c *agent.ActionProviderConfig) { c.Server = "Diagnostics" }), wantErr: "mcp-servers does not declare"},
		{name: "unsupported server type", config: with(func(c *agent.ActionProviderConfig) { c.Server = "streaming" }), wantErr: "unsupported type"},
		{name: "tool outside allowedTools", config: with(func(c *agent.ActionProviderConfig) { c.Tool = "restart" }), wantErr: "do not allow tool"},
		{name: "excluded tool", config: with(func(c *agent.ActionProviderConfig) { c.Server = "excluding" }), wantErr: "do not allow tool"},
		{name: "command runtime with server", config: agent.ActionProviderConfig{Command: []string{"sh"}, Server: "diagnostics"}, wantErr: "command runtime does not accept server or tool"},
		{name: "golang runtime with tool", config: agent.ActionProviderConfig{Runtime: "golang", Source: "./action", Mode: "trusted-static", Tool: "collect_debug"}, wantErr: "golang runtime does not accept server or tool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := NewProviderRegistry()
			err := registerConfiguredActionProviders(registry, map[string]agent.ActionProviderConfig{"diagnostics": tt.config}, t.TempDir(), mcpActionTestServers())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			if name := registry.ProviderName("diagnostics"); name != "mcp:diagnostics/collect_debug" {
				t.Fatalf("provider name = %q", name)
			}
		})
	}
}

func TestMCPServerConfigHash(t *testing.T) {
	base := mcp.MCPServerConfig{
		Command: []string{"diagnostics-mcp", "--stdio"}, AllowedTools: []string{"b", "a"},
		Environment: map[string]string{"TOKEN": "secret-value", "REGION": "eu"},
	}
	hash := mcpServerConfigHash(base)
	if !strings.HasPrefix(hash, "sha256:") {
		t.Fatalf("hash = %q", hash)
	}
	reordered := base
	reordered.Type = "local"
	reordered.AllowedTools = []string{"a", "b"}
	if got := mcpServerConfigHash(reordered); got != hash {
		t.Fatalf("equivalent configuration hash %q, want %q", got, hash)
	}
	for name, changed := range map[string]mcp.MCPServerConfig{
		"environment value": {Command: base.Command, AllowedTools: base.AllowedTools, Environment: map[string]string{"TOKEN": "other", "REGION": "eu"}},
		"command":           {Command: []string{"diagnostics-mcp"}, AllowedTools: base.AllowedTools, Environment: base.Environment},
		"excluded tools":    {Command: base.Command, AllowedTools: base.AllowedTools, ExcludedTools: []string{"c"}, Environment: base.Environment},
		"remote":            {Type: "remote", URL: "https://mcp.example.test", AllowedTools: base.AllowedTools, Environment: base.Environment},
	} {
		if mcpServerConfigHash(changed) == hash {
			t.Fatalf("%s change kept hash %q", name, hash)
		}
	}
}

const mcpActionTestManifest = `name: catalog-team
mcp-servers:
  diagnostics:
    type: local
    command: [sh, -c, "touch MARKER"]
    allowedTools: [collect_debug]
action-providers:
  diagnostics:
    runtime: mcp
    server: diagnostics
    tool: collect_debug
    timeout: 120
action-catalog:
`

func TestLoadTeamValidatesMCPActionProviderOffline(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	manifest := strings.Replace(mcpActionTestManifest, "MARKER", marker, 1) + actionCatalogTestEntry
	dir := writeActionCatalogTeam(t, manifest)
	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if name := session.ProviderRegistry.ProviderName("diagnostics"); name != "mcp:diagnostics/collect_debug" {
		t.Fatalf("provider name = %q", name)
	}
	entry, ok := session.ActionCatalog.Lookup("collect-debug-bundle")
	if !ok || entry.ProviderIdentityHash == "" {
		t.Fatalf("catalog entry = %#v", entry)
	}
	result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LintTeam: %v", err)
	}
	for _, finding := range result.Findings {
		if finding.Severity == FindingSeverityError {
			t.Fatalf("lint error finding %s: %s", finding.Code, finding.Message)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("loading or linting started the MCP server (marker stat err = %v)", err)
	}
}

func TestActionProviderIdentityAddsMCPFieldsOnlyForMCPProviders(t *testing.T) {
	commandTeam := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	identity, ok := configuredActionProviderIdentity(commandTeam, "diagnostics")
	if !ok {
		t.Fatal("command provider identity missing")
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"server"`, `"tool"`, `"server_config_hash"`} {
		if strings.Contains(string(encoded), key) {
			t.Fatalf("command provider identity %s contains %s", encoded, key)
		}
	}

	manifest := strings.Replace(mcpActionTestManifest, "MARKER", filepath.Join(t.TempDir(), "started"), 1) + actionCatalogTestEntry
	mcpTeam := loadActionCatalogTeam(t, manifest)
	identity, ok = configuredActionProviderIdentity(mcpTeam, "diagnostics")
	if !ok || identity.Server != "diagnostics" || identity.Tool != "collect_debug" || !strings.HasPrefix(identity.ServerConfigHash, "sha256:") {
		t.Fatalf("mcp provider identity = %#v", identity)
	}
	changed := strings.Replace(manifest, "allowedTools: [collect_debug]", "allowedTools: [collect_debug, restart]", 1)
	changedTeam := loadActionCatalogTeam(t, changed)
	before, _ := mcpTeam.ActionCatalog.Lookup("collect-debug-bundle")
	after, _ := changedTeam.ActionCatalog.Lookup("collect-debug-bundle")
	if before.Hash == after.Hash || before.ProviderIdentityHash == after.ProviderIdentityHash {
		t.Fatal("changing the MCP server configuration kept the catalog entry hash")
	}
}

func mcpActionFindingSession() *TeamSession {
	return &TeamSession{
		Config: agent.TeamConfig{ActionProviders: map[string]agent.ActionProviderConfig{
			"diagnostics": {Runtime: "mcp", Server: "diagnostics", Tool: "collect_debug"},
			"prepare":     {Command: []string{"prepare.sh"}},
		}},
	}
}

func mcpFindingCodes(findings []ContractFinding) []string {
	codes := make([]string, 0, len(findings))
	for _, finding := range findings {
		codes = append(codes, finding.Code)
	}
	return codes
}

func TestMCPActionTaskFindings(t *testing.T) {
	action := func(capability, payload string) *Action {
		return &Action{Capability: capability, Type: "collect_debug", Payload: payload}
	}
	tests := []struct {
		name string
		task TaskDef
		want []string
	}{
		{name: "valid", task: TaskDef{SideEffect: SideEffectNone, Action: action("diagnostics", `{"service":"api"}`)}},
		{name: "omitted side effect", task: TaskDef{Action: action("diagnostics", `{}`)}, want: []string{FindingMCPActionSideEffectUnsupported}},
		{name: "workspace write", task: TaskDef{SideEffect: SideEffectWorkspaceWrite, Action: action("diagnostics", `{}`)}, want: []string{FindingMCPActionSideEffectUnsupported}},
		{name: "array payload", task: TaskDef{SideEffect: SideEffectNone, Action: action("diagnostics", `["api"]`)}, want: []string{FindingMCPActionPayloadInvalid}},
		{name: "empty payload", task: TaskDef{SideEffect: SideEffectNone, Action: action("diagnostics", "")}, want: []string{FindingMCPActionPayloadInvalid}},
		{name: "duplicate key", task: TaskDef{SideEffect: SideEffectNone, Action: action("diagnostics", `{"a":1,"a":2}`)}, want: []string{FindingMCPActionPayloadInvalid}},
		{name: "two values", task: TaskDef{SideEffect: SideEffectNone, Action: action("diagnostics", `{} {}`)}, want: []string{FindingMCPActionPayloadInvalid}},
		{name: "oversized", task: TaskDef{SideEffect: SideEffectNone, Action: action("diagnostics", `{"a":"`+strings.Repeat("x", maxMCPActionPayloadBytes)+`"}`)}, want: []string{FindingMCPActionPayloadInvalid}},
		{name: "bound payload is checked at runtime", task: TaskDef{SideEffect: SideEffectNone, Action: &Action{
			Capability: "diagnostics", Type: "collect_debug", Payload: "{{scope}}",
			InputBindings: []ActionInputBinding{{Input: "scope"}},
		}}},
		{name: "command provider unaffected", task: TaskDef{Action: action("prepare", "not json")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, code := range mcpFindingCodes(validateActionTaskContract("tasks[0]", tt.task, mcpActionFindingSession())) {
				if strings.HasPrefix(code, "mcp_action_") {
					got = append(got, code)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("codes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMCPActionTeamFindings(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*TeamSession)
		want    []string
	}{
		{name: "no findings", prepare: func(s *TeamSession) {
			s.Agents = map[string]*agent.AgentDef{"worker": {Name: "worker", Tools: "view, diagnostics__other"}}
		}},
		{name: "resolver", prepare: func(s *TeamSession) {
			s.RunInputDefinitions = []RunInputDefinition{
				{Name: "scope", Resolver: &RunInputResolverSpec{Capability: "diagnostics"}},
				{Name: "other", Resolver: &RunInputResolverSpec{Capability: "prepare"}},
			}
		}, want: []string{FindingMCPActionResolverUnsupported}},
		{name: "reserved tool", prepare: func(s *TeamSession) {
			def := &agent.AgentDef{Name: "Worker", Tools: "view, Diagnostics__collect_debug"}
			s.Agents = map[string]*agent.AgentDef{"worker": def, "alias": def}
		}, want: []string{FindingMCPActionToolReserved}},
		{name: "tool name is exact", prepare: func(s *TeamSession) {
			s.Agents = map[string]*agent.AgentDef{"worker": {Name: "worker", Tools: "diagnostics__Collect_Debug"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := mcpActionFindingSession()
			tt.prepare(session)
			got := mcpFindingCodes(validateMCPActionProviders(session))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("codes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMCPActionCatalogEntryMustBeSideEffectNone(t *testing.T) {
	entry := strings.Replace(actionCatalogTestEntry, "side-effect: none\n", "side-effect: workspace_write\n    recovery: manual\n", 1)
	manifest := strings.Replace(mcpActionTestManifest, "MARKER", filepath.Join(t.TempDir(), "started"), 1) + entry
	dir := writeActionCatalogTeam(t, manifest)
	result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LintTeam: %v", err)
	}
	found := false
	for _, finding := range result.Findings {
		found = found || (finding.Code == FindingMCPActionSideEffectUnsupported && finding.Severity == FindingSeverityError)
	}
	if !found {
		t.Fatalf("lint findings lack %s", FindingMCPActionSideEffectUnsupported)
	}
	if _, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry); err == nil || !strings.Contains(err.Error(), "must be side-effect: none") {
		t.Fatalf("LoadTeam error = %v, want the MCP side-effect finding", err)
	}
}
