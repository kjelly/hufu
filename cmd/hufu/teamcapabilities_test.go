package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestTeamCapabilitiesCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	search := t.TempDir()
	prior := opts.agentTeamSearchPath
	opts.agentTeamSearchPath = search
	t.Cleanup(func() { opts.agentTeamSearchPath = prior })
	dir := filepath.Join(search, "inventory")
	if err := os.MkdirAll(filepath.Join(dir, "skills", "review"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"team.yaml":              "name: inventory\n",
		"worker.md":              "---\nname: worker\nrole: worker\ntools: view\n---\nInspect.\n",
		"skills/review/SKILL.md": "---\nname: review\ndescription: Fixture\n---\nRead files.\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(format string) (string, error) {
		t.Helper()
		var output bytes.Buffer
		command := newTeamCapabilitiesCommand()
		command.SetOut(&output)
		command.SetErr(&output)
		command.SetArgs([]string{"inventory", "--output", format})
		err := command.Execute()
		return output.String(), err
	}
	raw, err := run("json")
	if err != nil {
		t.Fatal(err)
	}
	var inventory team.CapabilityInventory
	if err := json.Unmarshal([]byte(raw), &inventory); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	if inventory.SchemaVersion != 1 || inventory.Team != "inventory" || len(inventory.Capabilities) != 1 {
		t.Fatalf("unexpected inventory: %#v", inventory)
	}
	text, err := run("text")
	if err != nil {
		t.Fatal(err)
	}
	entry := inventory.Capabilities[0]
	if !strings.Contains(text, entry.Name+": "+entry.Availability) || !strings.Contains(text, *entry.ContentDigest) {
		t.Fatalf("text differs from JSON: %s\n%s", text, raw)
	}
	if _, err := run("invalid"); err == nil {
		t.Fatal("invalid format accepted")
	}
}
