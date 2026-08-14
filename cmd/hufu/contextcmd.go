package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/team"
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
var contextEvidence string
var contextReason string
var contextSupersedeWith string
var contextRebuildVector bool
var contextRebuildAggregates bool
var contextLegacyProject string
var contextMigrateApply bool

var contextCmd = &cobra.Command{
	Use:   "context",
	Short: "Inspect and maintain the canonical context store",
}

var contextRepairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Retry redacted canonical context writes left pending by a transient store failure",
	Long: `Canonical context writes fail closed at their caller. A failed derived
write is recorded, redacted, in <workspace>/context-pending.jsonl so this
command can replay it after the store is healthy. It removes successful
entries and retains failures for a later retry. It is safe to run repeatedly.`,
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

var contextConfirmCmd = &cobra.Command{
	Use:   "confirm <id...>",
	Short: "Confirm candidate context records with sealed evidence",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runContextConfirm,
}

var contextRejectCmd = &cobra.Command{
	Use:   "reject <id...>",
	Short: "Reject candidate context records with an operator reason",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runContextReject,
}

var contextSupersedeCmd = &cobra.Command{
	Use:   "supersede <old-id...>",
	Short: "Mark current confirmed records superseded by a confirmed revision",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runContextSupersede,
}

var contextMigrateMemoryCmd = &cobra.Command{
	Use:   "migrate-memory",
	Short: "Import legacy MemoryRecord documents into canonical context (dry-run by default)",
	Args:  cobra.NoArgs,
	RunE:  runContextMigrateMemory,
}

var contextShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Show one canonical context record (content remains redacted by default)",
	Args:  cobra.ExactArgs(1),
	RunE:  runContextShow,
}

var contextCandidatesCmd = &cobra.Command{
	Use:   "candidates",
	Short: "List reviewable canonical context candidates",
	Args:  cobra.NoArgs,
	RunE:  runContextCandidates,
}

var contextHistoryCmd = &cobra.Command{
	Use:   "history <id>",
	Short: "Show a context record's supersession chain and evidence metadata",
	Args:  cobra.ExactArgs(1),
	RunE:  runContextHistory,
}

var contextConsolidateCmd = &cobra.Command{
	Use:   "consolidate",
	Short: "Report canonical knowledge consolidation candidates; never mutates without explicit supersede",
	Args:  cobra.NoArgs,
	RunE:  runContextConsolidate,
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
	contextRebuildCmd.Flags().BoolVar(&contextRebuildVector, "vector", false, "Also rebuild the disposable canonical vector index (requires --project and embedding provider access)")
	contextRebuildCmd.Flags().BoolVar(&contextRebuildAggregates, "aggregates", false, "Rebuild outcome-driven experience aggregates from event_store.jsonl")
	contextRebuildCmd.Flags().StringVar(&contextPolicyVersion, "policy-version", "memory-policy-v1", "Memory policy version used for aggregate replay")
	contextRebuildCmd.Flags().StringVar(&contextProject, "project", "", "Canonical project ID required with --vector")
	contextRebuildCmd.Flags().StringVar(&contextTeam, "team", "", "Optional team scope for --vector")
	contextCmd.AddCommand(contextRebuildCmd)
	for _, mutation := range []*cobra.Command{contextConfirmCmd, contextRejectCmd, contextSupersedeCmd} {
		mutation.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace directory containing context.sqlite")
		mutation.Flags().StringVar(&contextProject, "project", "", "Canonical project ID (required)")
		mutation.Flags().StringVar(&contextTeam, "team", "", "Optional exact team scope")
		contextCmd.AddCommand(mutation)
	}
	contextConfirmCmd.Flags().StringVar(&contextEvidence, "evidence", "", "Accepted evidence-manifest hash (required)")
	contextRejectCmd.Flags().StringVar(&contextReason, "reason", "", "Operator rejection reason (required)")
	contextSupersedeCmd.Flags().StringVar(&contextSupersedeWith, "with", "", "Confirmed replacement context ID (required)")
	contextMigrateMemoryCmd.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Destination workspace containing context.sqlite")
	contextMigrateMemoryCmd.Flags().StringVar(&contextProject, "project", "", "Canonical destination project ID (required)")
	contextMigrateMemoryCmd.Flags().StringVar(&contextTeam, "team", "", "Optional canonical destination team")
	contextMigrateMemoryCmd.Flags().StringVar(&contextLegacyProject, "legacy-project", "", "Original project path used to create the legacy MemoryStore (required)")
	contextMigrateMemoryCmd.Flags().BoolVar(&contextMigrateApply, "apply", false, "Write the import; without this flag, report the deterministic dry-run only")
	contextCmd.AddCommand(contextMigrateMemoryCmd)
	for _, readCmd := range []*cobra.Command{contextShowCmd, contextCandidatesCmd, contextHistoryCmd, contextConsolidateCmd} {
		addContextReadFlags(readCmd, true)
		contextCmd.AddCommand(readCmd)
	}
}

func legacyMemoryItem(record memory.MemoryRecord, scope contextstore.Scope) contextstore.ContextItem {
	supersedes := make([]string, 0, len(record.Supersedes))
	for _, oldID := range record.Supersedes {
		if oldID = strings.TrimSpace(oldID); oldID != "" {
			supersedes = append(supersedes, legacyMemoryContextID(oldID))
		}
	}
	item := contextstore.ContextItem{
		ID: legacyMemoryContextID(record.ID), Content: record.Content, Scope: scope,
		Authority: contextstore.AuthorityRepository, TrustLevel: contextstore.TrustInternal,
		Priority: contextstore.PriorityLow, Confidence: record.Confidence,
		Source:    contextstore.SourceRef{Type: "legacy_memory_record", Ref: record.ID},
		Metadata:  map[string]string{"legacy_memory_id": record.ID, "category": record.Category, "visibility": "shared", "memory_lifetime": "persistent", "legacy_status": record.EffectiveStatus(), "supersedes_ids": strings.Join(supersedes, "\n")},
		CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt,
	}
	if item.Confidence == 0 {
		item.Confidence = 1
	}
	if kind, err := memoryCategoryKindForMigration(record.Category); err == nil {
		item.Kind = kind
	} else {
		item.Kind = contextstore.ContextPattern
		item.Metadata["legacy_category"] = record.Category
	}
	for _, eventID := range record.SourceEventIDs {
		item.Evidence = append(item.Evidence, contextstore.EvidenceRef{Type: "legacy_event", Ref: eventID})
	}
	if record.SourceTaskID != "" {
		item.Evidence = append(item.Evidence, contextstore.EvidenceRef{Type: "task", Ref: record.SourceTaskID})
	}
	for _, path := range record.FilePaths {
		item.Evidence = append(item.Evidence, contextstore.EvidenceRef{Type: "file_path", Ref: path})
	}
	switch record.EffectiveStatus() {
	case memory.StatusCandidate:
		item.Lifecycle = contextstore.LifecycleCandidate
	case memory.StatusRejected:
		item.Lifecycle = contextstore.LifecycleRejected
	case memory.StatusSuperseded, memory.StatusExpired:
		// ContextItem encodes supersession as an edge/SupersededBy rather than
		// a lifecycle enum. Until a source record links it below, expire the
		// imported row so an orphaned historical record cannot enter recall.
		now := time.Now().UTC()
		item.ExpiresAt = &now
		item.Lifecycle = contextstore.LifecycleConfirmed
	default:
		item.Lifecycle = contextstore.LifecycleConfirmed
	}
	return item
}

func legacyMemoryContextID(legacyID string) string {
	sum := sha256.Sum256([]byte("legacy-memory\x00" + legacyID))
	return "legacy-memory-" + hex.EncodeToString(sum[:12])
}

func memoryCategoryKindForMigration(category string) (contextstore.ContextKind, error) {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "decision":
		return contextstore.ContextDecision, nil
	case "convention":
		return contextstore.ContextConvention, nil
	case "architecture":
		return contextstore.ContextArchitecture, nil
	case "issue", "error", "bug", "lesson":
		return contextstore.ContextError, nil
	case "observation", "finding", "api-discovery":
		return contextstore.ContextObservation, nil
	case "summary", "session-summary":
		return contextstore.ContextSummary, nil
	case "", "pattern":
		return contextstore.ContextPattern, nil
	default:
		return "", fmt.Errorf("unknown legacy category")
	}
}

func runContextMigrateMemory(cmd *cobra.Command, _ []string) error {
	if strings.TrimSpace(contextProject) == "" || strings.TrimSpace(contextLegacyProject) == "" {
		return fmt.Errorf("--project and --legacy-project are required")
	}
	store, err := memory.OpenExistingMemoryStore(contextLegacyProject)
	if err != nil {
		return fmt.Errorf("open legacy MemoryStore: %w", err)
	}
	defer store.Close()
	records, err := store.ExportRecords(cmd.Context())
	if err != nil {
		return fmt.Errorf("export legacy MemoryRecords: %w", err)
	}
	hash := sha256.New()
	items := make([]contextstore.ContextItem, 0, len(records))
	scope := contextstore.Scope{ProjectID: contextProject, TeamID: contextTeam}
	for _, record := range records {
		_, _ = hash.Write([]byte(record.ID + "\x00" + record.Content + "\n"))
		items = append(items, legacyMemoryItem(record, scope))
	}
	if !contextMigrateApply {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "context migrate-memory dry-run: %d records, sha256=%x; rerun with --apply to write canonical items\n", len(items), hash.Sum(nil))
		return err
	}
	dbPath := filepath.Join(getContextWorkspace(), "context.sqlite")
	backup, err := backupContextDatabase(dbPath)
	if err != nil {
		return fmt.Errorf("backup canonical context before migration: %w", err)
	}
	repo, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	if err := repo.Append(cmd.Context(), items...); err != nil {
		return fmt.Errorf("append canonical legacy import: %w", err)
	}
	canonicalIDs := make(map[string]string, len(records))
	canonicalItems := make(map[string]contextstore.ContextItem, len(records))
	for _, record := range records {
		item := legacyMemoryItem(record, scope)
		canonicalIDs[record.ID] = item.ID
		canonicalItems[record.ID] = item
	}
	for _, record := range records {
		// A candidate's supersession intent is retained in its metadata and is
		// applied atomically only if/when it is confirmed. Applying it during
		// import would make an unreviewed legacy candidate hide current truth.
		if canonicalItems[record.ID].Lifecycle != contextstore.LifecycleConfirmed || canonicalItems[record.ID].ExpiresAt != nil {
			continue
		}
		newID := canonicalIDs[record.ID]
		oldIDs := make([]string, 0, len(record.Supersedes))
		for _, oldLegacyID := range record.Supersedes {
			if oldID := canonicalIDs[oldLegacyID]; oldID != "" {
				oldIDs = append(oldIDs, oldID)
			}
		}
		if len(oldIDs) > 0 {
			if err := repo.MarkSuperseded(cmd.Context(), oldIDs, newID); err != nil {
				return fmt.Errorf("preserve legacy supersession for %q: %w", record.ID, err)
			}
		}
	}
	if err := repo.RebuildProjection(cmd.Context(), scope); err != nil {
		return fmt.Errorf("rebuild imported projections: %w", err)
	}
	if backup != "" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "context migrate-memory: backup=%s\n", backup)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context migrate-memory: imported %d records, sha256=%x\n", len(items), hash.Sum(nil))
	return err
}

func backupContextDatabase(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	backup := fmt.Sprintf("%s.pre-memory-migrate-%d", path, time.Now().UTC().UnixNano())
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		return "", err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		side, readErr := os.ReadFile(path + suffix)
		if readErr == nil {
			if err := os.WriteFile(backup+suffix, side, 0o600); err != nil {
				return "", err
			}
		}
	}
	return backup, nil
}

func openContextMutationRepo() (contextstore.Repository, error) {
	if strings.TrimSpace(contextProject) == "" {
		return nil, fmt.Errorf("--project is required")
	}
	return contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
}

func loadExactMutationItems(cmd *cobra.Command, repo contextstore.Repository, ids []string) ([]contextstore.ContextItem, error) {
	items, err := repo.GetMany(cmd.Context(), ids)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.Scope.ProjectID != contextProject || (contextTeam != "" && item.Scope.TeamID != contextTeam) {
			return nil, fmt.Errorf("context item %q is outside the requested project/team scope", item.ID)
		}
	}
	return items, nil
}

func rebuildMutationProjections(cmd *cobra.Command, repo contextstore.Repository, items []contextstore.ContextItem) error {
	seen := map[string]bool{}
	for _, item := range items {
		key := item.Scope.ProjectID + "\x00" + item.Scope.TeamID + "\x00" + item.Scope.SessionID
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := repo.RebuildProjection(cmd.Context(), item.Scope); err != nil {
			return fmt.Errorf("rebuild projections: %w", err)
		}
	}
	return nil
}

func runContextConfirm(cmd *cobra.Command, ids []string) error {
	if strings.TrimSpace(contextEvidence) == "" {
		return fmt.Errorf("--evidence is required")
	}
	repo, err := openContextMutationRepo()
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	items, err := loadExactMutationItems(cmd, repo, ids)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Lifecycle != contextstore.LifecycleCandidate {
			return fmt.Errorf("context item %q is not a candidate", item.ID)
		}
	}
	if err := repo.ConfirmCandidates(cmd.Context(), ids, contextstore.CandidateBinding{Evidence: contextstore.EvidenceRef{Type: "operator_evidence", Ref: contextEvidence}, Metadata: map[string]string{"confirmed_by": "hufu context"}}); err != nil {
		return err
	}
	if err := rebuildMutationProjections(cmd, repo, items); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context confirm: %d item(s) confirmed\n", len(items))
	return err
}

func runContextReject(cmd *cobra.Command, ids []string) error {
	if strings.TrimSpace(contextReason) == "" {
		return fmt.Errorf("--reason is required")
	}
	repo, err := openContextMutationRepo()
	if err != nil {
		return err
	}
	defer repo.Close()
	items, err := loadExactMutationItems(cmd, repo, ids)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Lifecycle != contextstore.LifecycleCandidate {
			return fmt.Errorf("context item %q is not a candidate", item.ID)
		}
	}
	if err := repo.BindCandidates(cmd.Context(), ids, contextstore.CandidateBinding{Evidence: contextstore.EvidenceRef{Type: "operator_rejection", Ref: contextReason}, Metadata: map[string]string{"rejection_reason": contextReason}}); err != nil {
		return err
	}
	if err := repo.UpdateLifecycle(cmd.Context(), ids, contextstore.LifecycleRejected); err != nil {
		return err
	}
	if err := rebuildMutationProjections(cmd, repo, items); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context reject: %d item(s) rejected\n", len(items))
	return err
}

func runContextSupersede(cmd *cobra.Command, oldIDs []string) error {
	if strings.TrimSpace(contextSupersedeWith) == "" {
		return fmt.Errorf("--with is required")
	}
	repo, err := openContextMutationRepo()
	if err != nil {
		return err
	}
	defer repo.Close()
	old, err := loadExactMutationItems(cmd, repo, oldIDs)
	if err != nil {
		return err
	}
	replacement, err := loadExactMutationItems(cmd, repo, []string{contextSupersedeWith})
	if err != nil {
		return err
	}
	if replacement[0].Lifecycle != contextstore.LifecycleConfirmed || replacement[0].SupersededBy != "" {
		return fmt.Errorf("replacement context item %q must be current confirmed knowledge", replacement[0].ID)
	}
	for _, item := range old {
		if item.Lifecycle != contextstore.LifecycleConfirmed || item.SupersededBy != "" {
			return fmt.Errorf("context item %q must be current confirmed knowledge", item.ID)
		}
		if item.Scope.ProjectID != replacement[0].Scope.ProjectID || item.Scope.TeamID != replacement[0].Scope.TeamID || item.Metadata["visibility"] != replacement[0].Metadata["visibility"] || item.Scope.AgentID != replacement[0].Scope.AgentID {
			return fmt.Errorf("context item %q does not share replacement identity", item.ID)
		}
	}
	if err := repo.MarkSuperseded(cmd.Context(), oldIDs, replacement[0].ID); err != nil {
		return err
	}
	all := append(old, replacement[0])
	if err := rebuildMutationProjections(cmd, repo, all); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context supersede: %d item(s) replaced by %s\n", len(old), replacement[0].ID)
	return err
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
	defer func() { _ = repo.Close() }()
	if contextRebuildAggregates {
		eventStore, openErr := team.OpenEventStore(getContextWorkspace())
		if openErr != nil {
			return fmt.Errorf("opening event store for aggregate rebuild: %w", openErr)
		}
		defer func() { _ = eventStore.Close() }()
		if verifyErr := eventStore.VerifyHashChain(); verifyErr != nil {
			return fmt.Errorf("verify event store before aggregate rebuild: %w", verifyErr)
		}
		events, readErr := eventStore.ReadEvents()
		if readErr != nil {
			return readErr
		}
		policy := agent.DefaultMemoryLearningPolicy()
		policy.PolicyVersion = contextPolicyVersion
		observations := team.ExperienceObservationsFromEvents(events, policy)
		if rebuildErr := repo.RebuildExperienceAggregates(cmd.Context(), observations); rebuildErr != nil {
			return fmt.Errorf("rebuilding experience aggregates: %w", rebuildErr)
		}
		keys := make([]string, 0, len(observations))
		for _, observation := range observations {
			keys = append(keys, observation.IdempotencyKey)
		}
		sort.Strings(keys)
		digest := sha256.Sum256([]byte(strings.Join(keys, "\x00")))
		payload, marshalErr := json.Marshal(map[string]any{"schema_version": 1, "policy_version": policy.PolicyVersion, "observation_count": len(observations)})
		if marshalErr != nil {
			return marshalErr
		}
		if appendErr := eventStore.Append(team.RunEvent{Type: "memory_aggregate_rebuilt", Actor: "maintenance", IdempotencyKey: "memory:aggregate_rebuilt:" + policy.PolicyVersion + ":" + hex.EncodeToString(digest[:12]), Payload: payload}); appendErr != nil {
			return fmt.Errorf("record aggregate rebuild: %w", appendErr)
		}
	}
	if err := repo.RebuildLexical(cmd.Context()); err != nil {
		return fmt.Errorf("rebuilding FTS5 index: %w", err)
	}
	if !contextRebuildVector {
		message := "context rebuild: FTS5 index rebuilt"
		if contextRebuildAggregates {
			message += "; experience aggregates rebuilt"
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), message)
		return err
	}
	if strings.TrimSpace(contextProject) == "" {
		return fmt.Errorf("--project is required with --vector")
	}
	vector, err := contextstore.OpenOllamaVectorStore(getContextWorkspace(), config.ResolveEmbeddingModel(""), config.DefaultOllamaAPIURL)
	if err != nil {
		return fmt.Errorf("opening canonical vector index: %w", err)
	}
	if err := vector.Rebuild(cmd.Context(), repo, contextstore.Scope{ProjectID: contextProject, TeamID: contextTeam}); err != nil {
		return fmt.Errorf("rebuilding canonical vector index: %w", err)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "context rebuild: FTS5 and canonical vector index rebuilt")
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
	ID                 string                        `json:"id"`
	Kind               contextstore.ContextKind      `json:"kind"`
	Scope              contextstore.Scope            `json:"scope"`
	Tier               string                        `json:"tier,omitempty"`
	Lifecycle          contextstore.ContextLifecycle `json:"lifecycle"`
	Confidence         float64                       `json:"confidence"`
	SourceType         string                        `json:"source_type,omitempty"`
	SourceRef          string                        `json:"source_ref,omitempty"`
	EvidenceIDs        []string                      `json:"evidence_ids,omitempty"`
	Evidence           []contextstore.EvidenceRef    `json:"evidence,omitempty"`
	Lifetime           string                        `json:"lifetime,omitempty"`
	SupersededBy       string                        `json:"superseded_by,omitempty"`
	ProjectionEligible bool                          `json:"projection_eligible"`
	CreatedAt          time.Time                     `json:"created_at"`
	UpdatedAt          time.Time                     `json:"updated_at"`
	Score              float64                       `json:"score,omitempty"`
	Content            string                        `json:"content,omitempty"`
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
	if item.Metadata != nil {
		if tier := item.Metadata["memory_tier"]; tier == "session" || tier == "persistent" {
			return tier
		}
		if lifetime := item.Metadata["memory_lifetime"]; lifetime == "session" || lifetime == "persistent" {
			return lifetime
		}
	}
	return ""
}

func contextItemLifetime(item contextstore.ContextItem) string {
	if item.Metadata != nil && item.Metadata["memory_lifetime"] != "" {
		return item.Metadata["memory_lifetime"]
	}
	if tier := contextItemTier(item); tier != "" {
		return tier
	}
	if item.Scope.SessionID != "" {
		return "session"
	}
	return "persistent"
}

func contextProjectionEligible(item contextstore.ContextItem) bool {
	return item.Lifecycle == contextstore.LifecycleConfirmed && item.SupersededBy == "" &&
		item.Scope.AgentID == "" && item.Scope.TaskID == "" && item.Scope.AttemptID == ""
}

func contextItemOutputFor(item contextstore.ContextItem, score float64) contextItemOutput {
	evidenceIDs := make([]string, 0, len(item.Evidence))
	for _, evidence := range item.Evidence {
		if evidence.ItemID != "" {
			evidenceIDs = append(evidenceIDs, evidence.ItemID)
		}
	}
	sort.Strings(evidenceIDs)
	evidence := make([]contextstore.EvidenceRef, 0, len(item.Evidence))
	for _, ref := range item.Evidence {
		ref.Ref = utils.RedactSecrets(ref.Ref)
		evidence = append(evidence, ref)
	}
	out := contextItemOutput{ID: item.ID, Kind: item.Kind, Scope: item.Scope, Tier: contextItemTier(item), Lifetime: contextItemLifetime(item), Lifecycle: item.Lifecycle, Confidence: item.Confidence, SourceType: item.Source.Type, SourceRef: utils.RedactSecrets(item.Source.Ref), EvidenceIDs: evidenceIDs, Evidence: evidence, SupersededBy: item.SupersededBy, ProjectionEligible: contextProjectionEligible(item), CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt, Score: score}
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
		if contextAgent != "" && item.Scope.AgentID != contextAgent {
			continue
		}
		results = append(results, contextstore.SearchResult{Item: item})
	}
	results = filterContextResults(results)
	return writeContextSearchOutput(cmd, results, contextstore.RetrievalTrace{})
}

func runContextShow(cmd *cobra.Command, args []string) error {
	if err := validateContextReadFilters(true); err != nil {
		return err
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return err
	}
	defer repo.Close()
	item, err := repo.Get(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	if item.Scope.ProjectID != contextProject || (contextTeam != "" && item.Scope.TeamID != contextTeam) {
		return fmt.Errorf("context item %q is outside the requested project/team scope", item.ID)
	}
	if item.Scope.AgentID != "" && contextAgent != item.Scope.AgentID && !contextAllAgents {
		return fmt.Errorf("private context item %q requires matching --agent or --all-agents", item.ID)
	}
	return writeContextSearchOutput(cmd, []contextstore.SearchResult{{Item: item}}, contextstore.RetrievalTrace{})
}

func runContextCandidates(cmd *cobra.Command, _ []string) error {
	if contextLifecycle != "" && contextLifecycle != string(contextstore.LifecycleCandidate) {
		return fmt.Errorf("context candidates always uses lifecycle=candidate")
	}
	previousLifecycle := contextLifecycle
	defer func() { contextLifecycle = previousLifecycle }()
	contextLifecycle = string(contextstore.LifecycleCandidate)
	return runContextList(cmd, nil)
}

func runContextHistory(cmd *cobra.Command, args []string) error {
	if err := validateContextReadFilters(true); err != nil {
		return err
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return err
	}
	defer repo.Close()
	item, err := repo.Get(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	if item.Scope.ProjectID != contextProject || (contextTeam != "" && item.Scope.TeamID != contextTeam) {
		return fmt.Errorf("context item %q is outside the requested project/team scope", item.ID)
	}
	if item.Scope.AgentID != "" && contextAgent != item.Scope.AgentID && !contextAllAgents {
		return fmt.Errorf("private context item %q requires matching --agent or --all-agents", item.ID)
	}
	all, err := repo.Query(cmd.Context(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: contextProject, TeamID: contextTeam}, Visibility: contextstore.VisibilitySubtree, IncludeCandidates: true, IncludeSuperseded: true, IncludeExpired: true, Limit: 100000})
	if err != nil {
		return err
	}
	byID := make(map[string]contextstore.ContextItem, len(all))
	for _, candidate := range all {
		byID[candidate.ID] = candidate
	}
	chain := []contextstore.SearchResult{{Item: item}}
	for next := item.SupersededBy; next != ""; {
		replacement, ok := byID[next]
		if !ok {
			break
		}
		chain = append(chain, contextstore.SearchResult{Item: replacement})
		next = replacement.SupersededBy
	}
	return writeContextSearchOutput(cmd, chain, contextstore.RetrievalTrace{})
}

func runContextConsolidate(cmd *cobra.Command, _ []string) error {
	return runContextConsolidateProposals(cmd)
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
