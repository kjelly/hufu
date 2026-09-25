package main

import (
	"bytes"
	"strings"
	"testing"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

func TestWriteCatalogActionReport(t *testing.T) {
	todos := []*team.TodoItem{
		{ID: "1", Agent: "worker", Status: team.TaskDone},
		{ID: "2", Agent: "runtime-engineer", Status: team.TaskDone, Action: &team.Action{Capability: "diagnostics", Type: "collect", Payload: `{"service":"secret-name"}`},
			CatalogAction: &team.CatalogActionBinding{ActionID: "collect", ArgumentsHash: "sha256:abc", ProposalIDs: []string{"tap_1", "tap_2"}}},
	}
	var b strings.Builder
	writeCatalogActionReport(&b, todos)
	report := b.String()
	if !strings.Contains(report, "### Catalog Actions") || !strings.Contains(report, "| 2 | collect | done | sha256:abc | tap_1, tap_2 |") {
		t.Fatalf("catalog report = %q", report)
	}
	if strings.Contains(report, "secret-name") {
		t.Fatalf("catalog report prints arguments: %q", report)
	}
	var empty strings.Builder
	writeCatalogActionReport(&empty, todos[:1])
	if empty.Len() != 0 {
		t.Fatalf("report without catalog actions = %q", empty.String())
	}
	if got := reportProviderIdentity(todos[1]); got != "action:diagnostics" {
		t.Fatalf("action task provider = %q, want action:diagnostics", got)
	}
	if got := reportProviderIdentity(&team.TodoItem{SubagentProvider: "codex"}); got != "codex" {
		t.Fatalf("worker task provider = %q", got)
	}
}

func TestCatalogStepLabel(t *testing.T) {
	label := catalogStepLabel(&team.CatalogActionBinding{ActionID: "collect", ArgumentsHash: "sha256:0123456789abcdef0123"})
	if label != "catalog action collect args=0123456789ab" {
		t.Fatalf("step label = %q", label)
	}
}

func TestRenderInspectTaskCatalogAction(t *testing.T) {
	var output bytes.Buffer
	envelope := &inspectpkg.Envelope{
		Kind: inspectpkg.KindTask,
		Data: inspectpkg.TaskData{
			RunID: "run-1", TaskID: "task-1",
			ExecutionTopology: []string{}, Attempts: []inspectpkg.AttemptData{}, ArtifactRefs: []string{}, ContextRefs: []string{}, MemoryRefs: []string{},
			CatalogAction: &inspectpkg.CatalogActionData{ActionID: "collect", EntryHash: "sha256:e", ArgumentsHash: "sha256:a", InvocationID: "tai_x", ProposalIDs: []string{"tap_1"}},
		},
	}
	if err := renderInspectText(&output, envelope); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Catalog action: collect  entry=sha256:e  args=sha256:a", "Invocation: tai_x  proposals: tap_1"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("inspect text lacks %q:\n%s", want, output.String())
		}
	}
}
