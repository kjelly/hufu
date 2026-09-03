package team

import (
	"os"
	"path/filepath"
	"testing"
)

// Phase 8 (spec.md Specification 05 / Specification 01 §9): the `advanced:`
// namespace is an alternative spelling for workflow/tasks/reliability/
// verification/retry, normalized into the same legacy top-level fields
// before anything else runs — so an equivalent legacy and advanced team
// must produce identical effective semantics, and defining the same field
// in both places must fail closed rather than silently pick one.

func writeAdvancedNamespaceFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const advancedNamespaceDeveloperMD = "---\nname: developer\nrole: worker\ntools: view,write,edit,grep,glob,ls,bash\nside_effect: workspace_write\nrecovery: retry\n---\nImplement the change.\n"

func TestAdvancedNamespace_WorkflowEquivalentToLegacyTopLevel(t *testing.T) {
	legacyYAML := `name: legacy-workflow
workflow:
  phases: [prepare, execute, verify]
policies:
  allow_phase_skip: true
delegation:
  bind-task-goal-contracts: true
tasks:
  - id: plan
    agent: developer
    when-goal-contains: plan
    phase: prepare
  - id: implement
    agent: developer
    when-goal-contains: implement
    phase: execute
  - id: review
    agent: developer
    when-goal-contains: review
    phase: verify
`
	legacyDir := t.TempDir()
	writeAdvancedNamespaceFile(t, legacyDir, "team.yaml", legacyYAML)
	writeAdvancedNamespaceFile(t, legacyDir, "developer.md", advancedNamespaceDeveloperMD)

	// A single "advanced:" YAML mapping key holds both workflow and tasks.
	advancedMerged := `name: advanced-workflow
policies:
  allow_phase_skip: true
delegation:
  bind-task-goal-contracts: true
advanced:
  workflow:
    phases: [prepare, execute, verify]
  tasks:
    - id: plan
      agent: developer
      when-goal-contains: plan
      phase: prepare
    - id: implement
      agent: developer
      when-goal-contains: implement
      phase: execute
    - id: review
      agent: developer
      when-goal-contains: review
      phase: verify
`
	advancedDir := t.TempDir()
	writeAdvancedNamespaceFile(t, advancedDir, "team.yaml", advancedMerged)
	writeAdvancedNamespaceFile(t, advancedDir, "developer.md", advancedNamespaceDeveloperMD)

	legacySession, err := LoadTeam(legacyDir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("load legacy team: %v", err)
	}
	advancedSession, err := LoadTeam(advancedDir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("load advanced team: %v", err)
	}

	if len(advancedSession.Config.Workflow.Phases) != len(legacySession.Config.Workflow.Phases) {
		t.Fatalf("Workflow.Phases = %v, want %v", advancedSession.Config.Workflow.Phases, legacySession.Config.Workflow.Phases)
	}
	for i, phase := range legacySession.Config.Workflow.Phases {
		if advancedSession.Config.Workflow.Phases[i] != phase {
			t.Errorf("Workflow.Phases[%d] = %q, want %q", i, advancedSession.Config.Workflow.Phases[i], phase)
		}
	}
	if len(advancedSession.ContractTasks) != len(legacySession.ContractTasks) {
		t.Fatalf("ContractTasks count = %d, want %d", len(advancedSession.ContractTasks), len(legacySession.ContractTasks))
	}
}

func TestAdvancedNamespace_WorkflowConflictFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeAdvancedNamespaceFile(t, dir, "team.yaml", `name: conflict
workflow:
  phases: [prepare, verify]
advanced:
  workflow:
    phases: [execute, verify]
`)
	writeAdvancedNamespaceFile(t, dir, "developer.md", advancedNamespaceDeveloperMD)

	_, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err == nil {
		t.Fatal("expected a conflict error for workflow defined both at top level and under advanced")
	}
}

func TestAdvancedNamespace_TasksConflictFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeAdvancedNamespaceFile(t, dir, "team.yaml", `name: conflict
tasks:
  - id: build
    agent: developer
    verify: "true"
advanced:
  tasks:
    - id: build2
      agent: developer
      verify: "true"
`)
	writeAdvancedNamespaceFile(t, dir, "developer.md", advancedNamespaceDeveloperMD)

	_, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err == nil {
		t.Fatal("expected a conflict error for tasks defined both at top level and under advanced")
	}
}

func TestAdvancedNamespace_ReliabilityConflictFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeAdvancedNamespaceFile(t, dir, "team.yaml", `name: conflict
reliability:
  rollout: shadow
advanced:
  reliability:
    rollout: active
`)
	writeAdvancedNamespaceFile(t, dir, "developer.md", advancedNamespaceDeveloperMD)

	_, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err == nil {
		t.Fatal("expected a conflict error for reliability defined both at top level and under advanced")
	}
}

func TestAdvancedNamespace_VerificationConflictFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeAdvancedNamespaceFile(t, dir, "team.yaml", `name: conflict
workflow:
  phases: [prepare, execute, verify]
delegation:
  bind-task-goal-contracts: true
verification:
  required: true
advanced:
  verification:
    required: true
tasks:
  - id: plan
    agent: developer
    when-goal-contains: plan
    phase: prepare
  - id: implement
    agent: developer
    when-goal-contains: implement
    phase: execute
  - id: review
    agent: developer
    when-goal-contains: review
    phase: verify
`)
	writeAdvancedNamespaceFile(t, dir, "developer.md", advancedNamespaceDeveloperMD)

	_, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err == nil {
		t.Fatal("expected a conflict error for verification defined both at top level and under advanced")
	}
}

func TestAdvancedNamespace_RetryConflictFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeAdvancedNamespaceFile(t, dir, "team.yaml", `name: conflict
workflow:
  phases: [prepare, execute, verify]
delegation:
  bind-task-goal-contracts: true
retry:
  transient:
    max_attempts: 1
advanced:
  retry:
    transient:
      max_attempts: 2
tasks:
  - id: plan
    agent: developer
    when-goal-contains: plan
    phase: prepare
  - id: implement
    agent: developer
    when-goal-contains: implement
    phase: execute
  - id: review
    agent: developer
    when-goal-contains: review
    phase: verify
`)
	writeAdvancedNamespaceFile(t, dir, "developer.md", advancedNamespaceDeveloperMD)

	_, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err == nil {
		t.Fatal("expected a conflict error for retry defined both at top level and under advanced")
	}
}

func TestAdvancedNamespace_AdvancedOnlyTasksLoadWithoutTopLevel(t *testing.T) {
	dir := t.TempDir()
	writeAdvancedNamespaceFile(t, dir, "team.yaml", `name: advanced-only
advanced:
  tasks:
    - id: build
      agent: developer
      verify: "true"
`)
	writeAdvancedNamespaceFile(t, dir, "developer.md", advancedNamespaceDeveloperMD)

	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(session.ContractTasks) != 1 || session.ContractTasks[0].ID != "build" {
		t.Fatalf("ContractTasks = %+v, want one task with id=build", session.ContractTasks)
	}
}

func TestAdvancedNamespace_LegacyTeamsWithoutAdvancedStillWork(t *testing.T) {
	dir := t.TempDir()
	writeAdvancedNamespaceFile(t, dir, "team.yaml", "name: legacy-only\nmax-rounds: 5\n")
	writeAdvancedNamespaceFile(t, dir, "developer.md", advancedNamespaceDeveloperMD)

	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if session.Config.MaxRounds != 5 {
		t.Fatalf("MaxRounds = %d, want 5", session.Config.MaxRounds)
	}
}
