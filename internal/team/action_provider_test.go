package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/golangruntime"
)

func TestLoadTeamRegistersConfiguredActionProviderInSessionRegistry(t *testing.T) {
	dir := t.TempDir()
	teamYAML := `name: adapter-team
workflow:
  phases: [prepare, audit, execute, verify]
capabilities:
  required: [structured-actions]
delegation:
  bind-task-goal-contracts: true
action-providers:
  structured-actions:
    command: [/bin/sh, -c, 'cat >/dev/null; printf "{\"status\":\"ok\"}"']
tasks:
  - id: prepare
    agent: preparer
    when-goal-contains: prepare
    phase: prepare
  - id: audit
    agent: auditor
    when-goal-contains: audit
    phase: audit
  - id: execute
    agent: executor
    when-goal-contains: execute
    phase: execute
    action:
      capability: structured-actions
      type: apply
      payload: '{}'
  - id: verify
    agent: verifier
    when-goal-contains: verify
    phase: verify
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(teamYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"preparer", "auditor", "executor", "verifier"} {
		content := "---\nname: " + name + "\nrole: worker\n---\nworker\n"
		if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := NewProviderRegistry()
	session, err := LoadTeam(dir, nil, nil, base)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if base.Has("structured-actions") {
		t.Fatal("team-specific provider mutated base registry")
	}
	provider, ok := session.ProviderRegistry.Get(" structured-actions ")
	if !ok {
		t.Fatal("configured action provider was not registered in session")
	}
	result, err := provider.Execute(context.Background(), Action{Capability: "structured-actions", Type: "apply", Payload: `{}`})
	if err != nil {
		t.Fatalf("configured provider Execute: %v", err)
	}
	resultMap, ok := result.(map[string]interface{})
	if !ok || resultMap["status"] != "ok" {
		t.Fatalf("configured provider result = %#v", result)
	}
}

func TestRegisterConfiguredGolangActionProviderValidatesTrustedStaticContract(t *testing.T) {
	teamDir := t.TempDir()
	source := filepath.Join(teamDir, "action")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	program := `package main
import ("context"; "io")
func Run(_ context.Context, _ io.Reader, _ io.Writer) error { return nil }
`
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte(program), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewProviderRegistry()
	err := registerConfiguredActionProviders(registry, map[string]agent.ActionProviderConfig{
		"demo": {Runtime: "golang", Source: "./action", Mode: golangruntime.TrustedStaticMode},
	}, teamDir)
	if err != nil {
		t.Fatal(err)
	}
	if name := registry.ProviderName("demo"); !strings.HasPrefix(name, "golang:sha256:") {
		t.Fatalf("provider name = %q", name)
	}

	for name, config := range map[string]agent.ActionProviderConfig{
		"missing mode": {Runtime: "golang", Source: "./action"},
		"mixed command": {
			Runtime: "golang", Source: "./action", Mode: golangruntime.TrustedStaticMode, Command: []string{"go"},
		},
		"unsupported runtime": {Runtime: "python", Source: "./action"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := registerConfiguredActionProviders(NewProviderRegistry(), map[string]agent.ActionProviderConfig{"demo": config}, teamDir); err == nil {
				t.Fatal("configuration was accepted")
			}
		})
	}
}

func TestGolangActionProviderUsesWireContractAndActionEnvironment(t *testing.T) {
	provider := &golangActionProvider{
		capability: "demo",
		program:    golangruntime.Program{Source: "/team/action", Digest: "sha256:fixture"},
		execute: func(_ context.Context, _ golangruntime.Program, input []byte, env []string, _, _ int) (golangruntime.Result, error) {
			var action Action
			if err := json.Unmarshal(input, &action); err != nil {
				t.Fatal(err)
			}
			if action.Type != "apply" || !slices.Contains(env, "HUFU_REPOSITORY=/repo") {
				t.Fatalf("action=%#v env=%#v", action, env)
			}
			return golangruntime.Result{Stdout: []byte(`{"status":"ok"}`)}, nil
		},
	}
	ctx := WithActionEnvironment(t.Context(), ActionEnvironment{Repository: "/repo"})
	result, err := provider.Execute(ctx, Action{Capability: "demo", Type: "apply", Payload: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["status"] != "ok" {
		t.Fatalf("result = %#v", result)
	}
}

func TestCommandActionProviderHonorsContextDeadline(t *testing.T) {
	provider := &commandActionProvider{
		capability: "structured-actions",
		command:    []string{"/bin/sh", "-c", "cat >/dev/null; sleep 1"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := provider.Execute(ctx, Action{Capability: "structured-actions", Type: "apply"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute error = %v, want context deadline exceeded", err)
	}
}

func TestCommandActionProviderInjectsEnvironment(t *testing.T) {
	provider := &commandActionProvider{
		capability: "structured-actions",
		command:    []string{"/bin/sh", "-c", `cat >/dev/null; printf '{"workspace":"%s","repo":"%s","team":"%s"}' "$HUFU_WORKSPACE" "$HUFU_REPOSITORY" "$HUFU_TEAM"`},
	}
	ctx := WithActionEnvironment(context.Background(), ActionEnvironment{
		Workspace:  "/test/workspace/my-team",
		Repository: "/test/repo",
		TeamName:   "my-team",
	})
	result, err := provider.Execute(ctx, Action{Capability: "structured-actions", Type: "apply"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	resMap, ok := result.(map[string]interface{})
	if !ok {
		t.Fatalf("result is not map: %#v", result)
	}
	if resMap["workspace"] != "/test/workspace/my-team" || resMap["repo"] != "/test/repo" || resMap["team"] != "my-team" {
		t.Fatalf("injected environment mismatch: %#v", resMap)
	}
}

func TestCommandActionProviderRunsStrictResolverEnvelope(t *testing.T) {
	provider := &commandActionProvider{
		capability: "resolve-demo",
		command:    []string{"/bin/sh", "-c", `cat >/dev/null; printf '{"status":"matched","value":3,"evidence":[{"source":"prompt","start":7,"end":8,"kind":"count"}],"resolver_version":"1"}'`},
	}
	response, err := provider.ResolveRunInput(t.Context(), RunInputResolverRequest{
		Type: "resolve_run_input", InputName: "count", Prompt: "last 3", SchemaHash: "sha256:fixture", ResolverID: "count-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "matched" || string(response.Value) != "3" || response.ResolverVersion != "1" {
		t.Fatalf("response = %#v", response)
	}

	provider.command = []string{"/bin/sh", "-c", `cat >/dev/null; printf '{"status":"matched","value":3,"resolver_version":"1","unknown":true}'`}
	if _, err := provider.ResolveRunInput(t.Context(), RunInputResolverRequest{}); err == nil {
		t.Fatal("resolver provider accepted an unknown response field")
	}
}
