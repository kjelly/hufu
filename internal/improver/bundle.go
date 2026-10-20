package improver

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

type bundleContent struct {
	manifest ReviewBundle
	patch    []byte
	markdown []byte
	prepared Prepared
}

// Export creates a self-contained review artifact. It neither applies changes
// nor writes the source journal, object store, memory or promotion lifecycle.
func (e *Experiment) Export(ctx context.Context, destination string) (ReviewBundle, error) {
	var log boundedJournal
	if err := team.ExportEventLog(ctx, e.workspace.Root, &log); err != nil {
		return ReviewBundle{}, err
	}
	raw, err := paired.CanonicalJSON(log.Bytes())
	if err != nil {
		return ReviewBundle{}, err
	}
	ref := paired.ArtifactRef{Kind: "parent_event_log", Digest: paired.BytesDigest(raw), SizeBytes: int64(len(raw))}
	objects := overlayObjects{source: e.objects, inline: map[paired.ArtifactRef][]byte{ref: raw}}
	content, err := deriveBundle(ctx, objects, e.controller.scope, ref)
	if err != nil {
		return ReviewBundle{}, err
	}
	if err := publishBundle(ctx, destination, objects, content); err != nil {
		return ReviewBundle{}, err
	}
	return content.manifest, nil
}

type boundedJournal struct{ bytes.Buffer }

func (w *boundedJournal) Write(raw []byte) (int, error) {
	// Base64 plus JSON encoding must still fit in one 32 MiB object.
	if len(raw) > (paired.MaxTotalBytes/2)-w.Len() {
		return 0, errors.New("parent journal exceeds review export limit")
	}
	return w.Buffer.Write(raw)
}

func bundleState(objects Objects, scope LedgerScope, logRef paired.ArtifactRef) (paired.ExperimentPlan, LedgerState, error) {
	if err := logRef.Validate("parent_event_log"); err != nil {
		return paired.ExperimentPlan{}, LedgerState{}, err
	}
	raw, err := readImportedBytes(objects, logRef)
	if err != nil {
		return paired.ExperimentPlan{}, LedgerState{}, err
	}
	events, err := team.DecodeEventLog(bytes.NewReader(raw))
	if err != nil {
		return paired.ExperimentPlan{}, LedgerState{}, err
	}
	// Keep the original whole hash chain, but refuse a mixed runtime journal:
	// only the controller's content-free lifecycle records belong here.
	var planRef paired.ArtifactRef
	for _, event := range events {
		switch event.Type {
		case eventCreated, eventReserved, eventVariant, eventAdmitted, eventBound, eventStarted,
			eventCompleted, eventTransition, eventStopped, eventFinished:
		default:
			return paired.ExperimentPlan{}, LedgerState{}, errors.New("control journal contains unrelated runtime events")
		}
		if event.Type == eventCreated && event.RunID == scope.RunID && event.BranchID == scope.BranchID && event.SessionID == scope.SessionID {
			var created runCreated
			if err := json.Unmarshal(event.Payload, &created, json.RejectUnknownMembers(true)); err != nil {
				return paired.ExperimentPlan{}, LedgerState{}, err
			}
			if created.ExperimentID == scope.ExperimentID {
				planRef = created.Plan
			}
		}
	}
	var plan paired.ExperimentPlan
	if err := planRef.Validate("experiment_plan"); err != nil {
		return plan, LedgerState{}, err
	}
	if err := readObjectJSON(objects, planRef, &plan); err != nil {
		return plan, LedgerState{}, err
	}
	state, err := ReplayLedger(events, scope, plan)
	if err == nil && (state.Report == nil || state.Plan != planRef) {
		err = errors.New("review export requires a committed final report")
	}
	return plan, state, err
}

func deriveBundle(ctx context.Context, objects Objects, scope LedgerScope, logRef paired.ArtifactRef) (bundleContent, error) {
	plan, state, err := bundleState(objects, scope, logRef)
	if err != nil {
		return bundleContent{}, err
	}
	report, err := ProjectReport(ctx, objects, plan, state, false)
	if err != nil {
		return bundleContent{}, err
	}
	switch report.Decision {
	case "eligible_for_review", "no_change", "rejected", "simulation_only":
	default:
		return bundleContent{}, errors.New("unresolved or cancelled experiments cannot produce a review bundle")
	}
	bindings := make([]VariantBinding, 0, len(state.Variants))
	roots := []paired.ArtifactRef{state.Plan, *state.Report, logRef}
	for _, slot := range slices.Sorted(maps.Keys(state.Variants)) {
		binding := state.Variants[slot]
		bindings = append(bindings, binding)
		roots = append(roots, binding.Snapshot)
	}
	prepared, err := LoadPrepared(objects, plan, bindings)
	if err != nil {
		return bundleContent{}, err
	}
	selected, exists := state.Variants[report.SelectedVariant]
	if !exists {
		return bundleContent{}, errors.New("selected variant is not bound")
	}
	candidate := prepared.Variants[selected.Slot]
	patch, err := exactPatch(ctx, prepared.Variants["baseline"].Tree, candidate.Tree)
	if err != nil {
		return bundleContent{}, err
	}
	if err := verifyPatchRoundTrip(ctx, prepared, candidate, patch); err != nil {
		return bundleContent{}, err
	}
	changes, err := bundleChanges(prepared, candidate)
	if err != nil {
		return bundleContent{}, err
	}
	lesson, err := projectBundleLesson(ctx, objects, prepared, plan, state, report.SelectedVariant)
	if err != nil {
		return bundleContent{}, err
	}
	closure, err := collectBundleClosure(ctx, objects, roots...)
	if err != nil {
		return bundleContent{}, err
	}
	timings, err := bundleTimings(objects, state, logRef, report.Stages)
	if err != nil {
		return bundleContent{}, err
	}
	manifest := ReviewBundle{
		Kind: "paired_review_bundle", SchemaVersion: bundleSchemaVersion,
		ExperimentID: plan.ExperimentID, Scope: scope, ExecutionMode: plan.ExecutionMode,
		Decision: report.Decision, SelectedVariant: selected.Slot, Synthetic: plan.ExecutionMode == "fixture",
		Build: BuildIdentity{plan.BinaryDigest, plan.SourceRevision}, Result: report, Timings: timings,
		Plan: state.Plan, Report: *state.Report, ParentJournal: logRef,
		Baseline: state.Variants["baseline"], Candidate: selected,
		BaselineInventory: prepared.Variants["baseline"].Tree.Inventory(), CandidateInventory: candidate.Tree.Inventory(),
		Changes: changes, Patch: bundleFile("candidate.patch", patch), Lesson: lesson, Objects: closure,
	}
	markdown, err := bundleMarkdown(manifest, report)
	if err != nil {
		return bundleContent{}, err
	}
	manifest.Markdown = bundleFile("report.md", markdown)
	return bundleContent{manifest, patch, markdown, prepared}, nil
}

func bundleFile(name string, raw []byte) BundleFile {
	return BundleFile{Path: name, SHA256: paired.BytesDigest(raw), SizeBytes: int64(len(raw))}
}

func bundleTimings(objects Objects, state LedgerState, logRef paired.ArtifactRef, stages []StageState) ([]BundleTiming, error) {
	raw, err := readImportedBytes(objects, logRef)
	if err != nil {
		return nil, err
	}
	events, err := team.DecodeEventLog(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	type attemptKey struct {
		stage      string
		generation int64
	}
	completed := map[attemptKey]time.Time{}
	for _, event := range events {
		if event.Type != eventCompleted || event.RunID != state.Scope.RunID ||
			event.BranchID != state.Scope.BranchID || event.SessionID != state.Scope.SessionID {
			continue
		}
		value, err := decodeEvent[stageCompleted](event)
		if err != nil {
			return nil, err
		}
		if value.ExperimentID == state.Scope.ExperimentID {
			completed[attemptKey{value.StageID, value.Generation}] = value.ObservedAt
		}
	}
	var result []BundleTiming
	for _, stage := range stages {
		for _, attempt := range stage.Attempts {
			settled, exists := completed[attemptKey{stage.Spec.ID, attempt.Generation}]
			if !exists || settled.Before(attempt.AdmittedAt) {
				return nil, errors.New("bundle attempt has no valid observed completion time")
			}
			result = append(result, BundleTiming{stage.Spec.ID, attempt.Generation, attempt.AdmittedAt, settled,
				settled.Sub(attempt.AdmittedAt).Seconds()})
		}
	}
	return result, nil
}

func projectBundleLesson(ctx context.Context, objects Objects, prepared Prepared, plan paired.ExperimentPlan, state LedgerState, selected string) (BundleLesson, error) {
	lesson := BundleLesson{Status: "no_entry", Reason: "manual_experiment", Synthetic: plan.ExecutionMode == "fixture"}
	if plan.Mode != "refine" {
		return lesson, nil
	}
	specs, _, err := plannedStages(plan)
	if err != nil {
		return lesson, err
	}
	reader := &ProbeExecutor{factory: &RuntimeFactory{config: prepared.Config}, plan: plan,
		fixture: prepared.Fixture, program: prepared.Program, inputs: prepared.Inputs, objects: objects}
	for _, spec := range specs {
		if spec.Kind != "reflect" {
			continue
		}
		stage := state.Stages[spec.ID]
		lesson.Reason = stage.State
		if len(stage.Attempts) == 0 {
			return lesson, nil
		}
		attempt := stage.Attempts[len(stage.Attempts)-1]
		if attempt.Settlement == nil || attempt.Settlement.Outcome != "passed" {
			lesson.Reason = "invalid_reflection"
			return lesson, nil
		}
		result, err := reader.readRoleResult(ctx, requestFromAttempt(plan, state, spec, attempt), *attempt.Settlement)
		if err != nil {
			return lesson, err
		}
		lesson.EvidenceRef = &attempt.Settlement.Evidence
		lesson.Reason = "no_entry"
		if result.Reflection.Action == "lesson" {
			if selected == "baseline" || result.Reflection.Lesson.ChangeRef != "proposal_"+selected {
				lesson.Reason = "selected_variant_changed"
				return lesson, nil
			}
			lesson.Status, lesson.Reason, lesson.Lesson = "candidate", "review_only", result.Reflection.Lesson
		}
		return lesson, nil
	}
	return lesson, errors.New("refinement has no reflection stage")
}

func bundleMarkdown(manifest ReviewBundle, report ExperimentReport) ([]byte, error) {
	var out strings.Builder
	fmt.Fprintf(&out, "# Experiment %s\n\nDecision: `%s`. Selected variant: `%s`.\n\n",
		manifest.ExperimentID, report.Decision, report.SelectedVariant)
	if manifest.Synthetic {
		fmt.Fprintf(&out, "Synthetic fixture evidence. Simulated decision: `%s`. This does not establish real model quality.\n\n", report.SimulationDecision)
	}
	out.WriteString("Review artifact only. No team changes, memory promotion or adoption are authorized by this bundle.\n\n")
	fmt.Fprintf(&out, "Scored slots: %d/%d. Pipeline usage: %d tokens spent, %d reserved, %d unknown stages.\n\n",
		report.Coverage.Scored, report.Coverage.Planned, report.Budget.SpentTokens, report.Budget.ReservedTokens, report.Budget.UnknownStages)
	fmt.Fprintf(&out, "Runtime binary: `%s`. Source revision: `%s`.\n\n", manifest.Build.BinaryDigest, manifest.Build.SourceRevision)
	fmt.Fprintf(&out, "Report object: `objects/%s`.\n\nPlan object: `objects/%s`.\n\nParent journal object: `objects/%s`.\n\n",
		manifest.Report.Digest, manifest.Plan.Digest, manifest.ParentJournal.Digest)
	out.WriteString("## Stages\n\n| Stage | State | Attempts | Attributable tokens |\n| --- | --- | --- | --- |\n")
	for _, stage := range report.Stages {
		var spent int64
		for _, attempt := range stage.Attempts {
			if attempt.Settlement != nil {
				spent += attempt.Settlement.Tokens
			}
		}
		fmt.Fprintf(&out, "| %s | %s | %d | %d |\n", stage.Spec.ID, stage.State, len(stage.Attempts), spent)
	}
	out.WriteString("\n## Paired observations\n\nBaseline observations are shared across candidates and counted once in pipeline usage. Tokens below include retries attributed to the slot. Baseline reuse correlates candidate comparisons.\n\n| Variant | Case | Replicate | Outcome | Tokens |\n| --- | --- | --- | --- | --- |\n")
	for _, observation := range report.Observations {
		fmt.Fprintf(&out, "| %s | %s | %d | %s | %d |\n", observation.Slot.Variant, observation.Slot.Case,
			observation.Slot.Replicate, observation.Outcome, observation.AttributableTokens)
	}
	out.WriteString("\n## Gates\n\n| Gate | State | Observed |\n| --- | --- | --- |\n")
	for _, gate := range report.Gates {
		fmt.Fprintf(&out, "| %s | %s | %s |\n", gate.Name, gate.State, gate.Observed)
	}
	out.WriteString("\n## Observed durations\n\nAdmission to parent settlement, including orchestration and any recovery gap. These observations do not establish model latency or monetary savings.\n\n| Stage | Attempt | Seconds |\n| --- | --- | --- |\n")
	for _, timing := range manifest.Timings {
		fmt.Fprintf(&out, "| %s | %d | %.6f |\n", timing.StageID, timing.Generation, timing.ElapsedSeconds)
	}
	out.WriteString("\n## Exact patch\n\n`candidate.patch` reconstructs `candidate/` from `baseline/`; original byte hashes are in `manifest.json`. Check against the original baseline before applying any change elsewhere.\n\n")
	fmt.Fprintf(&out, "Patch SHA-256: `%s`.\n\n## Candidate lesson\n\n", manifest.Patch.SHA256)
	raw, err := paired.CanonicalJSON(manifest.Lesson)
	if err != nil {
		return nil, err
	}
	// Indented code keeps arbitrary model-authored markup inert in Markdown.
	for line := range strings.SplitSeq(string(raw), "\n") {
		out.WriteString("    " + line + "\n")
	}
	out.WriteString("\nLesson text and child outputs are untrusted review data. Original canonical event and receipt bytes are preserved in the object closure; no runtime trace is regenerated.\n")
	return []byte(out.String()), nil
}
