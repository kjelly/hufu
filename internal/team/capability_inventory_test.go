package team

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/skill"
)

func TestInspectCapabilitiesSkillPrecedenceAndDrafts(t *testing.T) {
	project, home := t.TempDir(), t.TempDir()
	t.Chdir(project)
	t.Setenv("HOME", home)
	dir := lintTeamWithCoordinator(t, "")
	writeSkill := func(root, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		writeLintTestFile(t, filepath.Join(root, name, "SKILL.md"),
			"---\nname: "+name+"\ndescription: Fixture skill\nallowed-tools: view\n---\n"+body+"\n")
	}
	roots := []string{filepath.Join(dir, "skills"), filepath.Join(project, ".agents", "skills"), filepath.Join(home, ".agents", "skills")}
	for i, root := range roots {
		writeSkill(root, "shared", []string{"team body", "project body", "user body"}[i])
	}
	writeSkill(roots[2], "production", "production body")
	writeSkill(filepath.Join(roots[0], "drafts"), "production", "draft body")
	writeSkill(filepath.Join(roots[0], "drafts"), "only-draft", "draft only body")
	first, err := InspectCapabilities(dir)
	if err != nil {
		t.Fatal(err)
	}
	find := func(id string) StaticCapability {
		t.Helper()
		index := slices.IndexFunc(first.Capabilities, func(c StaticCapability) bool { return c.Origin == "skill" && c.ID == id })
		if index < 0 {
			t.Fatalf("missing capability %q: %#v", id, first.Capabilities)
		}
		return first.Capabilities[index]
	}
	shared := find("shared")
	if shared.Availability != "available" || shared.Owner != "team" || len(shared.Sources) != 3 || !shared.Sources[0].Selected || shared.Sources[1].Selected || shared.Sources[2].Selected {
		t.Fatalf("wrong precedence: %#v", shared)
	}
	digest, err := skill.ContentDigest(skill.DiscoverSkills(roots[:1], false)[0])
	if err != nil || shared.ContentDigest == nil || *shared.ContentDigest != digest {
		t.Fatalf("selected digest: %#v %v", shared, err)
	}
	if c := find("production"); c.Owner != "user" || c.Availability != "available" {
		t.Fatalf("draft shadowed production: %#v", c)
	}
	for _, id := range []string{"draft:production", "draft:only-draft"} {
		if c := find(id); c.Availability != "draft" || c.Sources[0].Selected {
			t.Fatalf("draft became active: %#v", c)
		}
	}
	second, err := InspectCapabilities(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(second)
	if err != nil || string(a) != string(b) {
		t.Fatalf("unstable inventory: %v\n%s\n%s", err, a, b)
	}
	for _, path := range []string{project, home, dir} {
		if strings.Contains(string(a), path) {
			t.Fatalf("absolute path leaked: %s", a)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "workspace")); !os.IsNotExist(err) {
		t.Fatalf("inspection created workspace: %v", err)
	}
}

func TestCapabilityMCPUnknownAndRedacted(t *testing.T) {
	session := &TeamSession{MCPServers: map[string]mcp.MCPServerConfig{
		"remote": {
			URL:          "https://private.example/token-secret",
			Command:      []string{"must-not-execute", "--secret", "private-argv"},
			AllowedTools: []string{"lookup"},
			ToolPolicies: map[string]mcp.WorkerToolPolicy{
				"lookup": {
					SideEffect: "none", Filesystem: "none",
					InputSchema: map[string]any{"type": "object", "additionalProperties": false},
				},
			},
		},
	}}
	rows := capabilityMCP(session)
	if len(rows) != 2 {
		t.Fatalf("rows: %#v", rows)
	}
	for _, row := range rows {
		if row.Availability != "unknown" || row.ContentDigest != nil {
			t.Fatalf("offline declaration claimed live descriptor: %#v", row)
		}
	}
	if rows[0].EffectClass != "unknown" || rows[1].EffectClass != "none" {
		t.Fatalf("policy effect lost or guessed: %#v", rows)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private.example", "token-secret", "must-not-execute", "private-argv"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("configuration leaked: %s", data)
		}
	}
	config := session.MCPServers["remote"]
	policy := config.ToolPolicies["lookup"]
	policy.SideEffect = "external"
	config.ToolPolicies["lookup"] = policy
	session.MCPServers["remote"] = config
	rows = capabilityMCP(session)
	if rows[1].Availability != "unavailable" || rows[1].EffectClass != "unknown" {
		t.Fatalf("invalid policy accepted: %#v", rows)
	}
}

func TestInspectCapabilitiesMissingSkill(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir := lintTeamWithCoordinator(t, "skills: missing\n")
	inventory, err := InspectCapabilities(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(inventory.Capabilities, func(c StaticCapability) bool {
		return c.ID == "missing:missing" && c.Availability == "unavailable" && c.ContentDigest == nil
	}) {
		t.Fatalf("missing requirement hidden: %#v", inventory)
	}
}

func TestInspectCapabilitiesReusesDuplicateDiagnostics(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir := lintTeamWithCoordinator(t, "")
	writeLintTestFile(t, filepath.Join(dir, "duplicate.md"), lintAgent("worker", "worker"))
	inventory, err := InspectCapabilities(dir)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Complete || !slices.ContainsFunc(inventory.Diagnostics, func(d CapabilityDiagnostic) bool {
		return d.Code == FindingDuplicateAgent
	}) {
		t.Fatalf("duplicate diagnostic lost: %#v", inventory)
	}
}

func TestCapabilityEffectsRequireExplicitPolicy(t *testing.T) {
	for _, tc := range []struct {
		input SideEffectClass
		want  string
	}{
		{SideEffectNone, "none"},
		{SideEffectWorkspaceWrite, "workspace_write"},
		{SideEffectExternalWrite, "external"},
		{SideEffectInfraMutation, "external"},
		{SideEffectCredential, "external"},
		{SideEffectClass("read-only shell command"), "unknown"},
	} {
		if got := capabilityEffect(tc.input); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.input, got, tc.want)
		}
	}
}

func TestInspectCapabilitiesActionCatalogIdentity(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir := writeActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	inventory, err := InspectCapabilities(dir)
	if err != nil {
		t.Fatal(err)
	}
	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := session.ActionCatalog.Lookup("collect-debug-bundle")
	if !ok {
		t.Fatal("canonical action missing")
	}
	found := false
	for _, row := range inventory.Capabilities {
		if row.ID != "action:collect-debug-bundle" {
			continue
		}
		found = true
		if row.ContentDigest == nil || *row.ContentDigest != entry.Hash || row.EffectClass != "none" ||
			!slices.Equal(row.DeclaredGrants["discover"], entry.Discover) ||
			!slices.Equal(row.DeclaredGrants["propose"], entry.Propose) {
			t.Fatalf("canonical descriptor changed: %#v", row)
		}
	}
	if !found {
		t.Fatalf("action missing from inventory: %#v", inventory)
	}
}
