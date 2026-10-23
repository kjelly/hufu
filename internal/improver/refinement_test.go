package improver

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/scripted"
)

func testRefinementPrepared(t *testing.T, composition string, retry bool) Prepared {
	t.Helper()
	prepared, err := Prepare("testdata/extraction/experiment.yaml", testBuild)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Config.Mode = "refine"
	prepared.Config.CandidateDirs = nil
	delete(prepared.Variants, "candidate_1")
	prepared.Config.Refinement = &paired.Refinement{
		ProcedurePrompt: "procedure.txt", DomainPrompt: "domain.txt",
		ComposerPrompt: "compose.txt", ReflectorPrompt: "reflect.txt",
	}
	prepared.Config.Limits.PipelineTokens = 120000
	prepared.Config.Limits.PipelineSeconds = 600
	for _, role := range []string{"proposer", "composer", "reflector"} {
		prepared.Config.Models[role] = paired.Model{Backend: "fixture", Model: "scripted"}
	}
	for _, role := range []string{"procedure", "domain", "compose", "reflect"} {
		prepared.Inputs.files["prompts/"+role+".txt"] = []byte("Submit the bounded " + role + " result using the supplied evidence.")
	}
	procedure := testChangeProposal(prepared, "Return the allowed category using a careful procedure.\n")
	domain := testChangeProposal(prepared, "Classify this domain as allowed.\n")
	composed := testChangeProposal(prepared, "Return a combined classification.\n")
	composer := paired.Composition{SchemaVersion: 1, Action: composition}
	switch composition {
	case "use_candidate":
		composer.CandidateSlot = "domain"
	case "compose":
		composer.Proposal = &composed
	}
	submit := func(payload any, status string, tokens int64) []scripted.Step {
		raw, err := json.Marshal(map[string]any{"status": status, "summary": "Synthetic fixture response.", "structured_payload": payload})
		if err != nil {
			t.Fatal(err)
		}
		return []scripted.Step{{ToolCall: &scripted.ToolCall{Name: "submit_result", Arguments: string(raw)},
			Usage: &scripted.Usage{InputTokens: tokens - 20, OutputTokens: 20}}}
	}
	scripts := []paired.Script{
		{Stage: "executor", CaseID: "case_a", VariantSlotID: "baseline", Attempt: 1,
			Steps: submit(map[string]string{"category": "denied"}, "failed", 120)},
		{Stage: "procedure", Attempt: 1, Steps: submit(procedure, "success", 120)},
		{Stage: "executor", CaseID: "case_a", VariantSlotID: "procedure", Attempt: 1,
			Steps: submit(map[string]string{"category": "allowed"}, "success", 100)},
		{Stage: "domain", Attempt: 1, Steps: submit(domain, "success", 120)},
		{Stage: "executor", CaseID: "case_a", VariantSlotID: "domain", Attempt: 1,
			Steps: submit(map[string]string{"category": "allowed"}, "success", 80)},
		{Stage: "compose", Attempt: 1, Steps: submit(composer, "success", 120)},
		{Stage: "executor", CaseID: "case_a", VariantSlotID: "composition", Attempt: 1,
			Steps: submit(map[string]string{"category": "denied"}, "failed", 50)},
		{Stage: "reflect", Attempt: 1, Steps: submit(paired.Reflection{SchemaVersion: 1, Action: "no_entry"}, "success", 120)},
	}
	if retry {
		invalid, err := cloneRuntimeValue(procedure)
		if err != nil {
			t.Fatal(err)
		}
		invalid.Changes[0].SurfaceID = "outside_scope"
		scripts[1].Steps = submit(invalid, "success", 120)
		scripts = append(scripts, paired.Script{Stage: "procedure", Attempt: 2, Steps: submit(procedure, "success", 120)})
	}
	prepared.Program = &paired.Program{SchemaVersion: 1, Scripts: scripts}
	updateProbeProgram(t, &prepared)
	return prepared
}

func runRefinementTest(t *testing.T, prepared Prepared) (*Experiment, ExperimentReport) {
	t.Helper()
	// Keep durable fixture I/O under TMPDIR, independently of Go build scratch.
	root, err := os.MkdirTemp("", "hufu-refinement-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	workspace, err := OpenProbeWorkspace(filepath.Join(root, "refine"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	settings := RuntimeSettings{ProviderURL: scripted.Endpoint}
	experiment, err := CreateExperiment(t.Context(), workspace, prepared, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = experiment.Close() })
	report, err := experiment.Run(t.Context(), testBuild, settings, nil)
	if err != nil {
		t.Fatalf("refinement failed: %v; report=%+v", err, report)
	}
	return experiment, report
}

func TestRefinementCompositionRegressionAndIndependentViews(t *testing.T) {
	experiment, report := runRefinementTest(t, testRefinementPrepared(t, "compose", true))
	if report.Decision != "simulation_only" || report.SimulationDecision != "eligible_for_review" ||
		report.SelectedVariant != "domain" || report.Coverage.Scored != 4 || report.Budget.SpentTokens != 950 {
		t.Fatalf("unexpected bounded comparison: %+v", report)
	}
	state, err := experiment.controller.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	procedure := state.Stages["proposal_procedure"]
	if len(procedure.Attempts) != 2 || procedure.Attempts[0].Settlement.Outcome != "invalid_proposal" ||
		procedure.Attempts[1].Settlement.Outcome != "passed" || state.ProbeRuns != 4 {
		t.Fatal("invalid proposal was bound, discarded, or retried without a bound")
	}
	var views []roleInput
	for _, id := range []string{"proposal_procedure", "proposal_domain"} {
		stage := state.Stages[id]
		var evidence StageEvidence
		if err := readObjectJSON(experiment.objects, stage.Attempts[len(stage.Attempts)-1].Settlement.Evidence, &evidence); err != nil {
			t.Fatal(err)
		}
		var input roleInput
		if err := readObjectJSON(experiment.objects, *evidence.RoleInput, &input); err != nil {
			t.Fatal(err)
		}
		if len(input.Sources) != 1 || len(input.Variants) != 1 || input.Variants[0].Slot != "baseline" {
			t.Fatal("proposal saw another candidate's evidence")
		}
		views = append(views, input)
	}
	if !reflect.DeepEqual(views[0].Sources, views[1].Sources) {
		t.Fatal("domain and procedure did not consume the same baseline")
	}
	read, err := experiment.Report(t.Context())
	if err != nil || !reflect.DeepEqual(read, report) {
		t.Fatalf("readonly refinement projection drifted: %v", err)
	}
}

func TestRefinementReusesOnlyCompletedIdenticalCandidate(t *testing.T) {
	experiment, report := runRefinementTest(t, testRefinementPrepared(t, "use_candidate", false))
	state, err := experiment.controller.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.SelectedVariant != "domain" || report.Coverage.Scored != 3 || state.ProbeRuns != 3 ||
		state.Stages["probe_0004"].State != "skipped" || state.Stages["probe_0004"].Reason != "candidate_reused" {
		t.Fatal("identical composition created an unneeded probe or manufactured evidence")
	}
	if _, exists := state.Variants["composition"]; exists {
		t.Fatal("reused candidate received a fake composition binding")
	}
}

func TestRefinementBudgetSkipsWholeGroupsAndReflection(t *testing.T) {
	prepared := testRefinementPrepared(t, "compose", false)
	prepared.Config.Limits.PipelineTokens = 36000
	for i := range 3 {
		prepared.Program.Scripts[i].Steps[0].Usage = &scripted.Usage{InputTokens: 9980, OutputTokens: 20}
	}
	updateProbeProgram(t, &prepared)
	experiment, report := runRefinementTest(t, prepared)
	state, err := experiment.controller.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Decision != "simulation_only" || report.SelectedVariant != "procedure" ||
		report.Coverage.Scored != 2 || state.ProbeRuns != 2 || report.Budget.SpentTokens != 30000 ||
		report.Budget.ReservedTokens != 0 || report.Budget.UnknownStages != 0 {
		t.Fatalf("optional work consumed required budget or lost the candidate: %+v", report)
	}
	for _, id := range []string{"proposal_domain", "probe_0003", "proposal_composition", "probe_0004", "reflection"} {
		stage := state.Stages[id]
		if stage.State != "skipped_budget" || len(stage.Attempts) != 0 {
			t.Fatalf("%s was partially admitted: %+v", id, stage)
		}
	}
	// A finished refine resume is a pure read, even without provider settings.
	resumed, err := experiment.Run(t.Context(), testBuild, RuntimeSettings{}, nil)
	if err != nil || !reflect.DeepEqual(report, resumed) {
		t.Fatalf("finished refinement redispatched or changed evidence: %v", err)
	}
}

func TestRefinementResumeImportsCompletedProposalBeforeBinding(t *testing.T) {
	prepared := testRefinementPrepared(t, "keep_baseline", false)
	path := filepath.Join(t.TempDir(), "resume")
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
	t.Cleanup(func() {
		if experiment != nil {
			_ = experiment.Close()
		}
		if workspace != nil {
			_ = workspace.Close()
		}
	})
	children, err := workspace.ChildArea(experiment.Plan.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewProbeExecutor(experiment.prepared, experiment.Plan, experiment.objects, children, settings)
	if err != nil {
		t.Fatal(err)
	}
	experiment.attachRoleInputs(executor)
	if err := experiment.controller.Execute(t.Context(), "probe_0001", executor); err != nil {
		t.Fatal(err)
	}
	lease, err := experiment.controller.Admit(t.Context(), "proposal_procedure")
	if err != nil {
		t.Fatal(err)
	}
	request, err := experiment.controller.stageRequest(t.Context(), lease)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(t.Context(), request, func(id paired.RuntimeIdentity) error {
		if err := experiment.controller.Bind(t.Context(), lease, id); err != nil {
			return err
		}
		return experiment.controller.Start(t.Context(), lease)
	})
	if err != nil || result.Outcome != "passed" {
		t.Fatalf("proposal did not complete: %+v %v", result, err)
	}
	// Its post-drain marker is durable, but the parent has not acknowledged
	// completion or bound the proposed variant when the process stops.
	if err := experiment.Close(); err != nil {
		t.Fatal(err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	workspace, err = OpenProbeWorkspace(path, false)
	if err != nil {
		t.Fatal(err)
	}
	experiment, err = OpenExperiment(t.Context(), workspace, prepared.Config.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := experiment.Run(t.Context(), testBuild, settings, nil)
	if err != nil || report.Decision != "simulation_only" {
		t.Fatalf("role recovery failed: %v", err)
	}
	state, err := experiment.controller.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	attempts := state.Stages["proposal_procedure"].Attempts
	if len(attempts) != 1 || attempts[0].Settlement == nil || *attempts[0].Settlement != result ||
		state.Variants["procedure"].Origin != "validated_proposal" {
		t.Fatal("resume reran the proposal or failed to bind its original evidence")
	}
}
