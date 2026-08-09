package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/utils"
)

var contextWorkspace string
var contextProject string
var contextTeam string
var contextAgent string
var contextTier string
var contextLifecycle string
var contextQueryJSON bool
var contextShowContent bool
var contextAllAgents bool
var contextTraceID string

var contextCmd = &cobra.Command{
	Use:   "context",
	Short: "Inspect and maintain the canonical context store",
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
	Short: "Search canonical context using runtime-safe visibility by default",
	Args:  cobra.ExactArgs(1),
	RunE:  runContextQuery,
}

var contextListCmd = &cobra.Command{
	Use:   "list",
	Short: "List canonical context metadata without exposing memory content",
	Args:  cobra.NoArgs,
	RunE:  runContextList,
}

var contextExplainCmd = &cobra.Command{
	Use:   "explain --trace <trace-id>",
	Short: "Show redacted metadata for a ContextCompiler shadow trace",
	Args:  cobra.NoArgs,
	RunE:  runContextExplain,
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
	contextQueryCmd.Flags().StringVar(&contextAgent, "agent", "", "Worker memory ID; includes that worker's private records and shared ancestors")
	contextQueryCmd.Flags().StringVar(&contextTier, "tier", "", "Memory tier filter: session or persistent")
	contextQueryCmd.Flags().StringVar(&contextLifecycle, "lifecycle", "", "Lifecycle filter: candidate, confirmed, or rejected")
	contextQueryCmd.Flags().BoolVar(&contextQueryJSON, "json", false, "Emit JSON")
	contextQueryCmd.Flags().BoolVar(&contextShowContent, "show-content", false, "Include safely redacted content (otherwise IDs and metadata only)")
	contextCmd.AddCommand(contextQueryCmd)
	addContextReadFlags(contextListCmd, true)
	contextCmd.AddCommand(contextListCmd)
	contextExplainCmd.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace directory containing context shadow traces")
	contextExplainCmd.Flags().StringVar(&contextTraceID, "trace", "", "Shadow trace ID (required)")
	contextExplainCmd.Flags().BoolVar(&contextQueryJSON, "json", false, "Emit JSON")
	contextCmd.AddCommand(contextExplainCmd)
	contextRebuildCmd.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace directory containing context.sqlite")
	contextCmd.AddCommand(contextRebuildCmd)
}

func addContextReadFlags(cmd *cobra.Command, includeAllAgents bool) {
	cmd.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace directory containing context.sqlite")
	cmd.Flags().StringVar(&contextProject, "project", "", "Canonical project ID (required)")
	cmd.Flags().StringVar(&contextTeam, "team", "", "Optional team scope")
	cmd.Flags().StringVar(&contextAgent, "agent", "", "Worker memory ID")
	cmd.Flags().StringVar(&contextTier, "tier", "", "Memory tier filter: session or persistent")
	cmd.Flags().StringVar(&contextLifecycle, "lifecycle", "", "Lifecycle filter: candidate, confirmed, or rejected")
	cmd.Flags().BoolVar(&contextQueryJSON, "json", false, "Emit JSON")
	cmd.Flags().BoolVar(&contextShowContent, "show-content", false, "Include safely redacted content (otherwise IDs and metadata only)")
	if includeAllAgents {
		cmd.Flags().BoolVar(&contextAllAgents, "all-agents", false, "Maintenance view of private records for all workers")
	}
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
	if err := validateContextReadFilters(false); err != nil {
		return err
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return err
	}
	defer repo.Close()
	var vector contextstore.VectorSearcher
	vectorStore, vectorErr := contextstore.OpenOllamaVectorStore(getContextWorkspace(), config.ResolveEmbeddingModel(""), config.DefaultOllamaAPIURL)
	if vectorErr == nil {
		vectorErr = vectorStore.Rebuild(cmd.Context(), repo, contextReadScope())
	}
	if vectorErr == nil {
		vector = vectorStore
	}
	visibility := contextstore.VisibilityAncestors
	// Session-tier memory has a session child scope. Querying it requires an
	// explicit worker identity plus the tier filter; only then may the CLI use
	// the controlled maintenance subtree view to inspect that worker's session
	// records. The normal query path remains runtime-equivalent ancestors only.
	if contextTier == "session" && contextAgent != "" {
		visibility = contextstore.VisibilitySubtree
	}
	results, trace, err := contextstore.HybridRetrieve(cmd.Context(), repo, vector, contextstore.SearchRequest{Query: args[0], Scope: contextReadScope(), Visibility: visibility, IncludeCandidates: contextLifecycle != "" && contextLifecycle != string(contextstore.LifecycleConfirmed), Limit: 20})
	if err != nil {
		return err
	}
	results = filterContextResults(results)
	return writeContextSearchOutput(cmd, results, trace)
}

const contextOutputSchemaVersion = 1

type contextItemOutput struct {
	ID          string                        `json:"id"`
	Kind        contextstore.ContextKind      `json:"kind"`
	Scope       contextstore.Scope            `json:"scope"`
	Tier        string                        `json:"tier,omitempty"`
	Lifecycle   contextstore.ContextLifecycle `json:"lifecycle"`
	Confidence  float64                       `json:"confidence"`
	SourceType  string                        `json:"source_type,omitempty"`
	SourceRef   string                        `json:"source_ref,omitempty"`
	EvidenceIDs []string                      `json:"evidence_ids,omitempty"`
	CreatedAt   time.Time                     `json:"created_at"`
	UpdatedAt   time.Time                     `json:"updated_at"`
	Score       float64                       `json:"score,omitempty"`
	Content     string                        `json:"content,omitempty"`
}

type contextOutputStats struct {
	ResultCount           int  `json:"result_count"`
	RetrievalInsufficient bool `json:"retrieval_insufficient,omitempty"`
}

type contextReadOutput struct {
	SchemaVersion int                 `json:"schema_version"`
	Results       []contextItemOutput `json:"results"`
	Stats         contextOutputStats  `json:"stats"`
}

func validateContextReadFilters(allowAllAgents bool) error {
	if contextProject == "" {
		return fmt.Errorf("--project is required")
	}
	if contextTier != "" && contextTier != "session" && contextTier != "persistent" {
		return fmt.Errorf("--tier must be session or persistent")
	}
	if contextLifecycle != "" && contextLifecycle != string(contextstore.LifecycleCandidate) && contextLifecycle != string(contextstore.LifecycleConfirmed) && contextLifecycle != string(contextstore.LifecycleRejected) {
		return fmt.Errorf("--lifecycle must be candidate, confirmed, or rejected")
	}
	if contextAllAgents && !allowAllAgents {
		return fmt.Errorf("--all-agents is only available to context list")
	}
	return nil
}

func contextReadScope() contextstore.Scope {
	return contextstore.Scope{ProjectID: contextProject, TeamID: contextTeam, AgentID: contextAgent}
}

func contextItemTier(item contextstore.ContextItem) string {
	if item.Metadata != nil && (item.Metadata["memory_tier"] == "session" || item.Metadata["memory_tier"] == "persistent") {
		return item.Metadata["memory_tier"]
	}
	return ""
}

func contextItemOutputFor(item contextstore.ContextItem, score float64) contextItemOutput {
	evidenceIDs := make([]string, 0, len(item.Evidence))
	for _, evidence := range item.Evidence {
		if evidence.ItemID != "" {
			evidenceIDs = append(evidenceIDs, evidence.ItemID)
		}
	}
	sort.Strings(evidenceIDs)
	out := contextItemOutput{ID: item.ID, Kind: item.Kind, Scope: item.Scope, Tier: contextItemTier(item), Lifecycle: item.Lifecycle, Confidence: item.Confidence, SourceType: item.Source.Type, SourceRef: utils.RedactSecrets(item.Source.Ref), EvidenceIDs: evidenceIDs, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, Score: score}
	if contextShowContent {
		out.Content = utils.RedactSecrets(item.Content)
	}
	return out
}

func filterContextResults(results []contextstore.SearchResult) []contextstore.SearchResult {
	filtered := make([]contextstore.SearchResult, 0, len(results))
	for _, result := range results {
		if contextTier != "" && contextItemTier(result.Item) != contextTier {
			continue
		}
		if contextLifecycle != "" && string(result.Item.Lifecycle) != contextLifecycle {
			continue
		}
		filtered = append(filtered, result)
	}
	return filtered
}

func writeContextSearchOutput(cmd *cobra.Command, results []contextstore.SearchResult, trace contextstore.RetrievalTrace) error {
	out := contextReadOutput{SchemaVersion: contextOutputSchemaVersion, Results: make([]contextItemOutput, 0, len(results)), Stats: contextOutputStats{ResultCount: len(results), RetrievalInsufficient: trace.RetrievalInsufficient}}
	for _, result := range results {
		out.Results = append(out.Results, contextItemOutputFor(result.Item, result.Score))
	}
	if contextQueryJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}
	for _, result := range out.Results {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%.4f\n", result.ID, result.Tier, result.Lifecycle, result.Score); err != nil {
			return err
		}
		if result.Content != "" {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", result.Content); err != nil {
				return err
			}
		}
	}
	if trace.RetrievalInsufficient {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "context query: retrieval insufficient")
		return err
	}
	return nil
}

func runContextList(cmd *cobra.Command, _ []string) error {
	if err := validateContextReadFilters(true); err != nil {
		return err
	}
	if !contextAllAgents && contextAgent == "" && (contextTier != "" || contextLifecycle != "") {
		return fmt.Errorf("--agent or --all-agents is required when filtering worker memory")
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return err
	}
	defer repo.Close()
	visibility := contextstore.VisibilityAncestors
	if contextAgent != "" || contextAllAgents {
		visibility = contextstore.VisibilitySubtree
	}
	items, err := repo.Query(cmd.Context(), contextstore.RepositoryQuery{Scope: contextReadScope(), Visibility: visibility, IncludeCandidates: contextLifecycle != "" && contextLifecycle != string(contextstore.LifecycleConfirmed), Limit: 1000})
	if err != nil {
		return err
	}
	results := make([]contextstore.SearchResult, 0, len(items))
	for _, item := range items {
		// A list of worker memory never reports shared context as worker memory.
		if item.Metadata["visibility"] != "private" {
			continue
		}
		if contextAgent != "" && item.Scope.AgentID != contextAgent {
			continue
		}
		results = append(results, contextstore.SearchResult{Item: item})
	}
	results = filterContextResults(results)
	return writeContextSearchOutput(cmd, results, contextstore.RetrievalTrace{})
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

// runContextExplain reads the same bounded shadow-trace format as inspect,
// but resolves an explicit trace ID. Shadow traces intentionally contain no
// prompt or retrieved-item content, so this is safe to expose to operators.
func runContextExplain(cmd *cobra.Command, _ []string) error {
	if strings.TrimSpace(contextTraceID) == "" {
		return fmt.Errorf("--trace is required")
	}
	path := filepath.Join(getContextWorkspace(), "context-shadow-traces.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("context trace %q was not found", contextTraceID)
		}
		return fmt.Errorf("opening context shadow traces: %w", err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		var trace contextShadowTrace
		if json.Unmarshal(s.Bytes(), &trace) != nil || trace.ID != contextTraceID {
			continue
		}
		if contextQueryJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				SchemaVersion int                `json:"schema_version"`
				Trace         contextShadowTrace `json:"trace"`
			}{SchemaVersion: contextOutputSchemaVersion, Trace: trace})
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "context explain: trace %s (%s)\nlegacy tokens: %d\ncanonical tokens: %d / budget %d\nselected items: %d\nmissing anchors: %d\n", trace.ID, trace.Kind, trace.LegacyTokens, trace.CanonicalTokens, trace.BudgetTokens, trace.SelectedItems, len(trace.MissingAnchors))
		return err
	}
	if err := s.Err(); err != nil {
		return fmt.Errorf("reading context shadow traces: %w", err)
	}
	return fmt.Errorf("context trace %q was not found", contextTraceID)
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
