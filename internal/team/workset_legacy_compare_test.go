package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// worksetComparison reports the ordered keys and substituted goals a legacy
// workspace-relative TSV fan-out and an artifact-backed fan-out produce over
// (nominally) the same item set.
//
// It only covers the behavior dimensions the legacy path actually has:
// expandFanOutTask (fan_out.go) never creates a WorksetBinding or
// WorksetExpansionReceipt, so duplicate-key rejection, stale-source
// rejection, and workset-scoped resume/retry/cancel have no legacy-side
// counterpart to compare against — see
// docs/tmp/now/05-workset-legacy-tsv-removal.md §5. General task-level
// retry/resume/cancel is exercised independently of fan-out kind by
// wp02_resume_test.go and wp08_retry_disposition_test.go.
type worksetComparison struct {
	LegacyKeys    []string
	ArtifactKeys  []string
	LegacyGoals   []string
	ArtifactGoals []string
}

// orderingEqual reports whether both paths produced the same items in the
// same position.
func (cmp worksetComparison) orderingEqual() bool {
	return reflect.DeepEqual(cmp.LegacyKeys, cmp.ArtifactKeys)
}

// keysEqual reports whether both paths produced the same set of keys,
// independent of order.
func (cmp worksetComparison) keysEqual() bool {
	legacy := append([]string(nil), cmp.LegacyKeys...)
	artifact := append([]string(nil), cmp.ArtifactKeys...)
	sort.Strings(legacy)
	sort.Strings(artifact)
	return reflect.DeepEqual(legacy, artifact)
}

// templateSubstitutionEqual reports whether both paths substituted
// goal_template identically, position by position.
func (cmp worksetComparison) templateSubstitutionEqual() bool {
	return reflect.DeepEqual(cmp.LegacyGoals, cmp.ArtifactGoals)
}

// childCountEqual reports whether both paths produced the same number of
// child tasks.
func (cmp worksetComparison) childCountEqual() bool {
	return len(cmp.LegacyGoals) == len(cmp.ArtifactGoals)
}

// compareLegacyAndArtifactWorkset expands a legacy TSV fan-out task and an
// artifact-backed (structured) fan-out task in the same workspace and
// collects their resulting keys and goals for comparison. legacyTask and
// artifactTask must describe the same logical items for the comparison to be
// meaningful; the caller is responsible for writing both source files.
func compareLegacyAndArtifactWorkset(t *testing.T, workspace string, legacyTask, artifactTask TaskDef) worksetComparison {
	t.Helper()
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}

	legacy, err := c.expandFanOutTasks([]TaskDef{legacyTask})
	if err != nil {
		t.Fatalf("legacy fan-out expansion: %v", err)
	}
	artifactBacked, err := c.expandFanOutTasks([]TaskDef{artifactTask})
	if err != nil {
		t.Fatalf("artifact-backed fan-out expansion: %v", err)
	}

	header, rows, err := readFanOutTSV(filepath.Join(workspace, legacyTask.FanOut.Source))
	if err != nil {
		t.Fatalf("read legacy TSV: %v", err)
	}
	if len(header) == 0 {
		t.Fatal("legacy TSV has no header")
	}
	legacyKeys := make([]string, len(rows))
	for i, row := range rows {
		legacyKeys[i] = row[0]
	}

	artifactKeys := make([]string, len(artifactBacked))
	for i, task := range artifactBacked {
		if task.WorksetBinding == nil {
			t.Fatalf("artifact-backed task %d has no workset binding", i)
		}
		artifactKeys[i] = task.WorksetBinding.ItemKey
	}

	legacyGoals := make([]string, len(legacy))
	for i, task := range legacy {
		legacyGoals[i] = task.Goal
	}
	artifactGoals := make([]string, len(artifactBacked))
	for i, task := range artifactBacked {
		artifactGoals[i] = task.Goal
	}

	return worksetComparison{
		LegacyKeys:    legacyKeys,
		ArtifactKeys:  artifactKeys,
		LegacyGoals:   legacyGoals,
		ArtifactGoals: artifactGoals,
	}
}

func writeWorksetTSVFixture(t *testing.T, workspace, relPath string, header []string, rows [][]string) {
	t.Helper()
	abs := filepath.Join(workspace, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Join(header, "\t") + "\n"
	for _, row := range rows {
		content += strings.Join(row, "\t") + "\n"
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeWorksetManifestFixture(t *testing.T, workspace, relPath string, items []WorksetItem) {
	t.Helper()
	abs := filepath.Join(workspace, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCompareLegacyAndArtifactWorksetOrdering proves both paths preserve
// source row order when the manifest lists items in the same order as the
// TSV.
func TestCompareLegacyAndArtifactWorksetOrdering(t *testing.T) {
	workspace := t.TempDir()
	writeWorksetTSVFixture(t, workspace, "inputs/items.tsv", []string{"item"}, [][]string{{"alpha"}, {"beta"}, {"gamma"}})
	writeWorksetManifestFixture(t, workspace, "inputs/items.json", []WorksetItem{
		{Key: "alpha", Bindings: map[string]string{"item": "alpha"}},
		{Key: "beta", Bindings: map[string]string{"item": "beta"}},
		{Key: "gamma", Bindings: map[string]string{"item": "gamma"}},
	})

	cmp := compareLegacyAndArtifactWorkset(t, workspace,
		TaskDef{ID: "legacy", FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"}},
		TaskDef{ID: "artifact", FanOut: &FanOutSpec{Source: "inputs/items.json", GoalTemplate: "process {item}"}},
	)
	if !cmp.orderingEqual() {
		t.Fatalf("ordering not equal: legacy=%v artifact=%v", cmp.LegacyKeys, cmp.ArtifactKeys)
	}
	if !cmp.keysEqual() {
		t.Fatalf("keys not equal: legacy=%v artifact=%v", cmp.LegacyKeys, cmp.ArtifactKeys)
	}
}

// TestCompareLegacyAndArtifactWorksetKeys proves keysEqual is a set
// comparison independent of order — and, by asserting orderingEqual is
// false here, that orderingEqual actually detects a position difference
// instead of being vacuously true.
func TestCompareLegacyAndArtifactWorksetKeys(t *testing.T) {
	workspace := t.TempDir()
	writeWorksetTSVFixture(t, workspace, "inputs/items.tsv", []string{"item"}, [][]string{{"alpha"}, {"beta"}, {"gamma"}})
	writeWorksetManifestFixture(t, workspace, "inputs/items.json", []WorksetItem{
		{Key: "gamma", Bindings: map[string]string{"item": "gamma"}},
		{Key: "alpha", Bindings: map[string]string{"item": "alpha"}},
		{Key: "beta", Bindings: map[string]string{"item": "beta"}},
	})

	cmp := compareLegacyAndArtifactWorkset(t, workspace,
		TaskDef{ID: "legacy", FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"}},
		TaskDef{ID: "artifact", FanOut: &FanOutSpec{Source: "inputs/items.json", GoalTemplate: "process {item}"}},
	)
	if !cmp.keysEqual() {
		t.Fatalf("keys not equal: legacy=%v artifact=%v", cmp.LegacyKeys, cmp.ArtifactKeys)
	}
	if cmp.orderingEqual() {
		t.Fatal("ordering unexpectedly equal despite a reordered manifest; orderingEqual looks vacuous")
	}
}

// TestCompareLegacyAndArtifactWorksetTemplateSubstitution proves
// goal_template substitution is identical across a multi-binding template,
// not just a single-column one.
func TestCompareLegacyAndArtifactWorksetTemplateSubstitution(t *testing.T) {
	workspace := t.TempDir()
	writeWorksetTSVFixture(t, workspace, "inputs/items.tsv", []string{"item", "lens"}, [][]string{{"alpha", "security"}, {"beta", "performance"}})
	writeWorksetManifestFixture(t, workspace, "inputs/items.json", []WorksetItem{
		{Key: "alpha", Bindings: map[string]string{"item": "alpha", "lens": "security"}},
		{Key: "beta", Bindings: map[string]string{"item": "beta", "lens": "performance"}},
	})

	cmp := compareLegacyAndArtifactWorkset(t, workspace,
		TaskDef{ID: "legacy", FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "Review {item} with the {lens} lens"}},
		TaskDef{ID: "artifact", FanOut: &FanOutSpec{Source: "inputs/items.json", GoalTemplate: "Review {item} with the {lens} lens"}},
	)
	if !cmp.templateSubstitutionEqual() {
		t.Fatalf("template substitution not equal: legacy=%v artifact=%v", cmp.LegacyGoals, cmp.ArtifactGoals)
	}
	want := []string{"Review alpha with the security lens", "Review beta with the performance lens"}
	if !reflect.DeepEqual(cmp.LegacyGoals, want) {
		t.Fatalf("legacy goals = %v, want %v", cmp.LegacyGoals, want)
	}
}

// TestCompareLegacyAndArtifactWorksetChildCount proves childCountEqual
// detects both the equal case and a real mismatch, so the check isn't
// vacuously true.
func TestCompareLegacyAndArtifactWorksetChildCount(t *testing.T) {
	workspace := t.TempDir()

	t.Run("equal", func(t *testing.T) {
		writeWorksetTSVFixture(t, workspace, "equal/items.tsv", []string{"item"}, [][]string{{"alpha"}, {"beta"}})
		writeWorksetManifestFixture(t, workspace, "equal/items.json", []WorksetItem{
			{Key: "alpha", Bindings: map[string]string{"item": "alpha"}},
			{Key: "beta", Bindings: map[string]string{"item": "beta"}},
		})
		cmp := compareLegacyAndArtifactWorkset(t, workspace,
			TaskDef{ID: "legacy", FanOut: &FanOutSpec{Source: "equal/items.tsv", GoalTemplate: "process {item}"}},
			TaskDef{ID: "artifact", FanOut: &FanOutSpec{Source: "equal/items.json", GoalTemplate: "process {item}"}},
		)
		if !cmp.childCountEqual() {
			t.Fatalf("child count not equal: legacy=%d artifact=%d", len(cmp.LegacyGoals), len(cmp.ArtifactGoals))
		}
	})

	t.Run("mismatched", func(t *testing.T) {
		writeWorksetTSVFixture(t, workspace, "mismatched/items.tsv", []string{"item"}, [][]string{{"alpha"}, {"beta"}, {"gamma"}})
		writeWorksetManifestFixture(t, workspace, "mismatched/items.json", []WorksetItem{
			{Key: "alpha", Bindings: map[string]string{"item": "alpha"}},
			{Key: "beta", Bindings: map[string]string{"item": "beta"}},
		})
		cmp := compareLegacyAndArtifactWorkset(t, workspace,
			TaskDef{ID: "legacy", FanOut: &FanOutSpec{Source: "mismatched/items.tsv", GoalTemplate: "process {item}"}},
			TaskDef{ID: "artifact", FanOut: &FanOutSpec{Source: "mismatched/items.json", GoalTemplate: "process {item}"}},
		)
		if cmp.childCountEqual() {
			t.Fatal("child count unexpectedly equal despite a dropped manifest item; childCountEqual looks vacuous")
		}
	})
}

// TestLegacyAndArtifactWorksetConsumerShapedFixtureEquivalent is the
// consumer-shaped fixture required by
// docs/tmp/now/05-workset-legacy-tsv-removal.md §4/§9 (PR-1). No real
// consumer uses legacy TSV fan-out today — the only bundled team with a
// fan_out (.agent-teams/hufu-code-review/team.yaml) already uses
// source-artifact — so this fixture mirrors that team's actual shape
// (reviewing a workset item under a named "lens") as a stand-in until a
// real legacy consumer needs migrating.
func TestLegacyAndArtifactWorksetConsumerShapedFixtureEquivalent(t *testing.T) {
	workspace := t.TempDir()
	writeWorksetTSVFixture(t, workspace, "inputs/review.tsv", []string{"key", "lens"}, [][]string{
		{"internal/team/workset.go", "correctness"},
		{"internal/team/fan_out.go", "correctness"},
		{"cmd/hufu/teamlintcmd.go", "security"},
	})
	writeWorksetManifestFixture(t, workspace, "inputs/review.json", []WorksetItem{
		{Key: "internal/team/workset.go", Bindings: map[string]string{"key": "internal/team/workset.go", "lens": "correctness"}},
		{Key: "internal/team/fan_out.go", Bindings: map[string]string{"key": "internal/team/fan_out.go", "lens": "correctness"}},
		{Key: "cmd/hufu/teamlintcmd.go", Bindings: map[string]string{"key": "cmd/hufu/teamlintcmd.go", "lens": "security"}},
	})

	cmp := compareLegacyAndArtifactWorkset(t, workspace,
		TaskDef{ID: "legacy", FanOut: &FanOutSpec{Source: "inputs/review.tsv", GoalTemplate: "Review workset item {key} with the {lens} lens."}},
		TaskDef{ID: "artifact", FanOut: &FanOutSpec{Source: "inputs/review.json", GoalTemplate: "Review workset item {key} with the {lens} lens."}},
	)
	if !cmp.orderingEqual() || !cmp.keysEqual() || !cmp.templateSubstitutionEqual() || !cmp.childCountEqual() {
		t.Fatalf("consumer-shaped fixture diverged: %#v", cmp)
	}
}
