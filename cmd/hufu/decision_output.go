package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

type decisionOutputExitError struct {
	code int
	err  error
}

func (e *decisionOutputExitError) Error() string        { return e.err.Error() }
func (e *decisionOutputExitError) Unwrap() error        { return e.err }
func (e *decisionOutputExitError) ProcessExitCode() int { return e.code }

func buildDecisionOutput(ctx context.Context, loadedTeams map[string]*teamContext, preview bool, elapsed time.Duration) (team.DecisionOutputV1, error) {
	kind := "decision_run_result"
	if preview {
		kind = "decision_run_preview"
	}
	out := team.NewDecisionOutputV1(kind)
	if preview {
		out.Outcome, out.ExitCode = "preview", 0
	}
	names := make([]string, 0, len(loadedTeams))
	for name := range loadedTeams {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) != 1 {
		return out, fmt.Errorf("decision output requires exactly one owner team")
	}
	tc := loadedTeams[names[0]]
	if tc == nil || tc.coordinator == nil || tc.session == nil {
		return out, fmt.Errorf("decision owner team is unavailable")
	}
	teamID, err := team.DecisionTeamID(names[0])
	if err != nil {
		return out, err
	}
	definitionDigest, err := decisionOutputTeamDigest(tc)
	if err != nil {
		return out, err
	}
	out.Team = &team.DecisionOutputTeamV1{ID: teamID, DefinitionDigest: definitionDigest}
	bundle, err := agent.ResolveBuiltInDecisionProfileBundle(opts.primaryDecisionProfile)
	if err != nil {
		return out, fmt.Errorf("resolve primary decision profile: %w", err)
	}
	policyDigest, err := agent.DecisionPolicyDigest(bundle.Policy)
	if err != nil {
		return out, err
	}
	out.Profile = &team.DecisionOutputProfileV1{
		Requested: opts.primaryDecisionProfileRequested, ResolvedRef: bundle.Ref, Origin: opts.primaryDecisionProfileOrigin,
		Version: "v2", PolicyDigest: policyDigest, BundleDigest: bundle.BundleDigest,
	}
	budget := tc.coordinator.Budget()
	if budget != nil {
		used, reserved := budget.TokensUsed(), budget.Reserved()
		if used > 0 {
			out.Budget.UsedTokens = uint64(used)
		}
		if reserved > 0 {
			out.Budget.ReservedTokens = uint64(reserved)
		}
	}
	if elapsed > 0 {
		out.Budget.UsedDurationMS = uint64(elapsed / time.Millisecond)
	}
	if preview {
		return team.RedactDecisionOutput(out)
	}

	snapshot := tc.coordinator.DecisionTerminalSnapshot()
	if !snapshot.Configured {
		return out, fmt.Errorf("decision terminal runtime is not configured")
	}
	out.LogicalRunID = stringPointer(snapshot.LogicalRunID)
	out.ExecutionRunID = stringPointer(snapshot.ExecutionRunID)
	out.BranchID = stringPointer(snapshot.BranchID)
	out.LogicalDisposition = "active"
	out.Primary.Generation = uint64(snapshot.Generation)
	if snapshot.Prepared {
		out.Primary.State = "blocked"
	}
	for _, code := range snapshot.ReasonCodes {
		out.Reasons = append(out.Reasons, team.DecisionOutputMessageV1{Code: code, Message: code, Retryable: code == team.ReasonDecisionPrimaryMissing})
	}
	result := tc.coordinator.LastRunResult()
	if result != nil {
		out.TerminalPersisted = true
		out.LogicalDisposition = "closed"
		out.Outcome = string(result.Outcome)
		out.ExitCode = result.ExitCode
		out.GoalSatisfied = result.GoalSatisfied
		if strings.TrimSpace(result.Reason) != "" {
			out.Reasons = append(out.Reasons, team.DecisionOutputMessageV1{Code: decisionOutputReasonCode(result), Message: result.Reason, Retryable: result.ExitCode == 7})
		}
		out.Acceptance.Team = decisionOutputTeamAcceptance(result.Acceptance)
	}
	if binding := snapshot.PrimaryBinding; binding != nil {
		out.Primary.State = "bound"
		out.Primary.TaskID = stringPointer(binding.TaskID)
		out.Primary.Generation = uint64(binding.Generation)
		out.Primary.DecisionID = stringPointer(binding.DecisionID)
		out.Primary.BindingEventID = snapshot.PrimaryBindingEventID
		out.Primary.RecordRef = &binding.RecordRef
		out.Primary.BaseEvidenceRef = &binding.BaseEvidenceRef
		out.Primary.SealedEvidenceHash = stringPointer(binding.SealedEvidenceHash)
		record, resolveErr := tc.coordinator.ResolvePrimaryDecisionRecord(ctx, binding.RecordRef)
		if resolveErr != nil {
			out.Primary.State = "invalidated"
			out.Reasons = append(out.Reasons, team.DecisionOutputMessageV1{Code: "decision_record_invalid", Message: resolveErr.Error(), Retryable: false})
		} else {
			view, truncated, viewErr := team.DecisionRecordView(record)
			if viewErr != nil {
				return out, viewErr
			}
			out.Primary.View = view
			out.Redaction.RecordViewTruncated = truncated
			out.Acceptance.DecisionProcess = "passed"
		}
		if evidence, evidenceErr := tc.coordinator.ResolvePrimaryDecisionEvidence(ctx, binding.BaseEvidenceRef); evidenceErr == nil {
			out.Coverage = decisionOutputCoverage(evidence.Coverage)
		}
	}
	if result == nil && snapshot.PrimaryBinding != nil {
		out.LogicalDisposition = "suspended"
		out.Outcome = "partial"
		out.ExitCode = 7
		out.Continuation = &team.DecisionOutputContinuationV1{ResumeLogicalRunID: snapshot.LogicalRunID, RequiresOperator: false}
	}
	if out.Acceptance.DecisionProcess == "passed" && (out.Acceptance.Team == "passed" || out.Acceptance.Team == "not_configured") {
		out.Acceptance.Combined = "passed"
	} else if out.TerminalPersisted {
		out.Acceptance.Combined = "failed"
	}
	return team.RedactDecisionOutput(out)
}

func decisionOutputTeamDigest(tc *teamContext) (string, error) {
	if tc.session.Dir != "" {
		if digest, err := team.StrictTeamDefinitionRevision(tc.session.Dir); err == nil {
			return digest, nil
		}
	}
	return team.DecisionContractDigest("hufu/team-definition-output/v1", struct {
		Name string `json:"name"`
	}{Name: tc.teamName})
}

func decisionOutputCoverage(value team.PrimaryEvidenceCoverageV1) *team.DecisionOutputCoverageV1 {
	return &team.DecisionOutputCoverageV1{
		InventoryRef: value.InventoryRef, InventoryCount: value.InventoryCount, SelectedCount: value.SelectedCount, ExcludedCount: value.ExcludedCount,
		ExcludedReasonCounts: team.DecisionOutputCoverageReasonsV1{
			Duplicate: value.ExcludedReasonCounts.Duplicate, OutOfScope: value.ExcludedReasonCounts.OutOfScope,
			Superseded: value.ExcludedReasonCounts.Superseded, RedactedUnusable: value.ExcludedReasonCounts.RedactedUnusable,
			OptionalOversize: value.ExcludedReasonCounts.OptionalOversize, Budget: value.ExcludedReasonCounts.Budget,
			UnsupportedMedia: value.ExcludedReasonCounts.UnsupportedMedia,
		},
		MandatorySelectedCount: value.MandatorySelectedCount, KnownIndependentGroups: value.KnownIndependentGroups,
		UnresolvedProvenanceCount: value.UnresolvedProvenanceCount,
	}
}

func decisionOutputReasonCode(result *team.RunResult) string {
	if result == nil {
		return "decision_failed"
	}
	if result.StopReason != "" {
		return string(result.StopReason)
	}
	return "decision_" + string(result.Outcome)
}

func decisionOutputTeamAcceptance(value *team.AcceptanceResult) string {
	if value == nil {
		return "not_configured"
	}
	switch value.EffectiveState() {
	case team.AcceptancePassed:
		return "passed"
	case team.AcceptanceFailed:
		return "failed"
	default:
		return "not_configured"
	}
}

func writeDecisionOutput(writer io.Writer, value team.DecisionOutputV1, indent bool) error {
	encoder := json.NewEncoder(writer)
	if opts.eventFormat == "jsonl" {
		return encoder.Encode(decisionJSONLEnvelope{SchemaVersion: 1, Type: "result", Sequence: decisionJSONLSequence.Add(1), Data: value})
	}
	if indent {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(value)
}

func renderDecisionOutputText(writer io.Writer, value team.DecisionOutputV1) error {
	if value.Kind == "decision_run_preview" {
		_, err := fmt.Fprintf(writer, "Decision preview: team=%s profile=%s (no logical run created)\n", value.Team.ID, value.Profile.ResolvedRef)
		return err
	}
	if value.Primary.View != nil {
		_, err := fmt.Fprintf(writer, "Decision: %s\nOutcome: %s\nSelected option: %s\n", nullableString(value.LogicalRunID), value.Outcome, value.Primary.View.SelectedOptionID)
		return err
	}
	_, err := fmt.Fprintf(writer, "Decision: %s\nOutcome: %s\n", nullableString(value.LogicalRunID), value.Outcome)
	return err
}

func renderResumedDecision(ctx context.Context, loadedTeams map[string]*teamContext) error {
	output, err := buildDecisionOutput(ctx, loadedTeams, false, 0)
	if err != nil {
		return err
	}
	if opts.outputFormat == "json" || opts.eventFormat == "jsonl" {
		err = writeDecisionOutput(outputWriter(), output, opts.eventFormat != "jsonl")
	} else {
		err = renderDecisionOutputText(outputWriter(), output)
	}
	if err != nil {
		return err
	}
	return decisionOutputProcessResult(output)
}

func outputWriter() io.Writer { return os.Stdout }

func decisionOutputProcessResult(output team.DecisionOutputV1) error {
	if output.ExitCode == 0 {
		return nil
	}
	return &decisionOutputExitError{code: output.ExitCode, err: fmt.Errorf("decision outcome: %s", output.Outcome)}
}

func stringPointer(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func nullableString(value *string) string {
	if value == nil {
		return "-"
	}
	return *value
}
