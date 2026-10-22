package main

import (
	"bytes"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	internalteam "github.com/kjelly/hufu/internal/team"
)

func TestTeamInlineProcessJourney(t *testing.T) {
	binary := buildProcessContractBinary(t)
	root := t.TempDir()
	dir := filepath.Join(root, ".agent-teams/dev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "preset: coding-reviewed\nvars: {prompt: Inspect design}\nagents:\n  architect:\n    preset: readonly\n    instructions: '{@ .prompt @}'\n"
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "security.md"), []byte("---\nname: auditor\npreset: review\n---\nReview security.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		code, out, stderr := runCompositionProcess(t, binary, root, args...)
		if code != 0 {
			t.Fatalf("%v code=%d out=%s stderr=%s", args, code, out, stderr)
		}
		return out
	}
	run("team", "validate", dir)
	var lint teamLintJSONDocument
	if data := run("team", "lint", dir, "--format", "json"); json.Unmarshal(data, &lint) != nil || !lint.Complete {
		t.Fatalf("lint=%s", data)
	}
	if data := run("team", "explain", dir, "--format", "json"); !bytes.Contains(data, []byte("inline:team.yml:agents.architect")) {
		t.Fatalf("inline provenance missing: %s", data)
	}
	if data := run("team", "show", dir); !bytes.Contains(data, []byte("team.yml")) || !bytes.Contains(data, []byte("instructions:")) || bytes.Contains(data, []byte("architect.md")) {
		t.Fatalf("show invented Markdown or lost authoring: %s", data)
	}
	for _, output := range []string{"text", "table", "json"} {
		data := run("list", "dev", "--output", output)
		if output == "json" {
			var teams []listedTeam
			if err := json.Unmarshal(data, &teams); err != nil || len(teams) != 1 || len(teams[0].Agents) != 4 {
				t.Fatalf("list=%s err=%v", data, err)
			}
			var names []string
			for _, def := range teams[0].Agents {
				names = append(names, def.Name)
			}
			if !slices.Equal(names, []string{"architect", "auditor", "developer", "reviewer"}) {
				t.Fatalf("roster=%v", names)
			}
		} else if !bytes.Contains(data, []byte("@architect")) || !bytes.Contains(data, []byte("@auditor")) {
			t.Fatalf("list=%s", data)
		}
	}
	migrated := run("team", "migrate", dir, "--dry-run")
	if !bytes.Contains(migrated, []byte("{@ .prompt @}")) || !bytes.Contains(migrated, []byte("agents:")) {
		t.Fatalf("migration lost raw inline source: %s", migrated)
	}
	for _, args := range [][]string{
		{"team", "add", "dev", "architect", "--preset", "review", "--force"},
		{"team", "add", "dev", "ARCHITECT", "--preset", "review"},
	} {
		if code, _, _ := runCompositionProcess(t, binary, root, args...); code == 0 {
			t.Fatalf("manifest identity replaced: %v", args)
		}
	}
	original, err := os.ReadFile(filepath.Join(dir, "team.yml"))
	entries, readErr := os.ReadDir(dir)
	if err != nil || readErr != nil || string(original) != manifest || len(entries) != 2 {
		t.Fatalf("inspection or collision mutated files: entries=%v err=%v %v", entries, err, readErr)
	}
	for _, path := range []string{filepath.Join(root, "workspace"), filepath.Join(dir, "workspace")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("static inspection created runtime workspace: %s", path)
		}
	}
	// A new, distinct Markdown member remains supported.
	run("team", "add", "dev", "writer", "--preset", "writer")
	// Reload the real CLI's migrated versioned output, including authored view.
	if err := os.Remove(filepath.Join(dir, "team.yml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), migrated, 0o644); err != nil {
		t.Fatal(err)
	}
	run("team", "validate", dir)
	if data := run("team", "show", dir); !bytes.Contains(data, []byte("spec:")) || !bytes.Contains(data, []byte("architect:")) {
		t.Fatalf("versioned authored shape lost: %s", data)
	}
}

func TestTeamInlineProcessErrors(t *testing.T) {
	binary := buildProcessContractBinary(t)
	root := t.TempDir()
	dir := filepath.Join(root, ".agent-teams/broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		manifest, code string
		exit           int
	}{
		{"agents: {worker: {preset: readonly, role: coordinator}}\n", "load_error", 2},
		{"agents: {worker: {preset: readonly, no-net: 'true'}}\n", "load_error", 2},
		{"agents: {worker: unknown}\n", "inline_agent_invalid", 1},
		{"agents: {coordinator: readonly}\n", "inline_agent_invalid", 1},
		{"agents: {worker: {preset: readonly, tools: {denied: [view, grep, glob, ls]}}}\n", "inline_agent_tools_empty", 1},
	} {
		if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(test.manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, stderr := runCompositionProcess(t, binary, root, "team", "lint", dir, "--format", "json")
		var result teamLintJSONDocument
		if code != test.exit || json.Unmarshal(out, &result) != nil || result.Complete || !bytes.Contains(out, []byte(test.code)) {
			t.Fatalf("lint code=%d out=%s stderr=%s", code, out, stderr)
		}
		if code, _, _ := runCompositionProcess(t, binary, root, "team", "validate", dir); code == 0 {
			t.Fatal("invalid inline worker validated")
		}
		if code, out, stderr := runCompositionProcess(t, binary, root, "list", "--output", "json"); code == 0 || len(out) != 0 || len(stderr) == 0 {
			t.Fatalf("invalid team produced partial JSON: %d %s %s", code, out, stderr)
		}
		if test.exit == 1 {
			code, out, _ := runCompositionProcess(t, binary, root, "team", "lint", dir, "--format", "json", "--ignore", test.code)
			if code != 0 || !bytes.Contains(out, []byte(`"ignored": true`)) {
				t.Fatalf("ignore contract changed: %d %s", code, out)
			}
		}
	}
}

func TestTeamInlineModelOverrides(t *testing.T) {
	dir := t.TempDir()
	manifest := "worker-model: ollama/team\nagents:\n  coder: {preset: coding, model: ollama/own}\n  reviewer: review\n"
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := internalteam.CompileTeam(dir, nil, nil, internalteam.DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	session := spec.RuntimeSession()
	if session.Agents["coder"].Generation.Model != "ollama/own" {
		t.Fatal("team replaced inline model")
	}
	overrides := ModelCLIOverrides{Model: "ollama/cli", WorkerModels: []WorkerModelOverride{{Agent: "CODER", Target: "ollama/pinned"}}}
	applyCLIModelOverrides(&session.Config, overrides)
	if err := applyCLIGenerationOverridesToAgents(session, overrides); err != nil {
		t.Fatal(err)
	}
	if session.Agents["coder"].Generation.Model != "ollama/pinned" || session.Agents["reviewer"].Generation.Model != "ollama/cli" {
		t.Fatal("CLI worker override precedence changed")
	}
	if err := applyCLIGenerationOverridesToAgents(session, ModelCLIOverrides{WorkerModels: []WorkerModelOverride{{Agent: "absent", Target: "ollama/x"}}}); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("unknown override accepted: %v", err)
	}
}

func TestTeamInlineProfileOverrides(t *testing.T) {
	for _, test := range []struct {
		args            []string
		coder, reviewer string
	}{
		{nil, "ollama/profile-worker", "ollama/profile"},
		{[]string{"--model", "ollama/cli"}, "ollama/cli", "ollama/cli"},
		{[]string{"--worker-model", "coder=ollama/cli-worker"}, "ollama/cli-worker", "ollama/profile"},
	} {
		t.Run(strings.Join(test.args, "/"), func(t *testing.T) {
			dir := t.TempDir()
			writeHufuYAML(t, dir, "profiles:\n  p:\n    model: ollama/profile\n    worker-model: coder=ollama/profile-worker\n")
			defer chdir(t, dir)()
			previous := opts
			t.Cleanup(func() { opts = previous })
			opts = runOptions{profileName: "p"}
			cmd := newWorkerModelProfileTestCmd()
			if err := cmd.ParseFlags(test.args); err != nil {
				t.Fatal(err)
			}
			if err := applyProfile(cmd); err != nil {
				t.Fatal(err)
			}
			overrides, err := currentModelOverrides()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("agents: {coder: {preset: coding, model: ollama/own}, reviewer: review}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			spec, err := internalteam.CompileTeam(dir, nil, nil, internalteam.DefaultProviderRegistry)
			if err != nil {
				t.Fatal(err)
			}
			session := spec.RuntimeSession()
			applyCLIModelOverrides(&session.Config, overrides)
			if err := applyCLIGenerationOverridesToAgents(session, overrides); err != nil {
				t.Fatal(err)
			}
			assertAgentModels(t, session, map[string]string{"coder": test.coder, "reviewer": test.reviewer})
		})
	}
}

func TestTeamInlineStaticCommandsNeverExecute(t *testing.T) {
	binary := buildProcessContractBinary(t)
	root := t.TempDir()
	dir := filepath.Join(root, ".agent-teams/offline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "unexpected-execution")
	script := filepath.Join(root, "fixture.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "agents: {worker: readonly}\nacceptance: '" + script + "'\nmcp-servers:\n  fixture:\n    command: ['" + script + "']\ncontracts:\n  - id: inspect\n    agent: worker\n    goal: inspect\n    verify: '" + script + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"team", "validate", dir}, {"team", "lint", dir, "--format", "json"},
		{"team", "explain", dir, "--format", "json"}, {"team", "show", dir},
		{"team", "migrate", dir, "--dry-run"}, {"list", "offline", "--output", "json"},
	} {
		code, out, stderr := runCompositionProcess(t, binary, root, args...)
		// Objective-check lint can reject the deliberately non-asserting fixture.
		if code != 0 && !(args[1] == "lint" && code == 1) {
			t.Fatalf("%v code=%d out=%s stderr=%s", args, code, out, stderr)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("%v executed an MCP, verifier or acceptance command", args)
		}
	}
}
