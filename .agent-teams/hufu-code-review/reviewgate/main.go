// reviewgate checks the team's review handoffs without interpreting task prose.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/actionstate"
)

type finding struct {
	SourceExcerpt    string   `json:"source_excerpt,omitempty"`
	SourceSide       string   `json:"source_side,omitempty"`
	SourceRevision   string   `json:"source_revision,omitempty"`
	Location         string   `json:"location"`
	Summary          string   `json:"summary"`
	Detail           string   `json:"detail"`
	Severity         string   `json:"severity"`
	Evidence         []string `json:"evidence"`
	RequiresCritique bool     `json:"requires_critique"`
}

type decision struct {
	FindingIndex int      `json:"finding_index"`
	Verdict      string   `json:"verdict"`
	Evidence     []string `json:"evidence"`
}

type inputReview struct {
	TaskID string          `json:"task_id"`
	Hash   string          `json:"payload_sha256"`
	Record json.RawMessage `json:"record"`
}

type record struct {
	Kind        string                `json:"kind"`
	Revision    string                `json:"review_revision"`
	Key         string                `json:"workset_key"`
	Lens        string                `json:"lens"`
	Findings    []finding             `json:"findings"`
	Gaps        []json.RawMessage     `json:"gaps"`
	InputReview []inputReview         `json:"input_review"`
	Decisions   []decision            `json:"decisions"`
	Invariants  []invariantAssessment `json:"invariant_assessments"`
	References  []referenceAssessment `json:"reference_assessments"`
}

type referenceAssessment struct {
	ID           string   `json:"reference_id"`
	Kind         string   `json:"kind"`
	Rationale    string   `json:"rationale"`
	Evidence     []string `json:"evidence"`
	FindingIndex *int     `json:"finding_index"`
	GapIndex     *int     `json:"gap_index"`
}

type invariantAssessment struct {
	ID              string   `json:"invariant_id"`
	Status          string   `json:"status"`
	Summary         string   `json:"summary"`
	FindingIndex    *int     `json:"finding_index,omitempty"`
	MissingEvidence []string `json:"missing_evidence,omitempty"`
}

type gapResolution struct {
	TaskID          string   `json:"source_task_id"`
	Index           int      `json:"gap_index"`
	Status          string   `json:"status"`
	ResolvingTaskID string   `json:"resolving_task_id"`
	Evidence        []string `json:"evidence"`
}

type payload struct {
	CanonicalFindings  json.RawMessage   `json:"canonical_findings,omitempty"`
	Scope              json.RawMessage   `json:"scope"`
	Tests              json.RawMessage   `json:"test_verification"`
	SnapshotCoverage   json.RawMessage   `json:"snapshot_coverage"`
	Record             json.RawMessage   `json:"record"`
	InputReview        []inputReview     `json:"input_review"`
	Coverage           string            `json:"coverage"`
	Completion         *reportCompletion `json:"completion,omitempty"`
	GapResolutions     []gapResolution   `json:"gap_resolutions"`
	SupplementOutcomes json.RawMessage   `json:"supplement_outcomes,omitempty"`
}

type sourceArtifactRef struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	TaskID string `json:"task_id"`
}

type sourceTask struct {
	Workspace    string `json:"workspace"`
	ID           string
	Status       string
	ContractID   string   `json:"contract_id"`
	EvidenceFrom []string `json:"evidence_from"`
	Binding      *struct {
		Bindings     map[string]string   `json:"bindings"`
		TouchedPaths []string            `json:"touched_paths"`
		Inputs       []sourceArtifactRef `json:"inputs"`
	} `json:"workset_binding"`
	VerifiedResult  bool   `json:"verified_result"`
	VerifiedOutputs bool   `json:"verified_outputs"`
	ResultError     string `json:"result_error"`
	OutputsError    string `json:"outputs_error"`

	Result *struct {
		RuntimeOutputs map[string]json.RawMessage `json:"runtime_outputs"`
		Summary        string                     `json:"summary"`
		Status         string                     `json:"status"`
		Findings       []finding                  `json:"findings"`
		Invariants     *struct {
			Assessments []invariantAssessment `json:"assessments"`
		} `json:"invariant_verification"`
		FilesRead []struct {
			Path string `json:"path"`
		} `json:"files_read"`
		Payload *struct {
			Value json.RawMessage `json:"value"`
			Hash  string          `json:"sha256"`
		} `json:"structured_payload"`
	} `json:"typed_result"`
}

type sourceSession struct {
	Version               int          `json:"version"`
	InputID               string       `json:"input_id"`
	Tasks                 []sourceTask `json:"tasks"`
	ArtifactDataDirectory string       `json:"artifact_data_directory"`
}

// Run is the only embedded action entrypoint. Runtime-owned action identity,
// rather than model-selected paths, selects the checkpoint to inspect.
func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	decoder := json.NewDecoder(io.LimitReader(in, 65537))
	decoder.DisallowUnknownFields()
	// The runtime also supplies capability in the action envelope.
	var envelope struct {
		Capability string `json:"capability"`
		Type       string `json:"type"`
		Payload    string `json:"payload"`
	}
	if err := decoder.Decode(&envelope); err != nil {
		return err
	}
	if envelope.Type != "verify_review_report" && envelope.Type != "inventory_review_handoffs" && envelope.Type != "inventory_review_supplement_requests" && envelope.Type != "derive_review_report" {
		return fmt.Errorf("unsupported review gate action")
	}
	var arguments struct {
		Scope json.RawMessage `json:"scope"`
	}
	payloadDecoder := json.NewDecoder(strings.NewReader(envelope.Payload))
	payloadDecoder.DisallowUnknownFields()
	if err := payloadDecoder.Decode(&arguments); err != nil || len(arguments.Scope) == 0 || string(arguments.Scope) == "null" {
		return fmt.Errorf("review gate requires a bound scope object")
	}
	var extra any
	if err := payloadDecoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one review gate payload")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("expected one action envelope")
	}
	data, err := actionstate.ActionSources(ctx)
	if err != nil {
		return fmt.Errorf("read verified review sources: %w", err)
	}
	var current sourceSession
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	if current.Version != 1 {
		return fmt.Errorf("unsupported source view version")
	}
	matched := false
	for _, item := range current.Tasks {
		isGate := (envelope.Type == "verify_review_report" && item.ContractID == "verify-review-report") ||
			(envelope.Type == "derive_review_report" && item.ContractID == "derive-review-report") ||
			(envelope.Type == "inventory_review_handoffs" && item.ContractID == "inventory-review-handoffs") ||
			(envelope.Type == "inventory_review_supplement_requests" && item.ContractID == "inventory-review-supplement-requests")
		if item.ID == os.Getenv("HUFU_TASK_ID") && isGate {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("current review gate occurrence is absent from source view")
	}
	for _, item := range current.Tasks {
		if item.ContractID == "produce-workset" && item.Result != nil {
			var attestation struct {
				Requested json.RawMessage `json:"requested"`
			}
			if err := json.Unmarshal(item.Result.RuntimeOutputs["scope"], &attestation); err != nil || !equalJSON(arguments.Scope, attestation.Requested) {
				return fmt.Errorf("review gate scope differs from the producer attestation")
			}
		}
	}
	return runGateAction(ctx, envelope.Type, current, out)
}

func accepted(item sourceTask) (payload, error) {
	var value payload
	if !item.VerifiedResult || item.Result == nil || item.Result.Payload == nil {
		return value, fmt.Errorf("task %s lacks a current accepted, verified handoff: %s", item.ID, item.ResultError)
	}
	if err := json.Unmarshal(item.Result.Payload.Value, &value); err != nil {
		return value, err
	}
	return value, nil
}

func equalJSON(a, b json.RawMessage) bool {
	left, leftErr := canonicalJSON(a)
	right, rightErr := canonicalJSON(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func bind(inputs []inputReview, ids []string, tasks map[string]sourceTask, values map[string]payload) error {
	if len(inputs) != len(ids) {
		return fmt.Errorf("input handoff count differs from evidence_from")
	}
	seen := make(map[string]bool)
	allowed := make(map[string]bool)
	for _, id := range ids {
		allowed[id] = true
	}
	for _, input := range inputs {
		source, ok := tasks[input.TaskID]
		if !ok || !allowed[input.TaskID] || seen[input.TaskID] || source.Result == nil || source.Result.Payload == nil || input.Hash != source.Result.Payload.Hash {
			return fmt.Errorf("invalid input binding for task %s", input.TaskID)
		}
		seen[input.TaskID] = true
		first, err := canonicalJSON(input.Record)
		if err != nil {
			return err
		}
		second, err := canonicalJSON(values[input.TaskID].Record)
		if err != nil || !bytes.Equal(first, second) {
			return fmt.Errorf("substituted source record for task %s", input.TaskID)
		}
	}
	return nil
}

func observed(item sourceTask, evidence []string) bool {
	if len(evidence) == 0 || item.Result == nil {
		return false
	}
	for _, ref := range evidence {
		found := false
		for _, file := range item.Result.FilesRead {
			if file.Path == ref {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func verify(current sourceSession) (string, string, error) {
	tasks, values, records, err := collectReportSources(current)
	if err != nil {
		return "", "", err
	}
	var reportID string
	var report payload
	for _, item := range current.Tasks {
		if item.ContractID != "synthesize-review" {
			continue
		}
		if reportID != "" || tasks[item.ID].ID != "" {
			return "", "", fmt.Errorf("ambiguous report tasks")
		}
		report, err = accepted(item)
		if err != nil {
			return "", "", err
		}
		reportID, tasks[item.ID] = item.ID, item
	}
	if reportID == "" {
		return "", "", fmt.Errorf("review report missing")
	}
	snapshot, err := collectSnapshotCoverage(current)
	if err != nil {
		return "", "", &sourceReviewError{cause: err}
	}
	coverage, completion, err := deriveReportCompletion(tasks, values, records, reportTestEvidence(current), snapshot)
	if err != nil {
		return "", "", err
	}
	if err := verifyActionEvidence(current, report); err != nil {
		return "", "", err
	}
	if snapshot.Mode == "repository_snapshot" {
		if err := verifySnapshotCoverage(current, snapshot, report.SnapshotCoverage); err != nil {
			return "", "", err
		}
	}
	if err := verifyReportHandoffEvidence(current, report); err != nil {
		return "", "", err
	}
	if len(report.InputReview) != len(records) {
		return "", "", fmt.Errorf("report omitted review or critic tasks")
	}
	if err := bind(report.InputReview, tasks[reportID].EvidenceFrom, tasks, values); err != nil {
		return "", "", err
	}
	for _, input := range report.InputReview {
		if _, ok := records[input.TaskID]; !ok {
			return "", "", fmt.Errorf("report included a non-review source")
		}
	}

	if report.Coverage != coverage || tasks[reportID].Result.Status != completion.Status {
		return "", "", fmt.Errorf("report overstated coverage")
	}
	canonicalFindings, err := canonicalDraftFindings(current, tasks, records)
	if err != nil {
		return "", "", &sourceReviewError{cause: err}
	}
	if err := verifyCanonicalFindings(current, report, canonicalFindings); err != nil {
		return "", "", err
	}

	if err := verifyReportCompletion(current, report, coverage, completion, tasks[reportID].Result.Summary); err != nil {
		return "", "", err
	}
	if err := verifyGaps(report, records, tasks); err != nil {
		return "", "", err
	}
	return reportID, tasks[reportID].Result.Payload.Hash, nil
}

type targetedTestEvidence struct {
	Completed       bool     `json:"completed"`
	Passed          *bool    `json:"passed"`
	ExitCode        *int     `json:"exit_code"`
	TimedOut        bool     `json:"timed_out"`
	OutputTruncated bool     `json:"output_truncated"`
	Diagnostic      string   `json:"diagnostic"`
	Gap             string   `json:"gap"`
	TimeoutKind     string   `json:"timeout_kind"`
	Command         []string `json:"command"`
	Packages        []string `json:"packages"`
}

func validTestGap(tests targetedTestEvidence) bool {
	if tests.Completed || *tests.Passed || tests.OutputTruncated || strings.TrimSpace(tests.Diagnostic) == "" {
		return false
	}
	switch tests.Gap {
	case "test_timeout_gap":
		if !tests.TimedOut || len(tests.Packages) == 0 {
			return false
		}
		switch tests.TimeoutKind {
		case "preflight":
			return *tests.ExitCode == -1 && len(tests.Command) == 0
		case "test", "command":
			return (*tests.ExitCode == 1 || *tests.ExitCode == -1) && len(tests.Command) > 0
		}
	case "test_scope_empty", "test_dependency_outside_snapshot":
		return !tests.TimedOut && tests.TimeoutKind == "none" && *tests.ExitCode == -1 && len(tests.Command) == 0 &&
			(len(tests.Packages) == 0) == (tests.Gap == "test_scope_empty")
	}
	return false
}

// A completed review may expose failing target tests. Execution failures remain
// blocking; negative or truncated test evidence must remain visible as a gap.
func verifyTestEvidence(data json.RawMessage) (bool, error) {
	if len(data) == 0 {
		return false, fmt.Errorf("missing verified execution output")
	}
	var tests targetedTestEvidence
	if err := json.Unmarshal(data, &tests); err != nil {
		return false, err
	}
	if tests.Passed == nil || tests.ExitCode == nil {
		return false, fmt.Errorf("targeted tests lack an outcome")
	}
	if tests.Gap != "" {
		if !validTestGap(tests) {
			return false, fmt.Errorf("targeted tests have inconsistent gap evidence")
		}
		return true, nil
	}
	if !tests.Completed || tests.TimedOut || (tests.TimeoutKind != "" && tests.TimeoutKind != "none") ||
		(*tests.ExitCode != 0 && *tests.ExitCode != 1) || *tests.Passed != (*tests.ExitCode == 0) {
		return false, fmt.Errorf("targeted tests lack consistent completed execution evidence")
	}
	if !*tests.Passed && strings.TrimSpace(tests.Diagnostic) == "" {
		return false, fmt.Errorf("failed targeted tests lack diagnostics")
	}
	return !*tests.Passed || tests.OutputTruncated, nil
}

func verifyReview(item sourceTask, r record) error {
	if r.Kind != "review" || item.Binding == nil || r.Revision != item.Binding.Bindings["review_revision"] || r.Key != item.Binding.Bindings["key"] || r.Lens != item.Binding.Bindings["lens"] {
		return fmt.Errorf("task %s changed its workset identity", item.ID)
	}
	if len(r.Findings) != len(item.Result.Findings) {
		return fmt.Errorf("task %s omitted typed findings", item.ID)
	}
	for i, f := range r.Findings {
		if f.Summary != item.Result.Findings[i].Summary || f.Detail != item.Result.Findings[i].Detail || f.Severity != item.Result.Findings[i].Severity || ((f.Severity == "error" || f.Severity == "warning") && !f.RequiresCritique) {
			return fmt.Errorf("task %s /record/findings/%d is inconsistent with the accepted envelope", item.ID, i)
		}
		if len(f.Evidence) == 0 {
			return fmt.Errorf("task %s /record/findings/%d/evidence has no observed source", item.ID, i)
		}
		for index, reference := range f.Evidence {
			if !observed(item, []string{reference}) {
				return fmt.Errorf("task %s /record/findings/%d/evidence/%d: reference %q was not observed by this task", item.ID, i, index, reference)
			}
		}
	}
	if (len(r.Gaps) > 0) != (item.Result.Status == "completed_with_gaps") {
		return fmt.Errorf("task %s concealed its coverage status", item.ID)
	}
	if err := verifyInvariants(item, r); err != nil {
		return err
	}
	return verifyReferenceAssessments(item, r)
}

// The producer binds the candidate inventory; only the model interprets prose.
// Every missing path must receive a grounded classification. Uncertainty stays
// visible in the existing gap/coverage contract, never an implicit exemption.
func verifyReferenceAssessments(item sourceTask, r record) error {
	var ids []string
	encoded := item.Binding.Bindings["reference_candidate_ids"]
	if err := json.Unmarshal([]byte(encoded), &ids); err != nil || ids == nil || len(ids) > 128 {
		return fmt.Errorf("task %s missing or invalid reference candidate binding", item.ID)
	}
	if len(ids) != len(r.References) {
		return fmt.Errorf("task %s omitted or added reference assessments", item.ID)
	}
	seen := make(map[string]bool)
	for i, assessment := range r.References {
		if assessment.ID != ids[i] || seen[assessment.ID] || strings.TrimSpace(assessment.Rationale) == "" || !observed(item, assessment.Evidence) {
			return fmt.Errorf("task %s reference %d is substituted, duplicated, or ungrounded", item.ID, i)
		}
		seen[assessment.ID] = true
		switch assessment.Kind {
		case "required":
			if assessment.FindingIndex == nil || *assessment.FindingIndex < 0 || *assessment.FindingIndex >= len(r.Findings) || assessment.GapIndex != nil {
				return fmt.Errorf("task %s required missing reference %s lacks a finding", item.ID, assessment.ID)
			}
			if !r.Findings[*assessment.FindingIndex].RequiresCritique {
				return fmt.Errorf("task %s required missing reference %s must receive critique", item.ID, assessment.ID)
			}
		case "uncertain":
			if assessment.GapIndex == nil || *assessment.GapIndex < 0 || *assessment.GapIndex >= len(r.Gaps) || assessment.FindingIndex != nil {
				return fmt.Errorf("task %s uncertain reference %s lacks a gap", item.ID, assessment.ID)
			}
		case "historical", "deleted", "example":
			if assessment.FindingIndex != nil || assessment.GapIndex != nil {
				return fmt.Errorf("task %s non-required reference %s has an incompatible finding or gap", item.ID, assessment.ID)
			}
		default:
			return fmt.Errorf("task %s reference %s has an invalid kind", item.ID, assessment.ID)
		}
	}
	return nil
}

func verifyCritique(item sourceTask, r record, tasks map[string]sourceTask, values map[string]payload, records map[string]record, critiqued map[string]bool) (string, bool, error) {
	if r.Kind != "critique" || len(item.EvidenceFrom) != 1 || len(r.InputReview) != 1 {
		return "", false, fmt.Errorf("critic %s must audit exactly one source review", item.ID)
	}
	if err := bind(r.InputReview, item.EvidenceFrom, tasks, values); err != nil {
		return "", false, err
	}
	sourceID := r.InputReview[0].TaskID
	source, ok := records[sourceID]
	if !ok || source.Kind != "review" || len(r.Decisions) != len(source.Findings) || critiqued[sourceID] {
		return "", false, fmt.Errorf("critic %s has incomplete or duplicate finding coverage", item.ID)
	}
	seen := make(map[int]bool)
	unverified := false
	for _, d := range r.Decisions {
		if d.FindingIndex < 0 || d.FindingIndex >= len(source.Findings) || seen[d.FindingIndex] {
			return "", false, fmt.Errorf("critic %s substituted or repeated a finding", item.ID)
		}
		seen[d.FindingIndex] = true
		if d.Verdict == "unverified" {
			unverified = true
		} else if !observed(item, d.Evidence) {
			return "", false, fmt.Errorf("critic %s verdict lacks observed evidence", item.ID)
		}
	}
	reportedGaps := item.Result.Status == "completed_with_gaps"
	if unverified && !reportedGaps {
		return "", false, fmt.Errorf("critic %s concealed unverified verdicts", item.ID)
	}
	// A critic may retain its bound source's coverage limitations even when
	// every finding has a settled verdict. Those gaps remain in the report's
	// source inventory; the conservative envelope must not erase them.
	if reportedGaps && !unverified && len(source.Gaps) == 0 {
		return "", false, fmt.Errorf("critic %s reported gaps without an unverified verdict or a source gap", item.ID)
	}
	return sourceID, reportedGaps, nil
}

func verifyInvariants(item sourceTask, r record) error {
	var acceptedAssessments []invariantAssessment
	if item.Result.Invariants != nil {
		acceptedAssessments = item.Result.Invariants.Assessments
	}
	if len(r.Invariants) != len(acceptedAssessments) {
		return fmt.Errorf("task %s invariant assessment count mismatch: record has %d, runtime verified %d", item.ID, len(r.Invariants), len(acceptedAssessments))
	}
	for i, assessment := range r.Invariants {
		expected := acceptedAssessments[i]
		// Empty and omitted missing_evidence have the same meaning.
		if len(assessment.MissingEvidence) == 0 {
			assessment.MissingEvidence = nil
		}
		if len(expected.MissingEvidence) == 0 {
			expected.MissingEvidence = nil
		}
		if !reflect.DeepEqual(assessment, expected) {
			return fmt.Errorf("task %s substituted an invariant assessment", item.ID)
		}
		if assessment.Status == "unknown" && len(r.Gaps) == 0 {
			return fmt.Errorf("task %s concealed an unknown invariant", item.ID)
		}
	}
	return nil
}

func verifyActionEvidence(current sourceSession, report payload) error {
	seen := make(map[string]bool)
	for _, item := range current.Tasks {
		var source json.RawMessage
		var target json.RawMessage
		switch item.ContractID {
		case "produce-workset":
			target = report.Scope
			if item.Result != nil {
				source = item.Result.RuntimeOutputs["scope"]
			}
		case "verify-targeted-go-tests":
			target = report.Tests
			if item.Result != nil {
				source = item.Result.RuntimeOutputs["targeted_go_tests"]
			}
		default:
			continue
		}
		if seen[item.ContractID] || !item.VerifiedOutputs || item.Result == nil {
			return &sourceReviewError{cause: fmt.Errorf("missing or ambiguous PREPARE evidence for %s", item.ContractID)}
		}
		seen[item.ContractID] = true
		first, err := canonicalJSON(source)
		if err != nil {
			return &sourceReviewError{cause: fmt.Errorf("invalid PREPARE evidence for %s: %w", item.ContractID, err)}
		}
		second, err := canonicalJSON(target)
		if err != nil || !bytes.Equal(first, second) {
			return fmt.Errorf("report substituted %s runtime evidence", item.ContractID)
		}
	}
	if len(seen) != 2 {
		return &sourceReviewError{cause: fmt.Errorf("missing PREPARE evidence for scope or executed tests")}
	}
	return nil
}

func verifyGaps(report payload, records map[string]record, tasks map[string]sourceTask) error {
	seen := make(map[string]bool)
	for _, gap := range report.GapResolutions {
		r, ok := records[gap.TaskID]
		key := fmt.Sprintf("%s/%d", gap.TaskID, gap.Index)
		if !ok || r.Kind != "review" || gap.Index < 0 || gap.Index >= len(r.Gaps) {
			return fmt.Errorf("report substituted an original gap: unknown %s (source has %d gaps)", key, len(r.Gaps))
		}
		if seen[key] {
			return fmt.Errorf("report substituted an original gap: duplicate %s", key)
		}
		seen[key] = true
		if gap.Status == "resolved_cross_workset" {
			resolver, ok := tasks[gap.ResolvingTaskID]
			if !ok || records[gap.ResolvingTaskID].Kind != "review" || gap.ResolvingTaskID == gap.TaskID || !observed(resolver, gap.Evidence) || !observed(tasks[findReportID(tasks)], gap.Evidence) {
				return fmt.Errorf("gap %s lacks observed cross-workset evidence", key)
			}
			if resolver.ContractID == "review-supplement-workset" && (resolver.Binding == nil || resolver.Binding.Bindings["source_task_id"] != gap.TaskID || resolver.Binding.Bindings["source_gap_index"] != strconv.Itoa(gap.Index)) {
				return fmt.Errorf("gap %s uses a supplement bound to another original gap", key)
			}
		}
	}
	var missing []string
	for id, r := range records {
		for index := range r.Gaps {
			key := fmt.Sprintf("%s/%d", id, index)
			if !seen[key] {
				missing = append(missing, key)
			}
		}
	}
	if len(missing) > 0 {
		// Keep the embedded interpreter on exported non-generic sort helpers.
		sort.Strings(missing)
		return fmt.Errorf("report omitted original gaps: missing %v", missing)
	}
	return nil
}

func findReportID(tasks map[string]sourceTask) string {
	for id, item := range tasks {
		if item.ContractID == "synthesize-review" {
			return id
		}
	}
	return ""
}

func runGateAction(ctx context.Context, actionType string, current sourceSession, out io.Writer) error {
	if actionType == "derive_review_report" {
		return deriveReportAction(current, out)
	}
	if actionType == "inventory_review_supplement_requests" {
		requests, err := collectSupplementRequests(current)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"outputs": map[string]any{"review_supplement_requests": requests}})
	}
	if actionType == "inventory_review_handoffs" {
		inventory, err := collectReviewHandoffs(current)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		coverage, err := collectSnapshotCoverage(current)
		if err != nil {
			return err
		}
		outcomes, err := collectSupplementOutcomes(current)
		if err != nil {
			return err
		}
		outputs := map[string]any{"review_handoff_inventory": inventory, "snapshot_coverage": coverage, "supplement_outcomes": outcomes}
		result := map[string]any{"outputs": outputs}
		if coverage.Mode == "repository_snapshot" {
			data, err := json.Marshal(coverage)
			if err != nil {
				return err
			}
			workspace := os.Getenv("HUFU_WORKSPACE")
			if workspace == "" {
				return fmt.Errorf("snapshot coverage requires the action workspace")
			}
			if len(data) > maxSnapshotArtifactBytes {
				return fmt.Errorf("snapshot coverage artifact has %d bytes (maximum %d)", len(data), maxSnapshotArtifactBytes)
			}
			if err := os.WriteFile(filepath.Join(workspace, snapshotCoverageArtifactPath), data, 0o600); err != nil {
				return fmt.Errorf("write snapshot coverage artifact: %w", err)
			}
			summary := summarizeSnapshotCoverage(coverage, data)
			outputs["snapshot_coverage"] = summary
			result["artifacts"] = []snapshotInventoryReference{summary.Inventory}
		}
		return json.NewEncoder(out).Encode(result)
	}
	reportID, reportHash, err := verify(current)
	if err != nil {
		// verify returns this type directly. Native errors.As reflection cannot
		// inspect a target type defined by the trusted-static interpreter.
		if _, sourceFailure := err.(*sourceReviewError); sourceFailure {
			// Rewriting the report cannot change accepted source records. Fail
			// the action instead of taking the report-only verification back-edge.
			return err
		}
		// A completed audit with a rejected report is verification evidence,
		// not a provider outage. The task-result assertion fails and its bounded
		// DAG repair sends this diagnostic back to the report producer.
		return json.NewEncoder(out).Encode(map[string]any{"outputs": map[string]any{"review_report_verification": map[string]any{"passed": false, "diagnostic": err.Error()}}})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	artifact, err := writeVerifiedReport(current, reportID, reportHash)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"outputs":   map[string]any{"review_report_verification": map[string]any{"passed": true, "report_task_id": reportID, "report_payload_sha256": reportHash}},
		"artifacts": []snapshotInventoryReference{artifact},
	})
}

func writeVerifiedReport(current sourceSession, reportID, reportHash string) (snapshotInventoryReference, error) {
	workspace := os.Getenv("HUFU_WORKSPACE")
	if workspace == "" {
		return snapshotInventoryReference{}, fmt.Errorf("verified report requires the action workspace")
	}
	for _, item := range current.Tasks {
		if item.ID != reportID || item.ContractID != "synthesize-review" {
			continue
		}
		data, err := canonicalJSON(item.Result.Payload.Value)
		if err != nil {
			return snapshotInventoryReference{}, err
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		if hash != reportHash {
			return snapshotInventoryReference{}, fmt.Errorf("verified report bytes differ from the accepted payload digest")
		}
		const name = "canonical_report.json"
		if err := os.WriteFile(filepath.Join(workspace, name), data, 0o600); err != nil {
			return snapshotInventoryReference{}, fmt.Errorf("write canonical report: %w", err)
		}
		return snapshotInventoryReference{ID: "sha256-" + hash, Kind: "canonical_review_report", Path: name, SHA256: hash, Bytes: int64(len(data))}, nil
	}
	return snapshotInventoryReference{}, fmt.Errorf("accepted report disappeared after verification")
}

func verifyReportHandoffEvidence(current sourceSession, report payload) error {
	if err := verifyHandoffInventory(current); err != nil {
		return &sourceReviewError{cause: err}
	}
	outcomes, err := collectSupplementOutcomes(current)
	if err != nil {
		return &sourceReviewError{cause: err}
	}
	if len(outcomes) > 0 || len(report.SupplementOutcomes) > 0 {
		expected, err := json.Marshal(outcomes)
		if err != nil {
			return &sourceReviewError{cause: err}
		}
		if !equalJSON(expected, report.SupplementOutcomes) {
			return fmt.Errorf("report substituted native supplement outcomes or limitations")
		}
	}
	return nil
}
