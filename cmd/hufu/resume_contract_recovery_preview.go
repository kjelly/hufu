package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

func writeContractRecoveryPreview(w io.Writer, plan *team.ContractRecoveryPlan, format string) error {
	if format == "json" {
		data, err := json.Marshal(plan, json.Deterministic(true))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(data))
		return err
	}
	var out strings.Builder
	failed, downstream := 0, 0
	for _, task := range plan.Tasks {
		if task.Reason == "original_failure" && task.Action == "retry" {
			failed++
		}
		if task.Reason == "upstream_changed" {
			downstream++
		}
	}
	label := "Contract recovery"
	if plan.WorkRetry != nil {
		label = "Read-only work recovery"
	}
	fmt.Fprintf(&out, "%s preview: retain %d; retry %d (original failures: %d; affected downstream: %d)\n", label, plan.Retained, plan.Retried, failed, downstream)
	if plan.Repaired > 0 {
		fmt.Fprintf(&out, "Result-only repairs: %d; original worker tools will not be replayed; collector turns require a separate child resume --repair-result.\n", plan.Repaired)
	}
	fmt.Fprintf(&out, "Source branch: %q; event: %s\n", plan.SourceBranch, plan.SourceHead)
	if retry := plan.WorkRetry; retry != nil {
		fmt.Fprintf(&out, "Read-only work retry: task %q; missing required decisions: %d; old work and decisions remain in the source branch.\n", retry.TaskID, len(retry.MissingDecisions))
	}
	if upgrade := plan.InputPolicyUpgrade; upgrade != nil {
		fmt.Fprintf(&out, "Input policy upgrade: %s -> %s; frozen snapshot: %q (sha256 %s)\n", upgrade.OriginalSHA256, upgrade.AcceptedSHA256, upgrade.SnapshotID, upgrade.SnapshotSHA256)
	}
	if plan.MetadataOnly {
		out.WriteString("Metadata-only recovery: project files remain shared.\n")
	}
	for _, task := range plan.Tasks {
		fmt.Fprintf(&out, "  Task %q: %s — %s (source status: %s)", task.ID, task.Action, recoveryReasonText(task.Reason), task.SourceStatus)
		if legacy := task.LegacySubmission; legacy != nil {
			fmt.Fprintf(&out, "; transport: tool -> runtime; legacy submissions spent: %d; no turns granted by publication", legacy.AttemptsUsed)
		}
		if len(task.AffectedBy) > 0 {
			fmt.Fprintf(&out, "; affected by: %s", strings.Join(task.AffectedBy, ", "))
		}
		out.WriteByte('\n')
	}
	out.WriteString("Contract changes:\n")
	if len(plan.ContractChanges) == 0 {
		out.WriteString("  none\n")
	}
	for _, change := range plan.ContractChanges {
		fmt.Fprintf(&out, "  %q: %q (sha256 %s; required=%t) -> %q (sha256 %s; required=%t)\n", change.Owner, change.Original.ID, change.Original.SchemaSHA256, change.Original.RequireStructured, change.Accepted.ID, change.Accepted.SchemaSHA256, change.Accepted.RequireStructured)
	}
	fmt.Fprintf(&out, "Resume phase: %s\n", plan.ResumePhase)
	out.WriteString("Task verification to rerun:\n")
	if len(plan.ReopenedVerification) == 0 {
		out.WriteString("  none\n")
	}
	for _, id := range plan.ReopenedVerification {
		fmt.Fprintf(&out, "  Task %q\n", id)
	}
	out.WriteString("Acceptance checks to reopen:\n")
	if len(plan.ReopenedAcceptance) == 0 {
		out.WriteString("  none configured\n")
	}
	for _, check := range plan.ReopenedAcceptance {
		fmt.Fprintf(&out, "  %q\n", check)
	}
	fmt.Fprintf(&out, "Plan hash: %s\nPreview only; no branch created and no tasks executed.\n", plan.SHA256)
	_, err := io.WriteString(w, out.String())
	return err
}

func recoveryReasonText(reason string) string {
	switch reason {
	case "legacy_submission":
		return "rejected legacy submit_result candidate preserved for bounded collection"
	case "original_failure":
		return "original failed or blocked task"
	case "unfinished_task":
		return "original unfinished task"
	case "upstream_changed":
		return "upstream task retried or its result contract changed"
	case "result_projection_changed":
		return "accepted result does not match the new envelope projection"
	case "result_schema_invalid":
		return "accepted payload does not satisfy the new result schema"
	case "result_contract_binding_changed":
		return "result contract binding was added or removed"
	case "result_revalidated":
		return "original payload revalidated against the new contract"
	case "accepted_result":
		return "accepted result and contract remain valid"
	default:
		return "reason unavailable in this plan version"
	}
}
