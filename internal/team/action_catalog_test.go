package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const actionCatalogTestEntry = `  collect-debug-bundle:
    description: Collect bounded runtime diagnostics for one service.
    capability: diagnostics
    type: collect_debug_bundle
    agent: runtime-engineer
    side-effect: none
    input-schema:
      type: object
      properties:
        service:
          type: string
          min-length: 1
          max-length: 128
        include-thread-dump:
          type: boolean
      required-properties: [service]
      additional-properties: false
    output-schema:
      type: object
      properties:
        summary:
          type: string
      required-properties: [summary]
    access:
      discover: [runtime-engineer, network-engineer, critic]
      propose: [runtime-engineer, network-engineer]
    invocation:
      require-proposal: true
      allow-unattended: true
      max-invocations: 4
`

const actionCatalogTestProviders = `action-providers:
  diagnostics:
    command: [/opt/team-actions/diagnostics.sh]
    timeout: 120
`

func actionCatalogTestManifest(header, entries string) string {
	return "name: catalog-team\n" + header + actionCatalogTestProviders + "action-catalog:\n" + entries
}

// writeActionCatalogTeam writes a dynamic team with three workers and the
// given manifest.
func writeActionCatalogTeam(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, dir, "coordinator.md", "name: coordinator\nrole: coordinator\ndescription: Coordinates.", "Coordinate.")
	writeAgentFile(t, dir, "runtime-engineer.md", "name: runtime-engineer\ndescription: Runs diagnostics.\ntools: view", "Diagnose.")
	writeAgentFile(t, dir, "network-engineer.md", "name: network-engineer\ndescription: Checks networks.\ntools: view", "Check.")
	writeAgentFile(t, dir, "critic.md", "name: critic\ndescription: Critiques.\ntools: view", "Critique.")
	return dir
}

func loadActionCatalogTeam(t *testing.T, manifest string) *TeamSession {
	t.Helper()
	session, err := LoadTeam(writeActionCatalogTeam(t, manifest), nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	return session
}

func TestLoadActionCatalogNormalizesEntry(t *testing.T) {
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	entry, ok := session.ActionCatalog.Lookup("collect-debug-bundle")
	if !ok {
		t.Fatalf("catalog = %#v, want collect-debug-bundle", session.ActionCatalog)
	}
	if entry.Agent != "runtime-engineer" || entry.Capability != "diagnostics" || entry.Type != "collect_debug_bundle" ||
		entry.SideEffect != SideEffectNone || entry.Recovery != RecoveryRetry || entry.MaxInvocations != 4 ||
		!entry.RequireProposal || !entry.AllowUnattended {
		t.Fatalf("normalized entry = %#v", entry)
	}
	if strings.Join(entry.Discover, ",") != "critic,network-engineer,runtime-engineer" || strings.Join(entry.Propose, ",") != "network-engineer,runtime-engineer" {
		t.Fatalf("access = discover %v propose %v, want sorted canonical names", entry.Discover, entry.Propose)
	}
	if entry.OutputSchema == nil || entry.InputSchema.Type != "object" {
		t.Fatalf("schemas = input %#v output %#v", entry.InputSchema, entry.OutputSchema)
	}
	if !runInputHashPattern.MatchString(entry.Hash) || !runInputHashPattern.MatchString(entry.ProviderIdentityHash) || !runInputHashPattern.MatchString(session.ActionCatalog.Hash) {
		t.Fatalf("hashes entry=%q provider=%q snapshot=%q", entry.Hash, entry.ProviderIdentityHash, session.ActionCatalog.Hash)
	}
	again := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	if again.ActionCatalog.Hash != session.ActionCatalog.Hash {
		t.Fatalf("snapshot hash is not deterministic: %q vs %q", again.ActionCatalog.Hash, session.ActionCatalog.Hash)
	}
}

func TestLoadActionCatalogDefaults(t *testing.T) {
	entry := strings.Replace(actionCatalogTestEntry, "      max-invocations: 4\n", "", 1)
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", entry))
	got, _ := session.ActionCatalog.Lookup("collect-debug-bundle")
	if got.MaxInvocations != 1 || got.Recovery != RecoveryRetry {
		t.Fatalf("defaults = max %d recovery %q, want 1 and retry", got.MaxInvocations, got.Recovery)
	}
	if without := loadActionCatalogTeam(t, "name: plain-team\n"+actionCatalogTestProviders); without.ActionCatalog != nil {
		t.Fatalf("team without action-catalog has catalog %#v", without.ActionCatalog)
	}
}

func TestActionCatalogEntryHashCoversEveryField(t *testing.T) {
	base := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	baseEntry, _ := base.ActionCatalog.Lookup("collect-debug-bundle")
	tests := []struct {
		name          string
		old, new      string
		inProviders   bool
		providerDrift bool
	}{
		{name: "description", old: "bounded runtime", new: "bounded service"},
		{name: "type", old: "type: collect_debug_bundle", new: "type: collect_bundle"},
		{name: "agent", old: "agent: runtime-engineer", new: "agent: network-engineer"},
		{name: "recovery", old: "side-effect: none\n", new: "side-effect: none\n    recovery: manual\n"},
		{name: "input schema", old: "max-length: 128", new: "max-length: 64"},
		{name: "output schema", old: "required-properties: [summary]", new: "required-properties: []"},
		{name: "discover", old: "discover: [runtime-engineer, network-engineer, critic]", new: "discover: [runtime-engineer, network-engineer]"},
		{name: "require proposal", old: "require-proposal: true", new: "require-proposal: false"},
		{name: "allow unattended", old: "allow-unattended: true", new: "allow-unattended: false"},
		{name: "max invocations", old: "max-invocations: 4", new: "max-invocations: 5"},
		{name: "provider command", old: "diagnostics.sh]", new: "diagnostics-v2.sh]", inProviders: true, providerDrift: true},
		{name: "provider timeout", old: "timeout: 120", new: "timeout: 60", inProviders: true, providerDrift: true},
		{name: "provider dir", old: "    timeout: 120\n", new: "    timeout: 120\n    dir: /opt/team-actions\n", inProviders: true, providerDrift: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers, entry := actionCatalogTestProviders, actionCatalogTestEntry
			target := &entry
			if tt.inProviders {
				target = &providers
			}
			if !strings.Contains(*target, tt.old) {
				t.Fatalf("fixture does not contain %q", tt.old)
			}
			*target = strings.Replace(*target, tt.old, tt.new, 1)
			session := loadActionCatalogTeam(t, "name: catalog-team\n"+providers+"action-catalog:\n"+entry)
			got, ok := session.ActionCatalog.Lookup("collect-debug-bundle")
			if !ok {
				t.Fatal("mutated entry did not load")
			}
			if got.Hash == baseEntry.Hash || session.ActionCatalog.Hash == base.ActionCatalog.Hash {
				t.Fatalf("hash unchanged after %s change", tt.name)
			}
			if drift := got.ProviderIdentityHash != baseEntry.ProviderIdentityHash; drift != tt.providerDrift {
				t.Fatalf("provider identity changed = %v, want %v", drift, tt.providerDrift)
			}
		})
	}
}

// TestActionCatalogProviderIdentityPinsGoSourceButNotCommandScripts records
// the D19 boundary: a golang provider's source is part of its identity, a
// command provider's script is not.
func TestActionCatalogProviderIdentityPinsGoSourceButNotCommandScripts(t *testing.T) {
	goProviders := "action-providers:\n  diagnostics:\n    runtime: golang\n    source: ./actions/diagnostics\n    mode: trusted-static\n"
	writeGoAction := func(t *testing.T, dir, output string) {
		t.Helper()
		source := filepath.Join(dir, "actions", "diagnostics")
		if err := os.MkdirAll(source, 0o755); err != nil {
			t.Fatal(err)
		}
		program := "package main\n\nimport (\n\t\"context\"\n\t\"io\"\n)\n\nfunc Run(ctx context.Context, in io.Reader, out io.Writer) error {\n\t_, err := io.WriteString(out, `" + output + "`)\n\treturn err\n}\n"
		if err := os.WriteFile(filepath.Join(source, "main.go"), []byte(program), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := writeActionCatalogTeam(t, "name: catalog-team\n"+goProviders+"action-catalog:\n"+actionCatalogTestEntry)
	writeGoAction(t, dir, `{"outputs":{"summary":"a"}}`)
	first, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	writeGoAction(t, dir, `{"outputs":{"summary":"b"}}`)
	second, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	firstEntry, _ := first.ActionCatalog.Lookup("collect-debug-bundle")
	secondEntry, _ := second.ActionCatalog.Lookup("collect-debug-bundle")
	if firstEntry.ProviderIdentityHash == secondEntry.ProviderIdentityHash || first.ActionCatalog.Hash == second.ActionCatalog.Hash {
		t.Fatal("golang source change did not change the provider identity")
	}

	scriptDir := t.TempDir()
	script := filepath.Join(scriptDir, "diagnostics.sh")
	commandProviders := "action-providers:\n  diagnostics:\n    command: [" + script + "]\n"
	commandTeam := writeActionCatalogTeam(t, "name: catalog-team\n"+commandProviders+"action-catalog:\n"+actionCatalogTestEntry)
	hashes := make([]string, 0, 2)
	for _, content := range []string{"#!/bin/sh\necho one\n", "#!/bin/sh\necho two\n"} {
		if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		session, err := LoadTeam(commandTeam, nil, nil, DefaultProviderRegistry)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, session.ActionCatalog.Hash)
	}
	if hashes[0] != hashes[1] {
		t.Fatal("command script content unexpectedly changed the catalog hash")
	}
}

func TestCloneSessionIsolatesActionCatalog(t *testing.T) {
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	clone := cloneSession(session, t.TempDir())
	clone.ActionCatalog.Entries[0].Discover[0] = "mutated"
	clone.ActionCatalog.Entries[0].InputSchema.Properties["service"] = RunInputSchema{Type: "integer"}
	original, _ := session.ActionCatalog.Lookup("collect-debug-bundle")
	if original.Discover[0] == "mutated" || original.InputSchema.Properties["service"].Type != "string" {
		t.Fatalf("clone mutation leaked into the original catalog: %#v", original)
	}
}

func newActionCatalogCoordinator(t *testing.T, workspace, teamDir string, prepare func(*TeamSession)) *Coordinator {
	t.Helper()
	session, err := LoadTeam(teamDir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	session.Workspace = filepath.Join(workspace, "session")
	if err := os.MkdirAll(session.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(workspace, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := session.SetCompatibilityWorkspaceScope(project); err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(session)
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(func() {
		if c.eventStore != nil {
			_ = c.eventStore.Close()
		}
		c.CloseContextPreflight()
	})
	return c
}

func TestChangedActionCatalogProviderFailsResumeClosed(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	start := func(providers string) *Coordinator {
		teamDir := writeActionCatalogTeam(t, "name: catalog-team\n"+providers+"action-catalog:\n"+actionCatalogTestEntry)
		c := newActionCatalogCoordinator(t, workspace, teamDir, nil)
		if session := LoadSession(c.session.Workspace); session != nil {
			c.SetSessionData(session)
		}
		c.initEventStore()
		return c
	}
	first := start(actionCatalogTestProviders)
	if err := first.checkRunAdmission(); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	if first.ExecutionPolicySnapshot().ActionCatalogHash != first.session.ActionCatalog.Hash {
		t.Fatalf("policy snapshot catalog hash = %q, want %q", first.ExecutionPolicySnapshot().ActionCatalogHash, first.session.ActionCatalog.Hash)
	}
	closeWorkerModelResumeStore(t, first)

	changed := start(strings.Replace(actionCatalogTestProviders, "diagnostics.sh", "diagnostics-v2.sh", 1))
	if err := changed.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "snapshot drift detected") {
		t.Fatalf("resume with a changed provider command error = %v, want policy snapshot drift", err)
	}
	closeWorkerModelResumeStore(t, changed)

	same := start(actionCatalogTestProviders)
	if err := same.checkRunAdmission(); err != nil {
		t.Fatalf("resume with an unchanged catalog: %v", err)
	}
}

func TestValidateActionCatalogProposersUsesResolvedWorkerTargets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	teamDir := writeActionCatalogTeam(t, actionCatalogTestManifest("default-llm-backend: ollama\n", actionCatalogTestEntry))
	tests := []struct {
		name    string
		models  map[string]string
		wantErr bool
	}{
		{name: "local proposers", models: map[string]string{"runtime-engineer": "ollama/a", "network-engineer": "ollama/b"}},
		{name: "one proposer moved to codex", models: map[string]string{"runtime-engineer": "codex/gpt-a", "network-engineer": "ollama/b"}},
		{name: "every proposer moved to codex", models: map[string]string{"runtime-engineer": "codex/gpt-a", "network-engineer": "codex/gpt-b"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newActionCatalogCoordinator(t, t.TempDir(), teamDir, func(session *TeamSession) {
				for name, model := range tt.models {
					session.Agents[name].Generation.Model = model
				}
			})
			err := c.ValidateActionCatalogProposers()
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateActionCatalogProposers() error = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), FindingActionCatalogProposerUnreachable) {
				t.Fatalf("error %q does not name %s", err, FindingActionCatalogProposerUnreachable)
			}
		})
	}
}
