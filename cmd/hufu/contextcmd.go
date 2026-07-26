package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/config"
	contextstore "github.com/anomalyco/hufu/internal/context"
)

var contextWorkspace string
var contextProject string
var contextTeam string
var contextQueryJSON bool

var contextCmd = &cobra.Command{
	Use:   "context",
	Short: "Inspect and maintain the canonical context store (Phase 1: shadow mode only)",
}

var contextRepairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Retry context items that failed to shadow-write into the canonical store",
	Long: `Shadow writes never block the legacy memory path (stm_write, ltm_update,
memory_save, AutoExtractLTM): a failed write is instead recorded, redacted,
in <workspace>/context-pending.jsonl. This command replays those pending
items into the canonical SQLite store, removing the ones that succeed and
leaving anything that still fails for the next run. It is safe to run
repeatedly.`,
	Args: cobra.NoArgs,
	RunE: runContextRepair,
}

var contextInspectCmd = &cobra.Command{
	Use:   "inspect",
	Short: "Show recent ContextCompiler shadow traces without exposing prompt content",
	Args:  cobra.NoArgs,
	RunE:  runContextInspect,
}

var contextQueryCmd = &cobra.Command{
	Use:   "query <text>",
	Short: "Hybrid-query canonical context (exact + FTS5; vector index when configured)",
	Args:  cobra.ExactArgs(1),
	RunE:  runContextQuery,
}

var contextRebuildCmd = &cobra.Command{
	Use:   "rebuild",
	Short: "Rebuild the canonical FTS5 lexical index from SQLite records",
	Args:  cobra.NoArgs,
	RunE:  runContextRebuild,
}

func init() {
	contextRepairCmd.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace directory containing context.sqlite (default: <cwd>/workspace)")
	contextInspectCmd.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace directory containing context shadow traces (default: <cwd>/workspace)")
	contextCmd.AddCommand(contextRepairCmd)
	contextCmd.AddCommand(contextInspectCmd)
	contextQueryCmd.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace directory containing context.sqlite")
	contextQueryCmd.Flags().StringVar(&contextProject, "project", "", "Canonical project ID (required)")
	contextQueryCmd.Flags().StringVar(&contextTeam, "team", "", "Optional team scope")
	contextQueryCmd.Flags().BoolVar(&contextQueryJSON, "json", false, "Emit JSON")
	contextCmd.AddCommand(contextQueryCmd)
	contextRebuildCmd.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace directory containing context.sqlite")
	contextCmd.AddCommand(contextRebuildCmd)
}

func runContextRebuild(cmd *cobra.Command, _ []string) error {
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return err
	}
	defer repo.Close()
	if err := repo.RebuildLexical(cmd.Context()); err != nil {
		return fmt.Errorf("rebuilding FTS5 index: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "context rebuild: FTS5 index rebuilt")
	return err
}

func runContextQuery(cmd *cobra.Command, args []string) error {
	if contextProject == "" {
		return fmt.Errorf("--project is required")
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return err
	}
	defer repo.Close()
	var vector contextstore.VectorSearcher
	vectorStore, vectorErr := contextstore.OpenOllamaVectorStore(getContextWorkspace(), config.ResolveEmbeddingModel(""), config.DefaultOllamaAPIURL)
	if vectorErr == nil {
		vectorErr = vectorStore.Rebuild(cmd.Context(), repo, contextstore.Scope{ProjectID: contextProject, TeamID: contextTeam})
	}
	if vectorErr == nil {
		vector = vectorStore
	}
	results, trace, err := contextstore.HybridRetrieve(cmd.Context(), repo, vector, contextstore.SearchRequest{Query: args[0], Scope: contextstore.Scope{ProjectID: contextProject, TeamID: contextTeam}, Limit: 20})
	if err != nil {
		return err
	}
	if contextQueryJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Results []contextstore.SearchResult `json:"results"`
			Trace   contextstore.RetrievalTrace `json:"trace"`
		}{results, trace})
	}
	for _, result := range results {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%.4f\t%s\n", result.Item.ID, result.Score, result.Item.Content); err != nil {
			return err
		}
	}
	if trace.RetrievalInsufficient {
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "context query: retrieval insufficient")
		return err
	}
	return nil
}

type contextShadowTrace struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Fingerprint     string   `json:"fingerprint,omitempty"`
	LegacyTokens    int      `json:"legacy_tokens"`
	CanonicalTokens int      `json:"canonical_tokens"`
	BudgetTokens    int      `json:"budget_tokens"`
	SelectedItems   int      `json:"selected_items"`
	MissingAnchors  []string `json:"missing_anchors,omitempty"`
	Error           string   `json:"error,omitempty"`
}

func runContextInspect(cmd *cobra.Command, _ []string) error {
	path := filepath.Join(getContextWorkspace(), "context-shadow-traces.jsonl")
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), "context inspect: no shadow traces")
		return werr
	}
	if err != nil {
		return fmt.Errorf("opening context shadow traces: %w", err)
	}
	defer f.Close()
	var traces []contextShadowTrace
	s := bufio.NewScanner(f)
	for s.Scan() {
		var t contextShadowTrace
		if json.Unmarshal(s.Bytes(), &t) == nil {
			traces = append(traces, t)
		}
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("reading context shadow traces: %w", err)
	}
	if len(traces) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "context inspect: no valid shadow traces")
		return err
	}
	t := traces[len(traces)-1]
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context inspect: trace %s (%s)\nlegacy tokens: %d\ncanonical tokens: %d / budget %d\nselected items: %d\nmissing anchors: %d\n", t.ID, t.Kind, t.LegacyTokens, t.CanonicalTokens, t.BudgetTokens, t.SelectedItems, len(t.MissingAnchors))
	return err
}

func getContextWorkspace() string {
	if contextWorkspace != "" {
		return contextWorkspace
	}
	return getWorkspace()
}

func runContextRepair(cmd *cobra.Command, _ []string) error {
	workspace := getContextWorkspace()
	dbPath := filepath.Join(workspace, "context.sqlite")
	pendingPath := filepath.Join(workspace, "context-pending.jsonl")
	if _, err := os.Stat(pendingPath); os.IsNotExist(err) {
		_, werr := fmt.Fprintln(cmd.OutOrStdout(), "context repair: no pending writes")
		return werr
	}
	repo, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		return fmt.Errorf("opening context store at %s: %w", dbPath, err)
	}
	defer repo.Close()

	recovered, remaining, err := contextstore.RepairPendingWrites(cmd.Context(), repo, pendingPath)
	if err != nil {
		return fmt.Errorf("repairing pending context writes: %w", err)
	}
	_, werr := fmt.Fprintf(cmd.OutOrStdout(), "context repair: %d recovered, %d still pending\n", recovered, remaining)
	return werr
}
