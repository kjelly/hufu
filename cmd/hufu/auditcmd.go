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

func init() {
	auditCmd.PersistentFlags().StringVarP(&auditWorkspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	auditVerifyCmd.Flags().StringVar(&auditRunID, "run", "", "Run ID to verify (required)")
	auditVerifyCmd.Flags().BoolVar(&auditJSON, "json", false, "Write a single JSON object to stdout; all diagnostics go to stderr")
	auditVerifyCmd.Flags().BoolVar(&auditRecheck, "recheck", false, "Re-execute deterministic, read-only verifiers instead of skipping the recheck dimension")
	auditCmd.AddCommand(auditVerifyCmd)
}

func getAuditWorkspace() string {
	if auditWorkspace != "" {
		return auditWorkspace
	}
	return getWorkspace()
}

func runAuditVerify(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(auditRunID) == "" {
		return &auditExitError{code: 2, msg: "hufu audit verify: --run is required"}
	}

	result, err := auditverify.VerifyWorkspaceRun(context.Background(), getAuditWorkspace(), auditRunID, auditverify.VerifyOptions{Recheck: auditRecheck})
	if err != nil {
		return &auditExitError{code: 2, msg: fmt.Sprintf("hufu audit verify: %v", err)}
	}

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
		return &auditExitError{code: 3, msg: fmt.Sprintf("hufu audit verify: run %q is INCOMPLETE", auditRunID)}
	default:
		return &auditExitError{code: 1, msg: fmt.Sprintf("hufu audit verify: run %q is FAIL", auditRunID)}
	}
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
