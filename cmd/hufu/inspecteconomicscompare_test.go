package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func TestInspectEconomicsComparisonCLI(t *testing.T) {
	manifest := inspectpkg.EconomicsComparisonManifest{SchemaVersion: 1, WorkloadID: "declared-cli-workload"}
	for _, group := range []string{"a", "b"} {
		workspace, runID, _ := buildInspectCommandFixture(t)
		view, err := inspectpkg.InspectEconomics(t.Context(), inspectpkg.InspectQuery{Workspace: workspace, RunID: runID})
		if err != nil {
			t.Fatal(err)
		}
		manifest.Runs = append(manifest.Runs, inspectpkg.EconomicsRunSelection{Group: group, Workspace: workspace, RunID: runID, BranchID: view.Query.BranchID})
	}
	path := filepath.Join(t.TempDir(), "comparison.json")
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(format string) string {
		t.Helper()
		command := newInspectCommand()
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"economics-compare", "--manifest", path, "--format", format})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private worker output", "sk-fixture-secret", "\x1b"} {
			if strings.Contains(out.String(), secret) {
				t.Fatalf("output leaked %q", secret)
			}
		}
		return out.String()
	}
	var envelope struct {
		Kind inspectpkg.Kind                    `json:"kind"`
		Data inspectpkg.EconomicsComparisonData `json:"data"`
	}
	if err := json.Unmarshal([]byte(run("json")), &envelope); err != nil {
		t.Fatal(err)
	}
	text := run("text")
	if envelope.Kind != inspectpkg.KindEconomicsCompare || envelope.Data.WorkloadSource != "operator_declared" {
		t.Fatalf("invalid comparison: %+v", envelope)
	}
	for _, g := range envelope.Data.Groups {
		for _, expected := range []string{"Group: " + g.Group, fmt.Sprintf("Runs: %d total, %d terminal, %d nonterminal, %d accepted", g.RunCount, g.TerminalRunCount, g.NonterminalRunCount, g.AcceptedRunCount), "Total cost (USD micros): unavailable", "Cost per accepted run (USD micros): unavailable"} {
			if !strings.Contains(text, expected) {
				t.Fatalf("text missing %q: %s", expected, text)
			}
		}
	}
	for _, r := range envelope.Data.Runs {
		for _, expected := range []string{r.Scope.RunID, r.Scope.BranchID, r.Economics.SourceHead.EventID, r.Economics.SourceHead.EventHash} {
			if !strings.Contains(text, expected) {
				t.Fatalf("text lost source %q", expected)
			}
		}
	}
	for _, args := range [][]string{{}, {"--workspace", ""}, {"-w", ""}, {"--branch", "main"}, {"--session", "session"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			command := newInspectCommand()
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			base := []string{"economics-compare"}
			if len(args) > 0 {
				base = append(base, "--manifest", path)
			}
			command.SetArgs(append(base, args...))
			err := command.Execute()
			exit, ok := errors.AsType[*inspectExitError](err)
			if !ok || exit.code != inspectpkg.ExitUsage {
				t.Fatalf("must reject scope/missing manifest with usage: %v", err)
			}
		})
	}
}
