package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/auditverify"
)

var (
	auditWorkspace string
	auditRunID     string
	auditJSON      bool
	auditRecheck   bool
	auditBundle    string

	auditExplainRunID string
	auditExplainJSON  bool

	auditExportRunID        string
	auditExportOutput       string
	auditExportArtifactMode string
)

// auditExitError carries a fixed process exit code across the cobra
// boundary, following the same main.go dispatch convention as
// team.RunOutcomeError (see cmd/hufu/main.go).
type auditExitError struct {
	code int
	msg  string
}

func (e *auditExitError) Error() string        { return e.msg }
func (e *auditExitError) ProcessExitCode() int { return e.code }

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Independently verify a run's completion evidence",
	Long: `hufu audit verifies a run's canonical completion evidence without trusting
worker prose: the event hash chain, evidence manifest, artifact digests,
acceptance state, and task-level provenance are all independently re-checked
against the durable workspace.

Canonical audit verification uses the EventStore hash chain, the canonical
run_finished event, and the EvidenceManifest -- never session.json, report.md,
or the diagnostic logs/audit/ JSONL, which are secondary projections only.`,
	Args: cobra.NoArgs,
}

var auditVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify that a run's persisted outcome is justified by its evidence",
	Args:  cobra.NoArgs,
	RunE:  runAuditVerify,
}

var auditExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export a portable, self-verifying audit bundle for a run",
	Long: `hufu audit export packages a run's canonical event log, evidence manifest,
referenced artifacts, execution receipts, and decision witness into a single
tar file that can be independently verified on another machine with no
access to the original workspace:

    hufu audit export --run <run-id> --output run-audit.tar
    hufu audit verify --bundle run-audit.tar

The export is crash-safe: the archive is built and self-verified in a temp
file first, and is only renamed into place if that self-verification passes.`,
	Args: cobra.NoArgs,
	RunE: runAuditExport,
}

var auditExplainCmd = &cobra.Command{
	Use:   "explain",
	Short: "Explain why a run was certified with its outcome",
	Long: `hufu audit explain answers "why was this run considered complete/failed/
partial?" by formatting the run's persisted decision witness: which
acceptance criteria and tasks justified it, which attempt won each task, and
what evidence backs each. It is fully deterministic and never calls an LLM.`,
	Args: cobra.NoArgs,
	RunE: runAuditExplain,
}

func init() {
	auditCmd.PersistentFlags().StringVarP(&auditWorkspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	auditVerifyCmd.Flags().StringVar(&auditRunID, "run", "", "Run ID to verify (required unless --bundle is set)")
	auditVerifyCmd.Flags().StringVar(&auditBundle, "bundle", "", "Verify a portable audit bundle file instead of a live workspace run")
	auditVerifyCmd.Flags().BoolVar(&auditJSON, "json", false, "Write a single JSON object to stdout; all diagnostics go to stderr")
	auditVerifyCmd.Flags().BoolVar(&auditRecheck, "recheck", false, "Re-execute deterministic, read-only verifiers instead of skipping the recheck dimension")
	auditCmd.AddCommand(auditVerifyCmd)

	auditExplainCmd.Flags().StringVar(&auditExplainRunID, "run", "", "Run ID to explain (required)")
	auditExplainCmd.Flags().BoolVar(&auditExplainJSON, "json", false, "Write a single JSON object to stdout; all diagnostics go to stderr")
	auditCmd.AddCommand(auditExplainCmd)

	auditExportCmd.Flags().StringVar(&auditExportRunID, "run", "", "Run ID to export (required)")
	auditExportCmd.Flags().StringVar(&auditExportOutput, "output", "", "Path to write the audit bundle tar file to (required)")
	auditExportCmd.Flags().StringVar(&auditExportArtifactMode, "artifact-mode", auditverify.ArtifactModeReferenced, "Artifact export mode: referenced (include artifact bytes) or metadata-only (digests only)")
	auditCmd.AddCommand(auditExportCmd)
}

func getAuditWorkspace() string {
	if auditWorkspace != "" {
		return auditWorkspace
	}
	return getWorkspace()
}

func runAuditVerify(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(auditBundle) != "" {
		if strings.TrimSpace(auditRunID) != "" {
			return &auditExitError{code: 2, msg: "hufu audit verify: --run and --bundle are mutually exclusive"}
		}
		result, err := auditverify.VerifyBundle(context.Background(), auditBundle, auditverify.VerifyOptions{Recheck: auditRecheck})
		if err != nil {
			return &auditExitError{code: 2, msg: fmt.Sprintf("hufu audit verify --bundle: %v", err)}
		}
		return finishAuditVerify(result, auditBundle)
	}
	if strings.TrimSpace(auditRunID) == "" {
		return &auditExitError{code: 2, msg: "hufu audit verify: --run is required (or use --bundle)"}
	}

	result, err := auditverify.VerifyWorkspaceRun(context.Background(), getAuditWorkspace(), auditRunID, auditverify.VerifyOptions{Recheck: auditRecheck})
	if err != nil {
		return &auditExitError{code: 2, msg: fmt.Sprintf("hufu audit verify: %v", err)}
	}
	return finishAuditVerify(result, auditRunID)
}

// finishAuditVerify renders result (JSON or text, per --json) and translates
// its verdict into the process exit code convention documented in spec.md
// §16.2 (0 pass, 1 fail, 3 incomplete); label identifies the run/bundle in
// error messages.
func finishAuditVerify(result *auditverify.AuditVerificationResult, label string) error {
	if auditJSON {
		if encErr := json.NewEncoder(os.Stdout).Encode(result); encErr != nil {
			return fmt.Errorf("encode audit result: %w", encErr)
		}
	} else {
		renderAuditVerifyText(os.Stdout, result)
	}

	switch result.Verdict {
	case auditverify.AuditVerdictPass:
		return nil
	case auditverify.AuditVerdictIncomplete:
		return &auditExitError{code: 3, msg: fmt.Sprintf("hufu audit verify: %q is INCOMPLETE", label)}
	default:
		return &auditExitError{code: 1, msg: fmt.Sprintf("hufu audit verify: %q is FAIL", label)}
	}
}

func runAuditExport(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(auditExportRunID) == "" {
		return &auditExitError{code: 2, msg: "hufu audit export: --run is required"}
	}
	if strings.TrimSpace(auditExportOutput) == "" {
		return &auditExitError{code: 2, msg: "hufu audit export: --output is required"}
	}
	opts := auditverify.ExportOptions{ArtifactMode: auditExportArtifactMode}
	if err := auditverify.ExportRun(context.Background(), getAuditWorkspace(), auditExportRunID, auditExportOutput, opts); err != nil {
		return &auditExitError{code: 1, msg: fmt.Sprintf("hufu audit export: %v", err)}
	}
	_, _ = fmt.Fprintf(os.Stdout, "Exported audit bundle for run %q to %s\n", auditExportRunID, auditExportOutput)
	return nil
}

func renderAuditVerifyText(w io.Writer, result *auditverify.AuditVerificationResult) {
	_, _ = fmt.Fprintf(w, "Run: %s\n", result.RunID)
	_, _ = fmt.Fprintf(w, "Expected outcome: %s\n", orUnavailable(string(result.ExpectedOutcome)))
	_, _ = fmt.Fprintf(w, "Derived outcome:  %s\n\n", orUnavailable(string(result.DerivedOutcome)))

	renderAuditDimension(w, "Integrity", result.Integrity)
	renderAuditDimension(w, "Provenance", result.Provenance)
	renderAuditDimension(w, "Evidence", result.Evidence)
	renderAuditDimension(w, "Acceptance", result.Acceptance)
	renderAuditDimension(w, "Completion", result.Completion)
	renderAuditDimension(w, "Recheck", result.Recheck)

	for _, finding := range result.Findings {
		_, _ = fmt.Fprintf(w, "\nfinding [%s/%s]: %s\n", finding.Code, finding.Severity, finding.Message)
	}

	_, _ = fmt.Fprintf(w, "\nAUDIT %s\n", strings.ToUpper(string(result.Verdict)))
}

func renderAuditDimension(w io.Writer, name string, dim auditverify.AuditDimensionResult) {
	status := strings.ToUpper(string(dim.Status))
	if dim.Reason != "" {
		_, _ = fmt.Fprintf(w, "%s\n  %s %s\n", name, status, dim.Reason)
	} else {
		_, _ = fmt.Fprintf(w, "%s\n  %s\n", name, status)
	}
}

func orUnavailable(s string) string {
	if s == "" {
		return "unavailable"
	}
	return s
}

func runAuditExplain(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(auditExplainRunID) == "" {
		return &auditExitError{code: 2, msg: "hufu audit explain: --run is required"}
	}

	result, err := auditverify.ExplainRun(context.Background(), getAuditWorkspace(), auditExplainRunID)
	if err != nil {
		return &auditExitError{code: 2, msg: fmt.Sprintf("hufu audit explain: %v", err)}
	}

	if auditExplainJSON {
		if encErr := json.NewEncoder(os.Stdout).Encode(result); encErr != nil {
			return fmt.Errorf("encode explain result: %w", encErr)
		}
	} else {
		renderAuditExplainText(os.Stdout, result)
	}

	if result.Verification == nil {
		return nil
	}
	switch result.Verification.Verdict {
	case auditverify.AuditVerdictPass:
		return nil
	case auditverify.AuditVerdictIncomplete:
		return &auditExitError{code: 3, msg: fmt.Sprintf("hufu audit explain: run %q audit is INCOMPLETE", auditExplainRunID)}
	default:
		return &auditExitError{code: 1, msg: fmt.Sprintf("hufu audit explain: run %q audit is FAIL", auditExplainRunID)}
	}
}

// renderAuditExplainText formats a persisted DecisionWitness deterministically
// (spec.md §20-21): it never generates new analysis, only presents the facts
// runWorkspaceAudit already verified.
func renderAuditExplainText(w io.Writer, result *auditverify.ExplainResult) {
	if result.Witness == nil {
		if result.Verification != nil {
			_, _ = fmt.Fprintf(w, "Run %s has no canonical decision witness available.\n", result.Verification.RunID)
			renderAuditDimension(w, "Integrity", result.Verification.Integrity)
		}
		return
	}
	witness := result.Witness
	_, _ = fmt.Fprintf(w, "Run %s was certified %s.\n\n", witness.RunID, strings.ToUpper(string(witness.Outcome)))

	for _, cw := range witness.Criteria {
		_, _ = fmt.Fprintf(w, "Criterion: %s\n", cw.CriterionID)
		_, _ = fmt.Fprintf(w, "  status: %s\n", strings.ToUpper(cw.Status))
		if cw.VerificationFingerprint != "" {
			_, _ = fmt.Fprintf(w, "  verification: %s\n", cw.VerificationFingerprint)
		}
		for _, ref := range cw.ReceiptRefs {
			_, _ = fmt.Fprintf(w, "  task: %s\n  attempt: %d\n", ref.TaskID, ref.Attempt)
			if ref.ReceiptHash != "" {
				_, _ = fmt.Fprintf(w, "  receipt: %s\n", ref.ReceiptHash)
			}
		}
		if len(cw.ArtifactIDs) > 0 {
			_, _ = fmt.Fprintln(w, "  evidence:")
			for _, id := range cw.ArtifactIDs {
				_, _ = fmt.Fprintf(w, "    - %s\n", id)
			}
		}
		_, _ = fmt.Fprintln(w)
	}

	_, _ = fmt.Fprintln(w, "CompletionGate:")
	_, _ = fmt.Fprintf(w, "  accepted: %v\n", witness.Gate.Accepted)
	_, _ = fmt.Fprintf(w, "  acceptance: %s\n", strings.ToUpper(string(witness.Gate.AcceptanceState)))
	if witness.Gate.EvidenceManifestStatus != "" {
		_, _ = fmt.Fprintf(w, "  evidence manifest: %s\n", strings.ToUpper(witness.Gate.EvidenceManifestStatus))
	}
	_, _ = fmt.Fprintf(w, "  required tasks: %d/%d done\n", witness.Gate.RequiredTasksDone, witness.Gate.RequiredTasksTotal)
	for _, reason := range witness.Gate.Reasons {
		_, _ = fmt.Fprintf(w, "  reason: %s\n", reason)
	}

	if len(result.AttemptHistory) > 0 {
		_, _ = fmt.Fprintln(w, "\nAttempt history:")
		for _, history := range result.AttemptHistory {
			_, _ = fmt.Fprintf(w, "  Task %s (%s)\n", history.TaskID, history.Status)
			for _, attempt := range history.Attempts {
				marker := ""
				if attempt.Winning {
					marker = " (winning attempt)"
				}
				exitCode := "?"
				if attempt.ExitCode != nil {
					exitCode = fmt.Sprintf("%d", *attempt.ExitCode)
				}
				_, _ = fmt.Fprintf(w, "    Attempt %d: exit=%s producer=%s%s\n", attempt.Attempt, exitCode, attempt.ProducerID, marker)
			}
		}
	}

	_, _ = fmt.Fprintln(w, "\nConclusion:")
	if witness.Gate.Accepted {
		_, _ = fmt.Fprintf(w, "  %s is justified by durable objective evidence.\n", strings.ToUpper(string(witness.Outcome)))
	} else {
		_, _ = fmt.Fprintf(w, "  %s: %s\n", strings.ToUpper(string(witness.Outcome)), auditConclusionReason(result.Verification))
	}
}

func auditConclusionReason(v *auditverify.AuditVerificationResult) string {
	if v == nil {
		return "no verification result available"
	}
	for _, dim := range []auditverify.AuditDimensionResult{v.Integrity, v.Provenance, v.Evidence, v.Acceptance, v.Completion} {
		if dim.Status == auditverify.AuditDimensionFail && dim.Reason != "" {
			return dim.Reason
		}
	}
	return "not certified as completed"
}
