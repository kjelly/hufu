package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

type reportCounts struct {
	Sources    int `json:"sources"`
	Critiques  int `json:"critiques"`
	Findings   int `json:"findings"`
	Gaps       int `json:"gaps"`
	Decisions  int `json:"decisions"`
	Unverified int `json:"unverified"`
}

type reportCompletion struct {
	Status  string       `json:"status"`
	Summary string       `json:"summary"`
	Counts  reportCounts `json:"counts"`
}

func collectReportSources(current sourceSession) (map[string]sourceTask, map[string]payload, map[string]record, error) {
	tasks := make(map[string]sourceTask)
	values := make(map[string]payload)
	records := make(map[string]record)
	for _, item := range current.Tasks {
		switch item.ContractID {
		case "review-primary-workset", "review-documentation-workset", "review-documentation-escalation", "review-supplement-workset", "critic-review":
		default:
			continue
		}
		if _, duplicate := tasks[item.ID]; duplicate {
			return nil, nil, nil, &sourceReviewError{cause: fmt.Errorf("duplicate task ID %s", item.ID)}
		}
		value, err := accepted(item)
		if err != nil {
			return nil, nil, nil, &sourceReviewError{cause: err}
		}
		tasks[item.ID], values[item.ID] = item, value
		var r record
		if err := json.Unmarshal(value.Record, &r); err != nil {
			return nil, nil, nil, &sourceReviewError{cause: fmt.Errorf("task %s has an invalid source record: %w", item.ID, err)}
		}
		records[item.ID] = r
	}
	if len(records) == 0 {
		return nil, nil, nil, &sourceReviewError{cause: fmt.Errorf("review workset results missing")}
	}
	return tasks, values, records, nil
}

// deriveReportCompletion is shared by native assembly and final audit. Its
// inputs are verified source records, test output, and snapshot coverage.
func deriveReportCompletion(tasks map[string]sourceTask, values map[string]payload, records map[string]record, tests json.RawMessage, snapshot snapshotCoverage) (string, reportCompletion, error) {
	critiqued := make(map[string]bool)
	limited := false
	var completion reportCompletion
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var sourceErrors []error
	for _, id := range ids {
		r := records[id]
		item := tasks[id]
		if item.ContractID != "critic-review" {
			completion.Counts.Sources++
			completion.Counts.Findings += len(r.Findings)
			completion.Counts.Gaps += len(r.Gaps)
			if err := verifyReview(item, r); err != nil {
				sourceErrors = append(sourceErrors, err)
			}
			limited = limited || len(r.Gaps) > 0
			continue
		}
		sourceID, critiqueLimited, err := verifyCritique(item, r, tasks, values, records, critiqued)
		if err != nil {
			sourceErrors = append(sourceErrors, err)
			continue
		}
		completion.Counts.Critiques++
		completion.Counts.Decisions += len(r.Decisions)
		for _, decision := range r.Decisions {
			if decision.Verdict == "unverified" {
				completion.Counts.Unverified++
			}
		}
		limited = limited || critiqueLimited
		critiqued[sourceID] = true
	}
	if len(sourceErrors) > 0 {
		return "", reportCompletion{}, &sourceReviewError{cause: errors.Join(sourceErrors...)}
	}
	for _, id := range ids {
		for _, f := range records[id].Findings {
			if findingRequiresCritique(f) && !critiqued[id] {
				return "", reportCompletion{}, &sourceReviewError{cause: fmt.Errorf("task %s has an unassigned required critique", id)}
			}
		}
	}

	testLimited, err := verifyTestEvidence(tests)
	if err != nil {
		return "", reportCompletion{}, &sourceReviewError{cause: fmt.Errorf("targeted test evidence: %w", err)}
	}
	coverage, status := "complete", "success"
	if limited || testLimited || len(snapshot.Uncompleted) > 0 {
		coverage, status = "completed_with_gaps", "completed_with_gaps"
	}
	completion.Status = status
	counts := completion.Counts
	completion.Summary = fmt.Sprintf("Reviewed %d sources with %d independent critiques. Recorded %d findings, %d original gaps and %d critique decisions (%d unverified).",
		counts.Sources, counts.Critiques, counts.Findings, counts.Gaps, counts.Decisions, counts.Unverified)
	return coverage, completion, nil
}

func deriveReportAction(current sourceSession, out io.Writer) error {
	if err := verifyHandoffInventory(current); err != nil {
		return err
	}
	tasks, values, records, err := collectReportSources(current)
	if err != nil {
		return err
	}
	snapshot, err := collectSnapshotCoverage(current)
	if err != nil {
		return err
	}
	coverage, completion, err := deriveReportCompletion(tasks, values, records, reportTestEvidence(current), snapshot)
	if err != nil {
		return err
	}
	findings, err := canonicalDraftFindings(current, tasks, records)
	if err != nil {
		return &sourceReviewError{cause: err}
	}
	return json.NewEncoder(out).Encode(map[string]any{"outputs": map[string]any{
		"review_coverage": coverage, "review_completion": completion, "canonical_findings": findings,
	}})
}

func reportTestEvidence(current sourceSession) json.RawMessage {
	var tests json.RawMessage
	seen := false
	for _, item := range current.Tasks {
		if item.ContractID != "verify-targeted-go-tests" {
			continue
		}
		if seen || !item.VerifiedOutputs || item.Result == nil {
			return nil
		}
		tests = item.Result.RuntimeOutputs["targeted_go_tests"]
		seen = true
	}
	return tests
}

func verifyReportCompletion(current sourceSession, report payload, coverage string, completion reportCompletion, summary string) error {
	seen := false
	for _, item := range current.Tasks {
		if item.ContractID != "derive-review-report" {
			continue
		}
		encodedCoverage, err := json.Marshal(coverage)
		if err != nil {
			return err
		}
		encodedCompletion, err := json.Marshal(completion)
		if err != nil {
			return err
		}
		if seen || !item.VerifiedOutputs || item.Result == nil ||
			!equalJSON(encodedCoverage, item.Result.RuntimeOutputs["review_coverage"]) ||
			!equalJSON(encodedCompletion, item.Result.RuntimeOutputs["review_completion"]) {
			return &sourceReviewError{cause: fmt.Errorf("report completion receipt differs from audited evidence")}
		}
		seen = true
	}
	// Older frozen report contracts did not carry the completion object.
	if seen || report.Completion != nil {
		if !seen || report.Completion == nil || *report.Completion != completion || summary != completion.Summary {
			return fmt.Errorf("report completion or summary differs from audited evidence")
		}
	}
	return nil
}

func verifyCanonicalFindings(current sourceSession, report payload, findings []canonicalFinding) error {
	var expected json.RawMessage
	for _, item := range current.Tasks {
		if item.ContractID != "derive-review-report" || !item.VerifiedOutputs || item.Result == nil {
			continue
		}
		expected = item.Result.RuntimeOutputs["canonical_findings"]
	}
	if len(expected) == 0 && len(report.CanonicalFindings) == 0 {
		// Earlier report contracts carried only the original source records.
		return nil
	}
	data, err := json.Marshal(findings)
	if err != nil {
		return err
	}
	if !equalJSON(data, expected) || !equalJSON(data, report.CanonicalFindings) {
		return fmt.Errorf("canonical finding references or locations differ from audited drafts")
	}
	return nil
}
