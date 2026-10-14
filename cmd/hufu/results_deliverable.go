package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/utils"
)

// CLI output owns its presentation budget independently of model context.
// Artifact references are printed as metadata and never opened.
func formatHistoricalDeliverable(row inspectpkg.HistoricalResult) string {
	result := row.TypedResult
	if result == nil || row.Kind == "run_response" {
		content := firstNonEmpty(row.Output, row.Summary, "(result content unavailable)")
		return utils.SanitizeTerminalText(utils.RedactSecrets(content))
	}
	var out strings.Builder
	if result.Summary != "" {
		out.WriteString("Summary: " + result.Summary + "\n")
	}
	if result.Details != "" {
		out.WriteString("Detailed Deliverable:\n" + result.Details + "\n")
	} else if result.StructuredPayload == nil && row.Output != "" && row.Output != result.Summary {
		// Older/free-text results may have only a bounded typed summary.
		// Preserve their saved deliverable without following transcript refs.
		out.WriteString("Saved output:\n" + row.Output + "\n")
	}
	for _, finding := range result.Findings {
		_, _ = fmt.Fprintf(&out, "Finding [%s/%s]: %s\n", finding.Severity, finding.Category, finding.Summary)
		if finding.Detail != "" {
			out.WriteString(finding.Detail + "\n")
		}
	}
	for _, decision := range result.Decisions {
		_, _ = fmt.Fprintf(&out, "Decision: %s — %s\n", decision.Topic, decision.Choice)
		if decision.Reason != "" {
			out.WriteString("Reason: " + decision.Reason + "\n")
		}
	}
	for _, artifact := range result.Artifacts {
		_, _ = fmt.Fprintf(&out, "Artifact: %s · %s · %s\n", artifact.ID, artifact.Path, artifact.Description)
	}
	if ref := result.RawOutputRef; ref != nil {
		_, _ = fmt.Fprintf(&out, "Transcript reference: %s · %s\n", ref.ID, ref.Path)
	}
	for _, command := range result.Commands {
		_, _ = fmt.Fprintf(&out, "Command: %s · exit=%d\n%s\n", command.Command, command.ExitCode, command.Output)
	}
	if len(result.Verification) > 0 {
		out.WriteString(formatResultsJSONSection("Verification", result.Verification))
	}
	if result.InvariantVerification != nil {
		out.WriteString(formatResultsJSONSection("Invariant verification", result.InvariantVerification))
	}
	for _, risk := range result.Risks {
		_, _ = fmt.Fprintf(&out, "Risk: %s\nImpact: %s\nMitigation: %s\n", risk.Description, risk.Impact, risk.Mitigation)
	}
	for _, question := range result.OpenQuestions {
		out.WriteString("Open question: " + question + "\n")
	}
	for _, file := range result.FilesModified {
		_, _ = fmt.Fprintf(&out, "Modified: %s · %s\n", file.Path, file.Purpose)
	}
	if payload := result.StructuredPayload; payload != nil {
		out.WriteString("Structured payload (SHA-256: " + payload.SHA256 + "):\n")
		if len(payload.Value) <= 2048 {
			out.Write(payload.Value)
			out.WriteString("\n")
		} else {
			_, _ = fmt.Fprintf(&out, "%d bytes; use show --payload for the complete JSON.\n", len(payload.Value))
		}
	}
	if out.Len() == 0 {
		out.WriteString(firstNonEmpty(row.Output, row.Summary, "(result content unavailable)"))
	}
	return utils.SanitizeTerminalText(utils.RedactSecrets(out.String()))
}

func formatResultsJSONSection(label string, value any) string {
	encoded, err := json.Marshal(value, jsontext.WithIndent("  "))
	if err != nil {
		return label + ": unavailable; use --output json for the result envelope.\n"
	}
	return label + ":\n" + string(encoded) + "\n"
}
