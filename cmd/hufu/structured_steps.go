package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

func renderStructuredStepsText(writer io.Writer, steps []team.StructuredStepMetadata) error {
	for _, step := range steps {
		if _, err := fmt.Fprintf(writer, "Step: %s  receipt=%s  attempt=%d  execution_kind=%s  proposed_model=%s  exit_code=%s\n",
			safeOverviewValue(valueOrUnavailable(step.StepID)), safeOverviewValue(valueOrUnavailable(step.ReceiptID)), step.Attempt,
			safeOverviewValue(step.ExecutionKind), safeOverviewValue(valueOrUnavailable(step.ProposedModel)), optionalInt(step.ExitCode)); err != nil {
			return err
		}
		if step.UnavailableReason != "" {
			if _, err := fmt.Fprintf(writer, "  Attribution: %s\n", safeOverviewValue(step.UnavailableReason)); err != nil {
				return err
			}
		}
		for _, actual := range step.Invocations {
			if _, err := fmt.Fprintf(writer, "  Invocation: %s  model=%s  provider=%s  execution_target=%s  reason=%s\n",
				safeOverviewValue(actual.ID), safeOverviewValue(actual.Model), safeOverviewValue(actual.Provider),
				safeOverviewValue(valueOrUnavailable(actual.ExecutionTarget)), safeOverviewValue(valueOrUnavailable(actual.UnavailableReason))); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(writer, "  Source: event=%s hash=%s receipt_sha256=%s\n",
			safeOverviewValue(valueOrUnavailable(step.SourceEventID)), safeOverviewValue(valueOrUnavailable(step.SourceHash)), safeOverviewValue(valueOrUnavailable(step.ReceiptSHA256))); err != nil {
			return err
		}
	}
	return nil
}

func writeStructuredStepReport(builder *strings.Builder, steps []team.StructuredStepMetadata) {
	if len(steps) == 0 {
		return
	}
	builder.WriteString("## Structured Step Execution\n\nProposed models describe selection only. Execution kind describes the direct step; tool-internal model calls retain their task-level accounting.\n\n")
	builder.WriteString("| Task / step | Attempt | Kind | Proposed model | Observed invocations | Unavailable reason |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, step := range steps {
		actual := []string{}
		for _, invocation := range step.Invocations {
			actual = append(actual, fmt.Sprintf("%s: %s / %s; target=%s", reportSafeMetadata(invocation.ID, 160),
				reportSafeMetadata(invocation.Provider, 120), reportSafeMetadata(invocation.Model, 160), reportSafeMetadata(valueOrUnavailable(invocation.ExecutionTarget), 160)))
		}
		observed := "none for direct step"
		if step.ExecutionKind == "unknown" {
			observed = "unavailable"
		}
		if len(actual) > 0 {
			observed = strings.Join(actual, "<br>")
		}
		fmt.Fprintf(builder, "| %s / %s | %d | %s | %s | %s | %s |\n", reportSafeMetadata(step.TaskID, 120),
			reportSafeMetadata(valueOrUnavailable(step.StepID), 120), step.Attempt, reportSafeMetadata(step.ExecutionKind, 40),
			reportSafeMetadata(valueOrUnavailable(step.ProposedModel), 160), observed, reportSafeMetadata(valueOrUnavailable(step.UnavailableReason), 160))
	}
	builder.WriteString("\n")
	for _, step := range steps {
		if step.SourceEventID != "" {
			fmt.Fprintf(builder, "\nReceipt `%s`: source `%s`, hash `%s`, payload `%s`.\n\n", reportSafeMetadata(step.ReceiptID, 160),
				reportSafeMetadata(step.SourceEventID, 160), reportSafeMetadata(step.SourceHash, 160), reportSafeMetadata(step.ReceiptSHA256, 160))
		}
	}
	builder.WriteString("\n")
}
