package improver

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/scripted"
)

func TestExperimentOfflineRoundTripAndReadOnlyReport(t *testing.T) {
	prepared, err := Prepare("testdata/extraction/experiment.yaml", testBuild)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "probe")
	workspace, err := OpenProbeWorkspace(path, false)
	if err != nil {
		t.Fatal(err)
	}
	settings := RuntimeSettings{ProviderURL: scripted.Endpoint}
	experiment, err := CreateExperiment(t.Context(), workspace, prepared, settings)
	if err != nil {
		_ = workspace.Close()
		t.Fatal(err)
	}
	report, err := experiment.RunManual(t.Context(), testBuild, settings, nil)
	if err != nil {
		_ = experiment.Close()
		_ = workspace.Close()
		t.Fatal(err)
	}
	if report.Decision != "simulation_only" || report.SimulationDecision != "eligible_for_review" ||
		report.SelectedVariant != "candidate_1" || report.Coverage.Scored != 2 || report.Budget.SpentTokens != 240 ||
		len(report.ManifestRefs) != 2 || len(report.Manifests) != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	state, err := experiment.controller.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A syntactically valid, content-addressed report is not authority to
	// replace the journal-derived decision, even if its digest verifies.
	forged := report
	forged.Decision, forged.ExecutionMode = "eligible_for_review", "llm"
	ref, err := experiment.objects.PutJSON("experiment_report", forged)
	if err != nil {
		t.Fatal(err)
	}
	state.Report, state.Decision = &ref, forged.Decision
	if _, err := ProjectReport(t.Context(), experiment.objects, experiment.Plan, state, false); err == nil {
		t.Fatal("reader trusted a forged report instead of canonical fixture evidence")
	}
	if err := experiment.Close(); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	// No child database is needed to inspect a finished experiment.
	if err := os.RemoveAll(filepath.Join(path, "improvement", "paired", prepared.Config.ExperimentID, "children")); err != nil {
		t.Fatal(err)
	}
	before, err := CaptureTree(path)
	if err != nil {
		t.Fatal(err)
	}
	databaseBefore, err := os.ReadFile(filepath.Join(path, "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = OpenProbeWorkspace(path, true)
	if err != nil {
		t.Fatal(err)
	}
	experiment, err = OpenExperiment(t.Context(), workspace, prepared.Config.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := experiment.Report(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report, loaded) {
		t.Fatal("reader disagrees with published report")
	}
	if err := experiment.Close(); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := CaptureTree(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Inventory(), after.Inventory()) {
		t.Fatal("read-only inspection modified the workspace")
	}
	databaseAfter, err := os.ReadFile(filepath.Join(path, "runtime.sqlite"))
	if err != nil || !bytes.Equal(databaseBefore, databaseAfter) {
		t.Fatal("read-only inspection modified the canonical database")
	}
	// Finished resume must work without a provider and without re-creating
	// the already removed child area.
	workspace, err = OpenProbeWorkspace(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = workspace.Close() }()
	experiment, err = OpenExperiment(t.Context(), workspace, prepared.Config.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = experiment.Close() }()
	resumed, err := experiment.RunManual(t.Context(), testBuild, RuntimeSettings{}, nil)
	if err != nil || !reflect.DeepEqual(report, resumed) {
		t.Fatalf("finished resume called runtime or changed report: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "improvement", "paired", prepared.Config.ExperimentID, "children")); !os.IsNotExist(err) {
		t.Fatal("finished resume re-created child area")
	}
}

func TestProbeWorkspaceReadOnlyMissingDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	if workspace, err := OpenProbeWorkspace(path, true); err == nil {
		_ = workspace.Close()
		t.Fatal("read-only open created a workspace")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing workspace was created")
	}
}

func TestExperimentResumeRejectsBuildDriftBeforeChild(t *testing.T) {
	prepared, err := Prepare("testdata/extraction/experiment.yaml", testBuild)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := OpenProbeWorkspace(filepath.Join(t.TempDir(), "probe"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = workspace.Close() }()
	experiment, err := CreateExperiment(t.Context(), workspace, prepared, RuntimeSettings{ProviderURL: scripted.Endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = experiment.Close() }()
	if _, err := experiment.RunManual(context.Background(), BuildIdentity{BinaryDigest: "different"}, RuntimeSettings{}, nil); err == nil {
		t.Fatal("drifted build accepted")
	}
	state, err := experiment.controller.Read(t.Context())
	if err != nil || state.ProbeRuns != 0 {
		t.Fatal("build drift admitted a child")
	}
}
