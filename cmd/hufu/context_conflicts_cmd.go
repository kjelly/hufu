package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/memoryconflict"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/utils"
)

var conflictsWorkspace, conflictsProject, conflictsTeam, conflictsSearchPath, conflictsModel, conflictsDismissReason string
var conflictsJSON, conflictsDryRun, conflictsVector, conflictsRetryUndetermined, conflictsAll, conflictsShowContent bool
var conflictsTopK, conflictsMaxPairs, conflictsMaxItems, conflictsVectorTopK int
var conflictsMinVectorSimilarity float64

// conflictJudgeFactory builds the scan judge; tests replace it with a fake.
var conflictJudgeFactory = func(ctx context.Context, teamDir, workspace string) (memoryconflict.TextGenerator, string, func(), error) {
	return newMaintenanceTextGenerator(ctx, maintenanceGeneratorOptions{
		TeamDir: teamDir, Workspace: workspace, ModelOverride: conflictsModel,
		Purpose: "memory_conflict_judge", Profile: sidecar.ClassifierProfile,
	})
}

// conflictVectorFactory opens the same Ollama vector index as hufu context
// query and rebuilds it for the scan scope; tests replace it with a fake.
var conflictVectorFactory = func(ctx context.Context, workspace string, repo contextstore.Repository, scope contextstore.Scope) (memoryconflict.SimilarSearcher, error) {
	store, err := contextstore.OpenOllamaVectorStore(workspace, config.ResolveEmbeddingModel(""), config.DefaultOllamaAPIURL)
	if err != nil {
		return nil, err
	}
	if err = store.Rebuild(ctx, repo, scope); err != nil {
		return nil, err
	}
	return store, nil
}

var contextConflictsCmd = &cobra.Command{Use: "conflicts", Short: "Find and review contradicting persistent memories"}
var contextConflictsScanCmd = &cobra.Command{Use: "scan", Short: "Judge candidate memory pairs for contradictions (offline model call)", Args: cobra.NoArgs, RunE: runConflictsScan}
var contextConflictsListCmd = &cobra.Command{Use: "list", Short: "List open memory conflicts without memory content", Args: cobra.NoArgs, RunE: runConflictsList}
var contextConflictsShowCmd = &cobra.Command{Use: "show <conflict-id>", Short: "Show one memory conflict", Args: cobra.ExactArgs(1), RunE: runConflictsShow}
var contextConflictsDismissCmd = &cobra.Command{Use: "dismiss <conflict-id>", Short: "Mark an open conflict as not a real contradiction", Args: cobra.ExactArgs(1), RunE: runConflictsDismiss}

func init() {
	contextCmd.AddCommand(contextConflictsCmd)
	flags := contextConflictsCmd.PersistentFlags()
	flags.StringVarP(&conflictsWorkspace, "workspace", "w", "", "Workspace containing context.sqlite and logs/event_store.jsonl")
	flags.StringVar(&conflictsProject, "project", "", "Canonical context project scope (required)")
	flags.StringVar(&conflictsTeam, "team", "", "Canonical context team scope (required)")
	flags.BoolVar(&conflictsJSON, "json", false, "Emit stable JSON")
	scan := contextConflictsScanCmd.Flags()
	scan.StringVar(&conflictsSearchPath, "team-search-path", "", "Comma-separated team search paths")
	scan.StringVar(&conflictsModel, "model", "", "Model used to judge memory pairs")
	scan.IntVar(&conflictsTopK, "top-k", 5, "Token-overlap neighbors per memory")
	scan.IntVar(&conflictsMaxPairs, "max-pairs", 20, "Maximum judge calls in this scan")
	scan.IntVar(&conflictsMaxItems, "max-items", 2000, "Maximum memories per scope")
	scan.BoolVar(&conflictsVector, "vector", false, "Also use Ollama embedding neighbors (requires a local embedding model)")
	scan.IntVar(&conflictsVectorTopK, "vector-top-k", 5, "Embedding neighbors per memory with --vector")
	scan.Float64Var(&conflictsMinVectorSimilarity, "min-vector-similarity", 0.45, "Minimum cosine similarity for embedding neighbors with --vector")
	scan.BoolVar(&conflictsRetryUndetermined, "retry-undetermined", false, "Judge again pairs whose previous judge output was invalid")
	scan.BoolVar(&conflictsDryRun, "dry-run", false, "Count candidate pairs without calling the judge or writing judgments")
	contextConflictsListCmd.Flags().BoolVar(&conflictsAll, "all", false, "Include dismissed, resolved, and inactive conflicts")
	contextConflictsShowCmd.Flags().BoolVar(&conflictsShowContent, "show-content", false, "Show both redacted memories and the judge rationale")
	contextConflictsDismissCmd.Flags().StringVar(&conflictsDismissReason, "reason", "", "Why this is not a real contradiction (required)")
	contextConflictsCmd.AddCommand(contextConflictsScanCmd, contextConflictsListCmd, contextConflictsShowCmd, contextConflictsDismissCmd)
}

func conflictsScopeValid() error {
	if strings.TrimSpace(conflictsProject) == "" {
		return fmt.Errorf("--project is required")
	}
	if strings.TrimSpace(conflictsTeam) == "" {
		return fmt.Errorf("--team is required")
	}
	return nil
}

func conflictsWorkspacePath() string {
	if conflictsWorkspace != "" {
		return conflictsWorkspace
	}
	return resolveWorkspaceForTeam(context.Background(), conflictsTeam)
}

func openConflictsRepository(ctx context.Context) (*contextstore.SQLiteRepository, error) {
	if err := conflictsScopeValid(); err != nil {
		return nil, err
	}
	repo, err := openExistingContextRepository(conflictsWorkspacePath())
	if err != nil {
		return nil, err
	}
	if err = flushGovernanceEvents(ctx, repo, conflictsWorkspacePath()); err != nil {
		_ = repo.Close()
		return nil, err
	}
	return repo, nil
}

func openConflictsReadOnly() (contextstore.ReadOnlyRepository, error) {
	if err := conflictsScopeValid(); err != nil {
		return nil, err
	}
	return contextstore.OpenSQLiteReadOnly(filepath.Join(conflictsWorkspacePath(), "context.sqlite"))
}

func runConflictsScan(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	repo, err := openConflictsRepository(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	workspace := conflictsWorkspacePath()
	pairOpts := memoryconflict.PairOptions{TopK: conflictsTopK, MaxItemsPerScope: conflictsMaxItems, VectorTopK: conflictsVectorTopK, MinVectorSimilarity: conflictsMinVectorSimilarity}
	vectorFailed := false
	if conflictsVector {
		searcher, vectorErr := conflictVectorFactory(ctx, workspace, repo, contextstore.Scope{ProjectID: conflictsProject, TeamID: conflictsTeam})
		if vectorErr != nil {
			vectorFailed = true
		} else {
			pairOpts.Vector = searcher
		}
	}
	scanOpts := memoryconflict.ScanOptions{ProjectID: conflictsProject, TeamID: conflictsTeam, MaxPairs: conflictsMaxPairs, MaxPromptRunes: sidecar.ClassifierProfile.InputRuneLimit(), DryRun: conflictsDryRun, RetryUndetermined: conflictsRetryUndetermined, Actor: "operator"}
	judge := memoryconflict.JSONJudge{}
	if !conflictsDryRun {
		registry := teamRegistryFromSearchPath(conflictsSearchPath)
		if err = registry.Discover(); err != nil {
			return err
		}
		teamDir, resolveErr := registry.Resolve(conflictsTeam)
		if resolveErr != nil {
			return resolveErr
		}
		generator, model, release, genErr := conflictJudgeFactory(ctx, teamDir, workspace)
		if genErr != nil {
			return genErr
		}
		defer release()
		judge.Generator = generator
		scanOpts.JudgeModel = model
	}
	report, scanErr := memoryconflict.Scan(ctx, repo, judge, scanOpts, pairOpts)
	if vectorFailed {
		report.Diagnostics = append(report.Diagnostics, memoryconflict.Diagnostic{Reason: memoryconflict.ReasonVectorUnavailable})
	}
	if flushErr := flushGovernanceEvents(ctx, repo, workspace); flushErr != nil && scanErr == nil {
		scanErr = flushErr
	}
	if err = writeConflictScanReport(cmd.OutOrStdout(), report); err != nil {
		return err
	}
	return scanErr
}

func vectorLabel(report memoryconflict.ScanReport) string {
	if report.VectorUsed {
		return fmt.Sprint(report.VectorPairs)
	}
	for _, d := range report.Diagnostics {
		if d.Reason == memoryconflict.ReasonVectorUnavailable {
			return "unavailable"
		}
	}
	return "off"
}

func writeConflictScanReport(w io.Writer, report memoryconflict.ScanReport) error {
	sort.SliceStable(report.Diagnostics, func(i, j int) bool {
		left, right := diagnosticSubject(report.Diagnostics[i]), diagnosticSubject(report.Diagnostics[j])
		if left != right {
			return left < right
		}
		return report.Diagnostics[i].Reason < report.Diagnostics[j].Reason
	})
	if conflictsJSON {
		return json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "scan": report})
	}
	suffix := ""
	if report.DryRun {
		suffix = " dry_run"
	}
	if _, err := fmt.Fprintf(w, "conflict scan: eligible=%d candidates=%d (lexical=%d file_path=%d vector=%s) already_judged=%d judged=%d contradictions=%d undetermined=%d skipped=%d deferred=%d%s\n",
		report.EligibleItems, report.CandidatePairs, report.LexicalPairs, report.FilePathPairs, vectorLabel(report), report.AlreadyJudged, report.Judged, report.Contradictions, report.Undetermined, report.Skipped, report.Deferred, suffix); err != nil {
		return err
	}
	for _, d := range report.Diagnostics {
		if _, err := fmt.Fprintf(w, "diagnostic\t%s\t%s\n", diagnosticSubject(d), d.Reason); err != nil {
			return err
		}
	}
	return nil
}

func diagnosticSubject(d memoryconflict.Diagnostic) string {
	switch {
	case d.ItemID != "":
		return d.ItemID
	case d.PairID != "":
		return d.PairID
	default:
		return "-"
	}
}

type conflictView struct {
	ID                 string    `json:"id"`
	State              string    `json:"state"`
	ItemIDs            []string  `json:"item_ids"`
	Kinds              []string  `json:"kinds"`
	ContentHashes      []string  `json:"content_hashes"`
	JudgePolicyVersion string    `json:"judge_policy_version"`
	JudgeModel         string    `json:"judge_model"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	Contents           []string  `json:"contents,omitempty"`
	Rationale          string    `json:"rationale,omitempty"`
	DismissReason      string    `json:"dismiss_reason,omitempty"`
}

func newConflictView(v contextstore.ConflictView) conflictView {
	return conflictView{
		ID: v.ID, State: string(v.State),
		ItemIDs: []string{v.ItemAID, v.ItemBID}, Kinds: []string{string(v.ItemAKind), string(v.ItemBKind)},
		ContentHashes:      []string{v.ItemAContentHash, v.ItemBContentHash},
		JudgePolicyVersion: v.JudgePolicyVersion, JudgeModel: v.JudgeModel, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
}

const conflictsUnavailableMessage = "Memory conflicts are unavailable until the context store is upgraded."

func runConflictsList(cmd *cobra.Command, _ []string) error {
	repo, err := openConflictsReadOnly()
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	items, err := repo.ListConflicts(cmd.Context(), contextstore.ConflictQuery{ProjectID: conflictsProject, TeamID: conflictsTeam, IncludeInactive: conflictsAll})
	if errors.Is(err, contextstore.ErrConflictsUnavailable) {
		if conflictsJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "conflicts": []any{}, "message": conflictsUnavailableMessage})
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), conflictsUnavailableMessage)
		return err
	}
	if err != nil {
		return err
	}
	views := make([]conflictView, 0, len(items))
	for _, item := range items {
		views = append(views, newConflictView(item))
	}
	if conflictsJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "conflicts": views})
	}
	if len(views) == 0 {
		message := "No open memory conflicts."
		if conflictsAll {
			message = "No memory conflicts."
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), message)
		return err
	}
	for _, v := range views {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", v.ID, v.State, v.ItemIDs[0], v.ItemIDs[1]); err != nil {
			return err
		}
	}
	return nil
}

func runConflictsShow(cmd *cobra.Command, args []string) error {
	repo, err := openConflictsReadOnly()
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	found, err := repo.GetConflict(cmd.Context(), args[0], conflictsProject, conflictsTeam)
	if errors.Is(err, contextstore.ErrConflictsUnavailable) {
		return contextstore.ErrConflictsUnavailable
	}
	if err != nil {
		return err
	}
	view := newConflictView(found)
	if conflictsShowContent {
		for _, id := range view.ItemIDs {
			content := "(memory no longer available)"
			item, getErr := repo.GetScoped(cmd.Context(), id, contextstore.ScopedReadOptions{Scope: contextstore.Scope{ProjectID: conflictsProject, TeamID: conflictsTeam, AgentID: found.AgentID}, IncludeContent: true})
			if getErr == nil {
				content = utils.RedactSecrets(item.Content)
			}
			view.Contents = append(view.Contents, content)
		}
		view.Rationale, view.DismissReason = found.Rationale, found.DismissReason
	}
	if conflictsJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "conflict": view})
	}
	out := cmd.OutOrStdout()
	if _, err = fmt.Fprintf(out, "%s\nstate: %s\njudge: %s (%s)\n", view.ID, view.State, view.JudgeModel, view.JudgePolicyVersion); err != nil {
		return err
	}
	for i, id := range view.ItemIDs {
		if _, err = fmt.Fprintf(out, "memory %s: kind=%s content_hash=%s\n", id, view.Kinds[i], view.ContentHashes[i]); err != nil {
			return err
		}
		if conflictsShowContent {
			if _, err = fmt.Fprintf(out, "  %s\n", view.Contents[i]); err != nil {
				return err
			}
		}
	}
	if conflictsShowContent {
		if _, err = fmt.Fprintf(out, "rationale: %s\n", view.Rationale); err != nil {
			return err
		}
		if view.DismissReason != "" {
			if _, err = fmt.Fprintf(out, "dismiss reason: %s\n", view.DismissReason); err != nil {
				return err
			}
		}
	}
	if found.State == contextstore.ConflictStateOpen {
		_, err = fmt.Fprintf(out, "resolve: hufu context supersede %s --with %s --project %s --team %s\n    or: hufu context conflicts dismiss %s --project %s --team %s --reason <why>\n",
			view.ItemIDs[0], view.ItemIDs[1], conflictsProject, conflictsTeam, view.ID, conflictsProject, conflictsTeam)
	}
	return err
}

func runConflictsDismiss(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	repo, err := openConflictsRepository(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	found, dismissed, err := repo.DismissConflict(ctx, args[0], conflictsProject, conflictsTeam, conflictsDismissReason, "operator")
	if err != nil {
		return err
	}
	if err = flushGovernanceEvents(ctx, repo, conflictsWorkspacePath()); err != nil {
		return err
	}
	if conflictsJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "conflict": newConflictView(found), "already_dismissed": !dismissed})
	}
	status := "dismissed"
	if !dismissed {
		status = "already dismissed"
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", found.ID, status)
	return err
}
