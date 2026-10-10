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
	"strings"
)

type finding struct {
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
	Scope          json.RawMessage `json:"scope"`
	Tests          json.RawMessage `json:"test_verification"`
	Record         json.RawMessage `json:"record"`
	InputReview    []inputReview   `json:"input_review"`
	Coverage       string          `json:"coverage"`
	GapResolutions []gapResolution `json:"gap_resolutions"`
}

type task struct {
	ID           string
	Status       string
	ContractID   string   `json:"contract_id"`
	EvidenceFrom []string `json:"evidence_from"`
	SnapshotID   string   `json:"run_input_snapshot_id"`
	Binding      *struct {
		Bindings map[string]string `json:"bindings"`
	} `json:"workset_binding"`
	Receipt *struct {
		RunID            string `json:"run_id"`
		TaskID           string `json:"task_id"`
		Attempt          int    `json:"attempt"`
		ProducerID       string `json:"producer_id"`
		ModelExecutionID string `json:"model_execution_id"`
		TranscriptRef    string `json:"transcript_ref"`
	} `json:"execution_receipt"`
	VerifyResult *struct {
		ExitCode    *int   `json:"exit_code"`
		Fingerprint string `json:"fingerprint"`
		TimedOut    bool   `json:"timed_out"`
	}
	Result *struct {
		RuntimeOutputs map[string]json.RawMessage `json:"runtime_outputs"`
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

type session struct {
	RuntimeWorkspace string `json:"runtime_workspace"`
	SnapshotID       string `json:"active_run_input_snapshot_id"`
	Tasks            []task `json:"tasks"`
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
	if envelope.Type != "verify_review_report" && envelope.Type != "inventory_review_handoffs" {
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
	root, runtimeRoot, err := checkpointRoot(os.Getenv("HUFU_WORKSPACE"), os.Getenv("HUFU_RUN_ID"), os.Getenv("HUFU_ACTION_INVOCATION_ID"))
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(root, "session.json"))
	if err != nil {
		return fmt.Errorf("read review checkpoint: %w", err)
	}
	var current session
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	// The runtime-owned staging path and current bound occurrence select the
	// checkpoint, never a model-selected session path.
	if current.RuntimeWorkspace != "" && current.RuntimeWorkspace != runtimeRoot {
		return fmt.Errorf("runtime workspace does not match checkpoint")
	}
	matched := false
	for _, item := range current.Tasks {
		isGate := (envelope.Type == "verify_review_report" && item.ContractID == "verify-review-report") ||
			(envelope.Type == "inventory_review_handoffs" && item.ContractID == "inventory-review-handoffs")
		if item.ID == os.Getenv("HUFU_TASK_ID") && isGate && item.Status == "in_progress" && current.SnapshotID != "" && item.SnapshotID == current.SnapshotID {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("current review gate occurrence is absent from checkpoint")
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
	if envelope.Type == "inventory_review_handoffs" {
		inventory, err := collectReviewHandoffs(current, os.Getenv("HUFU_RUN_ID"))
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"outputs": map[string]any{"review_handoff_inventory": inventory}})
	}
	reportID, reportHash, err := verify(current, os.Getenv("HUFU_RUN_ID"))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"outputs": map[string]any{"review_report_verification": map[string]any{"passed": true, "report_task_id": reportID, "report_payload_sha256": reportHash}}})
}

// This adapter intentionally uses the runtime action staging layout. A layout
// change fails closed; it never searches for or guesses another session.
func checkpointRoot(workspace, runID, actionID string) (string, string, error) {
	if !filepath.IsAbs(workspace) || runID == "" || actionID == "" {
		return "", "", fmt.Errorf("missing runtime action identity")
	}
	root := filepath.Clean(workspace)
	for depth := 0; depth < 5; depth++ {
		root = filepath.Dir(root)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if filepath.Join(runtimeRoot, "runs", runID, "actions", actionID) != workspace {
		return "", "", fmt.Errorf("unsupported action workspace layout")
	}
	return root, runtimeRoot, nil
}

func accepted(item task, snapshotID, runID string) (payload, error) {
	var value payload
	// Run-input snapshot metadata belongs to materialized native actions. LLM
	// handoffs bind to the current run/task receipt and immutable workset instead.
	// Reject conflicting snapshot metadata when present, without inventing it on
	// ordinary workers, critics, or synthesis tasks.
	if item.Status != "done" || item.Result == nil || (item.Result.Status != "success" && item.Result.Status != "completed_with_gaps") || item.Result.Payload == nil || item.Receipt == nil || item.Receipt.RunID != runID || item.Receipt.TaskID != item.ID || item.Receipt.Attempt <= 0 || item.Receipt.ProducerID == "" || item.Receipt.ModelExecutionID == "" || item.Receipt.TranscriptRef == "" || (item.SnapshotID != "" && item.SnapshotID != snapshotID) || item.VerifyResult == nil || item.VerifyResult.ExitCode == nil || *item.VerifyResult.ExitCode != 0 || item.VerifyResult.TimedOut || item.VerifyResult.Fingerprint == "" {
		return value, fmt.Errorf("task %s lacks a current accepted, verified handoff", item.ID)
	}
	canonical, err := canonicalJSON(item.Result.Payload.Value)
	if err != nil {
		return value, err
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != item.Result.Payload.Hash {
		return value, fmt.Errorf("task %s payload hash mismatch", item.ID)
	}
	if err := json.Unmarshal(canonical, &value); err != nil {
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

func bind(inputs []inputReview, ids []string, tasks map[string]task, values map[string]payload) error {
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

func observed(item task, evidence []string) bool {
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

func verify(current session, runID string) (string, string, error) {
	tasks := make(map[string]task)
	values := make(map[string]payload)
	records := make(map[string]record)
	var reportID string
	for _, item := range current.Tasks {
		switch item.ContractID {
		case "review-primary-workset", "review-documentation-workset", "review-documentation-escalation", "critic-review", "synthesize-review":
		default:
			continue
		}
		if _, duplicate := tasks[item.ID]; duplicate {
			return "", "", fmt.Errorf("duplicate task ID")
		}
		value, err := accepted(item, current.SnapshotID, runID)
		if err != nil {
			return "", "", err
		}
		tasks[item.ID], values[item.ID] = item, value
		if item.ContractID == "synthesize-review" {
			if reportID != "" {
				return "", "", fmt.Errorf("ambiguous report tasks")
			}
			reportID = item.ID
			continue
		}
		var r record
		if err := json.Unmarshal(value.Record, &r); err != nil {
			return "", "", err
		}
		records[item.ID] = r
	}
	if reportID == "" || len(records) == 0 {
		return "", "", fmt.Errorf("review report or workset results missing")
	}
	critiqued := make(map[string]bool)
	limited := false
	for id, r := range records {
		item := tasks[id]
		if item.ContractID != "critic-review" {
			if err := verifyReview(item, r); err != nil {
				return "", "", err
			}
			limited = limited || len(r.Gaps) > 0
			continue
		}
		sourceID, unverified, err := verifyCritique(item, r, tasks, values, records, critiqued)
		if err != nil {
			return "", "", err
		}
		limited = limited || unverified
		critiqued[sourceID] = true
	}
	for id, r := range records {
		for _, f := range r.Findings {
			if f.RequiresCritique && !critiqued[id] {
				return "", "", fmt.Errorf("task %s has an unassigned required critique", id)
			}
		}
	}
	report := values[reportID]
	if err := verifyActionEvidence(current, report, runID); err != nil {
		return "", "", err
	}
	if err := verifyHandoffInventory(current, runID); err != nil {
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
	coverage, resultStatus := "complete", "success"
	if limited {
		coverage, resultStatus = "completed_with_gaps", "completed_with_gaps"
	}
	if report.Coverage != coverage || tasks[reportID].Result.Status != resultStatus {
		return "", "", fmt.Errorf("report overstated coverage")
	}
	if err := verifyGaps(report, records, tasks); err != nil {
		return "", "", err
	}
	return reportID, tasks[reportID].Result.Payload.Hash, nil
}

func verifyReview(item task, r record) error {
	if r.Kind != "review" || item.Binding == nil || r.Revision != item.Binding.Bindings["review_revision"] || r.Key != item.Binding.Bindings["key"] || r.Lens != item.Binding.Bindings["lens"] {
		return fmt.Errorf("task %s changed its workset identity", item.ID)
	}
	if len(r.Findings) != len(item.Result.Findings) {
		return fmt.Errorf("task %s omitted typed findings", item.ID)
	}
	for i, f := range r.Findings {
		if f.Summary != item.Result.Findings[i].Summary || f.Detail != item.Result.Findings[i].Detail || f.Severity != item.Result.Findings[i].Severity || !observed(item, f.Evidence) || ((f.Severity == "error" || f.Severity == "warning") && !f.RequiresCritique) {
			return fmt.Errorf("task %s finding %d is inconsistent or ungrounded", item.ID, i)
		}
	}
	if (len(r.Gaps) > 0) != (item.Result.Status == "completed_with_gaps") {
		return fmt.Errorf("task %s concealed its coverage status", item.ID)
	}
	if err := verifyInvariants(item, r); err != nil {
		return err
	}
	return nil
}

func verifyCritique(item task, r record, tasks map[string]task, values map[string]payload, records map[string]record, critiqued map[string]bool) (string, bool, error) {
	if r.Kind != "critique" || len(item.EvidenceFrom) != 1 || len(r.InputReview) != 1 {
		return "", false, fmt.Errorf("critic %s must audit exactly one source review", item.ID)
	}
	if err := bind(r.InputReview, item.EvidenceFrom, tasks, values); err != nil {
		return "", false, err
	}
	sourceID := r.InputReview[0].TaskID
	source, ok := records[sourceID]
	if !ok || source.Kind != "review" || len(source.Findings) == 0 || len(r.Decisions) != len(source.Findings) || critiqued[sourceID] {
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
	if unverified != (item.Result.Status == "completed_with_gaps") {
		return "", false, fmt.Errorf("critic %s concealed unverified verdicts", item.ID)
	}
	return sourceID, unverified, nil
}

func verifyInvariants(item task, r record) error {
	var acceptedAssessments []invariantAssessment
	if item.Result.Invariants != nil {
		acceptedAssessments = item.Result.Invariants.Assessments
	}
	if len(r.Invariants) != len(acceptedAssessments) {
		return fmt.Errorf("task %s omitted invariant assessments", item.ID)
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

func verifyActionEvidence(current session, report payload, runID string) error {
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
		if seen[item.ContractID] || item.Status != "done" || item.Result == nil || item.Result.Status != "success" || item.Receipt == nil || item.Receipt.RunID != runID || item.SnapshotID != current.SnapshotID {
			return fmt.Errorf("missing or ambiguous PREPARE evidence")
		}
		seen[item.ContractID] = true
		first, err := canonicalJSON(source)
		if err != nil {
			return err
		}
		second, err := canonicalJSON(target)
		if err != nil || !bytes.Equal(first, second) {
			return fmt.Errorf("report substituted %s runtime evidence", item.ContractID)
		}
	}
	if len(seen) != 2 {
		return fmt.Errorf("report lacks PREPARE scope or executed-test evidence")
	}
	return nil
}

func verifyGaps(report payload, records map[string]record, tasks map[string]task) error {
	seen := make(map[string]bool)
	count := 0
	for _, r := range records {
		count += len(r.Gaps)
	}
	if len(report.GapResolutions) != count {
		return fmt.Errorf("report omitted original gaps")
	}
	for _, gap := range report.GapResolutions {
		r, ok := records[gap.TaskID]
		key := fmt.Sprintf("%s/%d", gap.TaskID, gap.Index)
		if !ok || gap.Index < 0 || gap.Index >= len(r.Gaps) || seen[key] {
			return fmt.Errorf("report substituted an original gap")
		}
		seen[key] = true
		if gap.Status == "resolved_cross_workset" {
			resolver, ok := tasks[gap.ResolvingTaskID]
			if !ok || records[gap.ResolvingTaskID].Kind != "review" || gap.ResolvingTaskID == gap.TaskID || !observed(resolver, gap.Evidence) || !observed(tasks[findReportID(tasks)], gap.Evidence) {
				return fmt.Errorf("gap %s lacks observed cross-workset evidence", key)
			}
		}
	}
	return nil
}

func findReportID(tasks map[string]task) string {
	for id, item := range tasks {
		if item.ContractID == "synthesize-review" {
			return id
		}
	}
	return ""
}
