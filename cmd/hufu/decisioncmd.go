package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
)

// hufu decision — the cross-run entry point for decisions
// (docs/hufu-decision-aware-runtime-spec.md §49.2).
//
// A decision's record and events live in the run that formed it, and that run
// exits. These commands exist so a decision can still be found, read and
// resolved afterwards, which is the addressing layer outcome resolution needs.
//
// Nothing here computes calibration. Resolving an outcome records what
// happened; it never edits the DecisionRecord and never reweights anything.

var (
	decisionWorkspace string
	decisionJSON      bool
	decisionAll       bool

	decisionResolveOutcome  string
	decisionResolveEvidence []string
	decisionResolveLessons  []string
	decisionResolveBy       string
	decisionResolveNotes    string
	decisionResolveForecast float64

	decisionAssumeStatus string
	decisionAssumeNote   string

	decisionStatsRunID string
)

// decisionExitError carries a fixed process exit code across the cobra
// boundary, following cmd/hufu/main.go's dispatch convention.
type decisionExitError struct {
	code int
	msg  string
}

func (e *decisionExitError) Error() string        { return e.msg }
func (e *decisionExitError) ProcessExitCode() int { return e.code }

var decisionCmd = &cobra.Command{
	Use:   "decision",
	Short: "Inspect and resolve decisions formed by the decision-aware runtime",
	Long: `hufu decision addresses decisions across runs.

A decision's canonical record is an artifact in the run that formed it and its
transitions are in that run's event log. Both become unreachable once the run
exits, so the workspace also keeps an append-only index of every decision any
run has formed. These commands read that index.

Resolving an outcome never edits the decision record: what was decided, and on
what evidence, stays exactly as it was formed. The outcome is a separate
record, and it counts as verified only when the artifacts it cites actually
resolve in the workspace's content-addressed store.`,
	Args: cobra.NoArgs,
}

var decisionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List decisions awaiting an outcome",
	Long: `hufu decision list shows decisions that have no recorded outcome yet.

Use --all to include already-resolved decisions.`,
	Args: cobra.NoArgs,
	RunE: runDecisionList,
}

var decisionShowCmd = &cobra.Command{
	Use:   "show <decision-id>",
	Short: "Show one decision's index entry and recorded outcome",
	Args:  cobra.ExactArgs(1),
	RunE:  runDecisionShow,
}

var decisionResolveCmd = &cobra.Command{
	Use:   "resolve <decision-id>",
	Short: "Record what a decision actually turned out to be",
	Long: `hufu decision resolve records the observed outcome of a decision.

    hufu decision resolve dec-1 --outcome succeeded \
        --evidence <sha256> --lesson "ingest lag never exceeded 2m"

--outcome is one of succeeded, failed, mixed, superseded, unresolved. It
describes what happened, not whether the judgment was sound: a bad outcome does
not by itself prove the decision was unreasonable given the evidence available
at the time.

An outcome is recorded as verified only when every artifact digest it cites
resolves in the workspace's artifact store, and at least one does. An outcome
with no evidence is still recorded, as unverified — knowing what happened
without proof is worth keeping, as long as nothing downstream treats it as
proven. --resolved-by is provenance, never justification.

A decision can be resolved once. The decision record itself is never edited.`,
	Args: cobra.ExactArgs(1),
	RunE: runDecisionResolve,
}

var decisionAssumeCmd = &cobra.Command{
	Use:   "assume <decision-id> <assumption-id>",
	Short: "Record that a decision's assumption was checked",
	Long: `hufu decision assume records an operator's check of a decision assumption.

    hufu decision assume dec-1 A1 --status contradicted --note "traffic doubled in week 2"

This is the third of the three sources allowed to change an assumption's status;
the runtime never infers one. Only supported and contradicted are reportable:
'unknown' is the absence of a check, and 'stale' is the runtime's own conclusion
that an earlier check no longer applies.

A contradicted critical assumption is what invalidates a decision. Recording one
does not edit the decision: what was decided, and on what evidence, stays as it
was formed.`,
	Args: cobra.ExactArgs(2),
	RunE: runDecisionAssume,
}

var decisionStatsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Project decision activity from the durable event log and index",
	Long: `hufu decision stats reports the decision metrics from §40 of the spec.

Every counter is a projection over state the runtime already persisted, so a
metric can never drift from the events that produced it. Nothing is stored
separately and nothing is counted twice.

Without --run the report covers what the cross-run index knows: how many
decisions exist, under which profiles, how well-sourced they were, and how many
have been resolved. With --run it also projects that run's event log, which is
where gate failures, rejected opinions, degradations, kill criteria and replans
are recorded.

The report also states how close the resolved sample is to opening Phase 5,
which is the one entry condition code cannot satisfy.`,
	Args: cobra.NoArgs,
	RunE: runDecisionStats,
}

func init() {
	decisionCmd.PersistentFlags().StringVarP(&decisionWorkspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	decisionCmd.PersistentFlags().BoolVar(&decisionJSON, "json", false, "Write JSON to stdout; all diagnostics go to stderr")

	decisionListCmd.Flags().BoolVar(&decisionAll, "all", false, "Include decisions that already have a recorded outcome")
	decisionCmd.AddCommand(decisionListCmd)

	decisionCmd.AddCommand(decisionShowCmd)

	decisionResolveCmd.Flags().StringVar(&decisionResolveOutcome, "outcome", "", "Observed outcome: succeeded, failed, mixed, superseded, or unresolved (required)")
	decisionResolveCmd.Flags().StringArrayVar(&decisionResolveEvidence, "evidence", nil, "SHA-256 digest of an artifact that evidences the outcome (repeatable)")
	decisionResolveCmd.Flags().StringArrayVar(&decisionResolveLessons, "lesson", nil, "A lesson learned from this outcome (repeatable)")
	decisionResolveCmd.Flags().StringVar(&decisionResolveBy, "resolved-by", "", "Who recorded this outcome (provenance, not justification)")
	decisionResolveCmd.Flags().StringVar(&decisionResolveNotes, "notes", "", "Free-text notes about the outcome")
	decisionResolveCmd.Flags().Float64Var(&decisionResolveForecast, "forecast", 0, "Override the forecast this outcome resolves against (default: the decision's own probability)")
	decisionCmd.AddCommand(decisionResolveCmd)

	decisionAssumeCmd.Flags().StringVar(&decisionAssumeStatus, "status", "", "Observed status: supported or contradicted (required)")
	decisionAssumeCmd.Flags().StringVar(&decisionAssumeNote, "note", "", "Why the status changed")
	decisionCmd.AddCommand(decisionAssumeCmd)

	decisionStatsCmd.Flags().StringVar(&decisionStatsRunID, "run", "", "Also project this run's event log (default: index-derived metrics only)")
	decisionCmd.AddCommand(decisionStatsCmd)
}

func getDecisionWorkspace() string {
	if decisionWorkspace != "" {
		return decisionWorkspace
	}
	return getWorkspace()
}

func openDecisionIndex() (*team.DecisionIndex, error) {
	index, err := team.OpenDecisionIndex(getDecisionWorkspace())
	if err != nil {
		return nil, &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision: %v", err)}
	}
	return index, nil
}

func runDecisionList(cmd *cobra.Command, args []string) error {
	index, err := openDecisionIndex()
	if err != nil {
		return err
	}
	entries, err := index.List()
	if err != nil {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision list: %v", err)}
	}
	if !decisionAll {
		var pending []team.DecisionIndexEntry
		for _, entry := range entries {
			if !entry.Resolved() {
				pending = append(pending, entry)
			}
		}
		entries = pending
	}

	if decisionJSON {
		return writeDecisionJSON(cmd, map[string]any{
			"index":   index.Path(),
			"count":   len(entries),
			"entries": entries,
		})
	}

	if len(entries) == 0 {
		scope := "unresolved decisions"
		if decisionAll {
			scope = "decisions"
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No %s in %s\n", scope, index.Path())
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "DECISION\tRUN\tPROFILE\tCHOSE\tP\tSTATUS\tFORMED")
	for _, entry := range entries {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			entry.DecisionID, shortOrDash(entry.RunID), shortOrDash(entry.Profile),
			shortOrDash(entry.FinalOption), formatProbability(entry.Probability),
			decisionStatus(entry), formatDecisionTime(entry.CreatedAt))
	}
	return w.Flush()
}

func runDecisionShow(cmd *cobra.Command, args []string) error {
	index, err := openDecisionIndex()
	if err != nil {
		return err
	}
	entry, found, err := index.Get(args[0])
	if err != nil {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision show: %v", err)}
	}
	if !found {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision show: decision %q is not in %s", args[0], index.Path())}
	}
	if decisionJSON {
		return writeDecisionJSON(cmd, entry)
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Decision:   %s\n", entry.DecisionID)
	_, _ = fmt.Fprintf(out, "Run:        %s\n", shortOrDash(entry.RunID))
	_, _ = fmt.Fprintf(out, "Task:       %s\n", shortOrDash(entry.TaskID))
	_, _ = fmt.Fprintf(out, "Profile:    %s\n", shortOrDash(entry.Profile))
	_, _ = fmt.Fprintf(out, "Question:   %s\n", shortOrDash(entry.Question))
	_, _ = fmt.Fprintf(out, "Chose:      %s (p=%s)\n", shortOrDash(entry.FinalOption), formatProbability(entry.Probability))
	_, _ = fmt.Fprintf(out, "Evidence:   %s\n", shortOrDash(entry.EvidenceHash))
	_, _ = fmt.Fprintf(out, "Record:     %s\n", shortOrDash(entry.RecordPath))
	_, _ = fmt.Fprintf(out, "Formed:     %s\n", formatDecisionTime(entry.CreatedAt))
	_, _ = fmt.Fprintf(out, "Status:     %s\n", decisionStatus(entry))
	if entry.Stale {
		_, _ = fmt.Fprintf(out, "Stale:      %s\n", shortOrDash(entry.StaleReason))
	}
	if len(entry.FalsificationConditions) > 0 {
		_, _ = fmt.Fprintln(out, "\nWould have shown this decision was wrong:")
		for _, condition := range entry.FalsificationConditions {
			_, _ = fmt.Fprintf(out, "  - %s\n", condition)
		}
	}
	if entry.Outcome != nil {
		outcome := entry.Outcome
		_, _ = fmt.Fprintf(out, "\nOutcome:    %s\n", outcome.ResolvedOutcome)
		_, _ = fmt.Fprintf(out, "Verified:   %t (%s)\n", outcome.Verified, outcome.VerificationSummary)
		_, _ = fmt.Fprintf(out, "Resolved:   %s by %s\n", formatDecisionTime(outcome.ResolvedAt), shortOrDash(outcome.ResolvedBy))
		if outcome.Notes != "" {
			_, _ = fmt.Fprintf(out, "Notes:      %s\n", outcome.Notes)
		}
		for _, lesson := range outcome.Lessons {
			_, _ = fmt.Fprintf(out, "  lesson: %s\n", lesson)
		}
		for _, ref := range outcome.ObservedEvidence {
			_, _ = fmt.Fprintf(out, "  evidence: %s %s\n", ref.SHA256, ref.Path)
		}
		for _, digest := range outcome.UnverifiedEvidence {
			_, _ = fmt.Fprintf(out, "  unresolved evidence: %s\n", digest)
		}
	}
	return nil
}

func runDecisionResolve(cmd *cobra.Command, args []string) error {
	decisionID := args[0]
	if strings.TrimSpace(decisionResolveOutcome) == "" {
		return &decisionExitError{code: 2, msg: "hufu decision resolve: --outcome is required"}
	}
	if !team.ValidOutcome(decisionResolveOutcome) {
		return &decisionExitError{code: 2, msg: fmt.Sprintf(
			"hufu decision resolve: --outcome %q is not one of succeeded, failed, mixed, superseded, unresolved",
			decisionResolveOutcome)}
	}

	index, err := openDecisionIndex()
	if err != nil {
		return err
	}
	entry, found, err := index.Get(decisionID)
	if err != nil {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision resolve: %v", err)}
	}
	if !found {
		return &decisionExitError{code: 2, msg: fmt.Sprintf(
			"hufu decision resolve: decision %q is not in %s", decisionID, index.Path())}
	}
	if entry.Resolved() {
		return &decisionExitError{code: 2, msg: fmt.Sprintf(
			"hufu decision resolve: decision %q was already resolved as %q; outcomes are recorded once",
			decisionID, entry.Outcome.ResolvedOutcome)}
	}

	outcome := team.DecisionOutcomeRecord{
		DecisionID:      decisionID,
		ResolvedOutcome: decisionResolveOutcome,
		Forecast:        decisionResolveForecast,
		Lessons:         decisionResolveLessons,
		Notes:           decisionResolveNotes,
		ResolvedBy:      decisionResolveBy,
		ResolvedAt:      time.Now().UTC(),
	}
	for _, digest := range decisionResolveEvidence {
		digest = strings.TrimSpace(digest)
		if digest == "" {
			continue
		}
		outcome.ObservedEvidence = append(outcome.ObservedEvidence, team.ArtifactRef{SHA256: digest})
	}
	sort.Slice(outcome.ObservedEvidence, func(a, b int) bool {
		return outcome.ObservedEvidence[a].SHA256 < outcome.ObservedEvidence[b].SHA256
	})

	// Verification is against real bytes in the workspace's store, so an
	// outcome cannot be declared verified by asserting it.
	artifacts, err := team.WorkspaceArtifacts(getDecisionWorkspace())
	if err != nil {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision resolve: %v", err)}
	}
	if err := team.VerifyOutcomeEvidence(&outcome, team.NewArtifactResolver(artifacts)); err != nil {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision resolve: %v", err)}
	}

	updated, err := index.Resolve(decisionID, outcome)
	if err != nil {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision resolve: %v", err)}
	}

	if decisionJSON {
		return writeDecisionJSON(cmd, updated)
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Recorded outcome %q for decision %s\n", updated.Outcome.ResolvedOutcome, decisionID)
	_, _ = fmt.Fprintf(out, "Verified: %t — %s\n", updated.Outcome.Verified, updated.Outcome.VerificationSummary)
	if len(updated.Outcome.UnverifiedEvidence) > 0 {
		_, _ = fmt.Fprintln(os.Stderr, "warning: some cited evidence did not resolve in the artifact store:")
		for _, digest := range updated.Outcome.UnverifiedEvidence {
			_, _ = fmt.Fprintf(os.Stderr, "  %s\n", digest)
		}
	}
	_, _ = fmt.Fprintf(out, "The decision record was not modified.\n")
	return nil
}

func writeDecisionJSON(cmd *cobra.Command, payload any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision: encoding JSON: %v", err)}
	}
	return nil
}

func decisionStatus(entry team.DecisionIndexEntry) string {
	switch {
	case entry.Outcome != nil && entry.Outcome.Verified:
		return entry.Outcome.ResolvedOutcome + " (verified)"
	case entry.Outcome != nil:
		return entry.Outcome.ResolvedOutcome + " (unverified)"
	case entry.Stale:
		return "stale"
	default:
		return "unresolved"
	}
}

func formatProbability(p float64) string {
	if p == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", p)
}

func formatDecisionTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func shortOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func runDecisionAssume(cmd *cobra.Command, args []string) error {
	decisionID, assumptionID := args[0], args[1]
	if strings.TrimSpace(decisionAssumeStatus) == "" {
		return &decisionExitError{code: 2, msg: "hufu decision assume: --status is required"}
	}
	if !team.ValidCheckStatus(decisionAssumeStatus) {
		return &decisionExitError{code: 2, msg: fmt.Sprintf(
			"hufu decision assume: --status %q is not reportable; use supported or contradicted",
			decisionAssumeStatus)}
	}

	index, err := openDecisionIndex()
	if err != nil {
		return err
	}
	store, err := team.OpenEventStore(getDecisionWorkspace())
	if err != nil {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision assume: opening canonical event store: %v", err)}
	}
	defer func() { _ = store.Close() }()
	team.BindDecisionIndexEventStore(index, store)
	entry, assumption, err := index.CheckAssumption(decisionID, assumptionID, decisionAssumeStatus, decisionAssumeNote)
	if err != nil {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision assume: %v", err)}
	}

	if decisionJSON {
		return writeDecisionJSON(cmd, entry)
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Recorded assumption %s as %s for decision %s\n",
		assumptionID, assumption.EffectiveStatus(), decisionID)
	if assumption.Critical && assumption.EffectiveStatus() == team.AssumptionContradicted {
		_, _ = fmt.Fprintf(out, "This is a critical assumption: decision %s now rests on a premise that does not hold.\n", decisionID)
	}
	_, _ = fmt.Fprintf(out, "The decision record was not modified.\n")
	return nil
}

func runDecisionStats(cmd *cobra.Command, args []string) error {
	index, err := openDecisionIndex()
	if err != nil {
		return err
	}
	entries, err := index.List()
	if err != nil {
		return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision stats: %v", err)}
	}

	var events []team.RunEvent
	if strings.TrimSpace(decisionStatsRunID) != "" {
		store, storeErr := team.OpenEventStore(getDecisionWorkspace())
		if storeErr != nil {
			return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision stats: %v", storeErr)}
		}
		read, readErr := store.ReadEvents()
		if readErr != nil {
			return &decisionExitError{code: 2, msg: fmt.Sprintf("hufu decision stats: %v", readErr)}
		}
		for _, event := range read {
			if event.RunID == decisionStatsRunID {
				events = append(events, event)
			}
		}
	}

	metrics := team.ComputeDecisionMetrics(events, entries)
	if decisionJSON {
		return writeDecisionJSON(cmd, map[string]any{
			"metrics":      metrics,
			"phase5_entry": metrics.Phase5Entry(),
		})
	}

	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Decisions formed:      %d\n", metrics.DecisionCount)
	for _, profile := range metrics.SortedProfiles() {
		_, _ = fmt.Fprintf(out, "  %-18s %d\n", profile, metrics.DecisionProfileCount[profile])
	}
	_, _ = fmt.Fprintf(out, "Independent groups:    %d\n", metrics.IndependentEvidenceGroupCount)
	_, _ = fmt.Fprintf(out, "Stale decisions:       %d\n", metrics.DecisionStaleCount)

	if len(events) > 0 {
		_, _ = fmt.Fprintf(out, "\nFrom run %s:\n", decisionStatsRunID)
		_, _ = fmt.Fprintf(out, "  Outside-view blocks:   %d\n", metrics.OutsideViewGateFailures)
		_, _ = fmt.Fprintf(out, "  No-go option missing:  %d\n", metrics.NoGoOptionMissing)
		_, _ = fmt.Fprintf(out, "  Premortem failure modes: %d\n", metrics.PremortemFailureModes)
		_, _ = fmt.Fprintf(out, "  Opinions rejected:     %d\n", metrics.DecisionOpinionRejectedCount)
		_, _ = fmt.Fprintf(out, "  Budget degradations:   %d\n", metrics.DecisionBudgetDegradedCount)
		_, _ = fmt.Fprintf(out, "  Shared-origin warnings: %d\n", metrics.SharedOriginWarnings)
		_, _ = fmt.Fprintf(out, "  Mean dispersion:       %.4f\n", metrics.MeanDispersion())
		_, _ = fmt.Fprintf(out, "  Revision rate:         %.4f\n", metrics.DecisionRevisionRate)
		_, _ = fmt.Fprintf(out, "  Assumption invalidations: %d\n", metrics.AssumptionInvalidations)
		_, _ = fmt.Fprintf(out, "  Kill criteria fired:   %d\n", metrics.KillCriteriaTriggered)
		_, _ = fmt.Fprintf(out, "  Replans requested:     %d\n", metrics.ReplanCount)
		_, _ = fmt.Fprintf(out, "  Commit gate blocks:    %d\n", metrics.CommitGateBlocked)
	}

	entry := metrics.Phase5Entry()
	_, _ = fmt.Fprintf(out, "\nOutcome sample:        %d resolved (%d verified)\n", entry.Resolved, entry.Verified)
	if entry.Open {
		_, _ = fmt.Fprintf(out, "Phase 5 entry condition is met (>= %d resolved, >= %d verified).\n",
			entry.ResolvedRequired, entry.VerifiedRequired)
	} else {
		_, _ = fmt.Fprintf(out, "Phase 5 needs %d resolved and %d verified; calibration stays closed until then.\n",
			entry.ResolvedRequired, entry.VerifiedRequired)
	}
	return nil
}
