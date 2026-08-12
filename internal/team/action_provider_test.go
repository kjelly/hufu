package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
