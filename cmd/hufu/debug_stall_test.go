package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestDebugRunIncludesOnlyItsReferencedStallDumps(t *testing.T) {
	workspace, output := t.TempDir(), t.TempDir()
	stall := filepath.Join(workspace, "logs", "stall")
	if err := os.MkdirAll(stall, 0700); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"selected.txt":     "blocked in prompt.Run token=secret-stall-token",
		"other.txt":        "other-run-private-content",
		"unreferenced.txt": "unreferenced-private-content",
	}
	for name, content := range paths {
		if err := os.WriteFile(filepath.Join(stall, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(output, "external.txt")
	if err := os.WriteFile(outside, []byte("external-private-content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stall, "link.txt")); err != nil {
		t.Fatal(err)
	}
	makeEvent := func(run, path string) team.RunEvent {
		payload, err := json.Marshal(map[string]string{"dump_path": path})
		if err != nil {
			t.Fatal(err)
		}
		return team.RunEvent{RunID: run, Type: "run_stall_detected", Payload: payload}
	}
	writeEventJournal(t, workspace,
		team.RunEvent{RunID: "run-stall", Type: "run_started", Payload: []byte(`{}`)},
		makeEvent("run-stall", filepath.Join(stall, "selected.txt")),
		makeEvent("run-stall", filepath.Join(stall, "selected.txt")),
		makeEvent("run-other", filepath.Join(stall, "other.txt")),
		makeEvent("run-stall", outside),
		makeEvent("run-stall", filepath.Join(stall, "link.txt")),
		makeEvent("run-stall", filepath.Join(stall, "missing.txt")),
	)
	t.Chdir(output)
	relativeWorkspace, err := filepath.Rel(output, workspace)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetArgs([]string{"debug", "run-stall", "--workspace", relativeWorkspace})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	bundle := readDebugBundle(t, filepath.Join(output, "hufu-debug-run-stall.tar.gz"))
	selected := string(bundle["logs/stall/selected.txt"])
	if !strings.Contains(selected, "prompt.Run") || strings.Contains(selected, "secret-stall-token") {
		t.Fatalf("missing or unredacted dump: %s", selected)
	}
	for name, data := range bundle {
		if name == "logs/stall/other.txt" || name == "logs/stall/unreferenced.txt" || strings.Contains(string(data), "external-private-content") {
			t.Fatalf("export escaped run scope: %s", name)
		}
	}
	manifest := string(bundle["bundle-manifest.json"])
	for _, expected := range []string{"logs/stall/selected.txt", "outside logs/stall", "symlink", "logs/stall/missing.txt"} {
		if !strings.Contains(manifest, expected) {
			t.Fatalf("manifest omitted %s: %s", expected, manifest)
		}
	}
}
