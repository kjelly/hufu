package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// These DTOs implement the provider-neutral failure wire contract. Review
// vocabulary and the explanation remain owned by this action, not Hufu core.
type reviewDiagnosticFact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type reviewBudgetDiagnosis struct {
	Version     int                    `json:"version"`
	Code        string                 `json:"code"`
	Summary     string                 `json:"summary"`
	CauseStatus string                 `json:"cause_status"`
	Cause       string                 `json:"cause"`
	Evidence    []reviewDiagnosticFact `json:"evidence"`
	Suggestions []string               `json:"suggestions"`
}

type reviewBudgetError struct {
	message   string
	diagnosis reviewBudgetDiagnosis
}

func (e *reviewBudgetError) Error() string { return e.message }

// Report only typed, measured failures. Unknown errors retain the ordinary
// stderr diagnostic; prose is never parsed to manufacture an explanation.
func writeReviewFailure(out io.Writer, err error) error {
	failure := findReviewBudgetError(err)
	if failure == nil {
		return err
	}
	envelope := struct {
		Error struct {
			Class     string                `json:"failure_class"`
			Message   string                `json:"message"`
			Diagnosis reviewBudgetDiagnosis `json:"diagnosis"`
		} `json:"error"`
	}{}
	envelope.Error.Class = "execution"
	envelope.Error.Message = failure.diagnosis.Summary
	envelope.Error.Diagnosis = failure.diagnosis
	if encodeErr := json.NewEncoder(out).Encode(envelope); encodeErr != nil {
		return fmt.Errorf("write review failure: %w (original: %v)", encodeErr, err)
	}
	return err
}

// Traverse both wrapped and joined causes without native errors.As reflection;
// interpreted error types do not implement native reflection's error target.
func findReviewBudgetError(err error) *reviewBudgetError {
	if err == nil {
		return nil
	}
	if typed, ok := err.(*reviewBudgetError); ok {
		return typed
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if found := findReviewBudgetError(cause); found != nil {
				return found
			}
		}
	} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return findReviewBudgetError(wrapped.Unwrap())
	}
	return nil
}

func oversizedReviewInputError(current *batch, source, inventory []byte, tokens int, config Config) error {
	var evidence evidenceInventory
	// Inventory was created by bundleDataWithScopes in this process.
	if err := json.Unmarshal(inventory, &evidence); err != nil {
		return fmt.Errorf("decode source budget inventory: %w", err)
	}
	primaryBytes, relatedBytes := int64(0), int64(0)
	relatedPaths := 0
	for _, row := range evidence.Sources {
		content := int64(0)
		for _, span := range row.AvailableSpans {
			content += span.End - span.Start
		}
		if row.Role == "primary" {
			primaryBytes += content
		} else {
			relatedBytes += content
			relatedPaths++
		}
	}
	fixedSourceBytes := int64(len(source)) - primaryBytes
	summary := "Cannot prepare review worksets: an indivisible evidence item exceeds its input budget."
	cause := "The remaining source interval and diff fragment cannot be split further within the configured evidence budgets."
	if fixedSourceBytes > int64(config.MaxSourceSnapshotBytes) {
		summary = "Cannot prepare review worksets: fixed dependency evidence exceeds the per-item source budget."
		cause = "Related-source content and section metadata already exceed the source snapshot budget without primary content. Splitting primary source or diff hunks cannot remove this fixed per-item overhead. Related paths are identifier candidates bound to package/import dependencies, with conservative fallback when module ownership is unknown. The inventory retains every selected source identity even when content is omitted."
	} else if estimateReviewInputTokens(len(inventory)) > config.MaxReviewInputTokens {
		summary = "Cannot prepare review worksets: the dependency inventory alone exceeds the input budget."
		cause = "The dependency inventory alone exceeds the review input token estimate budget. Splitting primary source or diff hunks cannot reduce the repeated inventory."
	}
	fact := func(name string, value int64) reviewDiagnosticFact {
		return reviewDiagnosticFact{Name: name, Value: strconv.FormatInt(value, 10)}
	}
	primaryPath := "(none)"
	if len(current.paths) > 0 {
		primaryPath = current.paths[0]
	}
	diagnosis := reviewBudgetDiagnosis{Version: 1, Code: "workset_item_too_large", Summary: summary, CauseStatus: "confirmed", Cause: cause,
		Evidence: []reviewDiagnosticFact{
			{Name: "primary_path", Value: primaryPath},
			fact("source_snapshot_bytes", int64(len(source))), fact("max_source_snapshot_bytes", int64(config.MaxSourceSnapshotBytes)),
			fact("primary_content_bytes", primaryBytes), fact("related_content_bytes", relatedBytes), fact("related_paths", int64(relatedPaths)),
			fact("source_metadata_bytes", int64(len(source))-primaryBytes-relatedBytes), fact("fixed_source_bytes", fixedSourceBytes), fact("inventory_bytes", int64(len(inventory))),
			fact("estimated_input_units", int64(tokens)), fact("max_input_units", int64(config.MaxReviewInputTokens)),
		}, Suggestions: []string{
			"Review team maintainers should refine dependency selection or partition metadata while retaining explicit evidence coverage and source identities.",
			"Repeating the same action or changing the worker model cannot reduce this deterministic evidence size. Any scope or budget change needs an explicit review decision.",
		},
	}
	return &reviewBudgetError{message: fmt.Sprintf("workset_item_too_large: paths=%v source_snapshot_bytes=%d estimated_review_input_tokens=%d; configured caps: max_source_snapshot_bytes=%d max_review_input_tokens=%d; remediation: narrow the request or provide an explicit input budget sufficient for one source line and dependency inventory", current.paths, len(source), tokens, config.MaxSourceSnapshotBytes, config.MaxReviewInputTokens), diagnosis: diagnosis}
}

// Keep interpreted causes in the interpreted error tree. Native errors.Join
// erases their concrete interpreter identity when they cross back into Yaegi.
type reviewJoinedError struct{ causes []error }

func (e *reviewJoinedError) Error() string {
	var messages []string
	for _, cause := range e.causes {
		messages = append(messages, cause.Error())
	}
	return strings.Join(messages, "\n")
}
func (e *reviewJoinedError) Unwrap() []error { return e.causes }
func joinReviewErrors(first, second error) error {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return &reviewJoinedError{causes: []error{first, second}}
}
