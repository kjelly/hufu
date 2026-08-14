package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

var contextApplyProposal bool
var contextProposalText string
var contextProposalSources string

var contextConsolidationCmd = &cobra.Command{Use: "consolidation", Short: "Review memory consolidation proposals"}
var contextConsolidationShowCmd = &cobra.Command{Use: "show <proposal-id>", Args: cobra.ExactArgs(1), RunE: runContextConsolidationShow}
var contextConsolidationApproveCmd = &cobra.Command{Use: "approve <proposal-id>", Args: cobra.ExactArgs(1), RunE: runContextConsolidationApprove}
var contextConsolidationRejectCmd = &cobra.Command{Use: "reject <proposal-id>", Args: cobra.ExactArgs(1), RunE: runContextConsolidationReject}

func init() {
	contextConsolidateCmd.Flags().BoolVar(&contextApplyProposal, "apply-proposal", false, "Persist a validated candidate proposal; never confirms it")
	contextConsolidateCmd.Flags().StringVar(&contextProposalText, "proposal-text", "", "Candidate text produced by the proposal stage (required with --apply-proposal)")
	contextConsolidateCmd.Flags().StringVar(&contextProposalSources, "source", "", "Comma-separated source ContextItem IDs (required with --apply-proposal)")
	contextConsolidateCmd.Flags().StringVar(&contextPolicyVersion, "policy-version", "memory-policy-v1", "Memory policy version for aggregate revisions")
	for _, command := range []*cobra.Command{contextConsolidationShowCmd, contextConsolidationApproveCmd, contextConsolidationRejectCmd} {
		command.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace containing context.sqlite")
		command.Flags().StringVar(&contextProject, "project", "", "Canonical project ID (required)")
		command.Flags().BoolVar(&contextQueryJSON, "json", false, "Emit JSON")
		command.Flags().StringVar(&contextPolicyVersion, "policy-version", "memory-policy-v1", "Memory policy version for aggregate revision checks")
		contextConsolidationCmd.AddCommand(command)
	}
	contextCmd.AddCommand(contextConsolidationCmd)
}

func runContextConsolidateProposals(cmd *cobra.Command) error {
	if err := validateContextReadFilters(true); err != nil {
		return err
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	if !contextApplyProposal {
		items, queryErr := repo.Query(cmd.Context(), contextstore.RepositoryQuery{Scope: contextReadScope(), Visibility: contextstore.VisibilitySubtree, Limit: 100000})
		if queryErr != nil {
			return queryErr
		}
		clusters := consolidationClusters(items)
		if contextQueryJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"dry_run": true, "clusters": clusters})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "context consolidate dry-run: %d eligible cluster(s); use --apply-proposal --source <ids> --proposal-text <text> to persist a candidate\n", len(clusters))
		return err
	}
	if strings.TrimSpace(contextProposalText) == "" || strings.TrimSpace(contextProposalSources) == "" {
		return fmt.Errorf("--proposal-text and --source are required with --apply-proposal")
	}
	if utils.RedactSecrets(contextProposalText) != contextProposalText {
		return fmt.Errorf("proposal text contains secret-like material")
	}
	ids := splitConsolidationIDs(contextProposalSources)
	if len(ids) < 2 {
		return fmt.Errorf("consolidation requires at least two source items")
	}
	sources, err := repo.GetMany(cmd.Context(), ids)
	if err != nil {
		return err
	}
	if err := validateConsolidationSources(sources, contextProject, contextTeam); err != nil {
		return err
	}
	policy := agent.DefaultMemoryLearningPolicy()
	policy.PolicyVersion = contextPolicyVersion
	if err := validateConsolidationSupport(cmd, repo, sources, policy); err != nil {
		return err
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00") + "\x00" + contextProposalText))
	proposalID := "consolidation-" + hex.EncodeToString(sum[:10])
	candidateID := "ctx-consolidated-" + hex.EncodeToString(sum[10:20])
	metadata := map[string]string{"derived_from": strings.Join(ids, ","), "consolidation_proposal": proposalID}
	candidate, err := repo.UpsertCandidate(cmd.Context(), contextstore.ContextItem{ID: candidateID, Kind: sources[0].Kind, Content: contextProposalText, Scope: sources[0].Scope, Authority: sources[0].Authority, TrustLevel: contextstore.TrustInternal, Priority: sources[0].Priority, Confidence: minimumSourceConfidence(sources), Lifecycle: contextstore.LifecycleCandidate, Source: contextstore.SourceRef{Type: "consolidation_proposal", Ref: proposalID}, Metadata: metadata})
	if err != nil {
		return err
	}
	revisions, aggregateRevisions := map[string]string{}, map[string]int64{}
	edges := make([]contextstore.ContextEdge, 0, len(sources))
	for _, source := range sources {
		revisions[source.ID] = source.ContentHash
		if aggregate, aggregateErr := repo.ExperienceAggregate(cmd.Context(), source.ID, contextPolicyVersion); aggregateErr == nil {
			aggregateRevisions[source.ID] = aggregate.Revision
		}
		edges = append(edges, contextstore.ContextEdge{FromID: candidate.ID, Relation: "derived_from", ToID: source.ID})
	}
	if err := repo.AddEdges(cmd.Context(), edges...); err != nil {
		return err
	}
	proposal := contextstore.ConsolidationProposal{ID: proposalID, ProjectID: contextProject, TeamID: contextTeam, CandidateContextItemID: candidate.ID, SourceIDs: ids, SourceRevisions: revisions, AggregateRevisions: aggregateRevisions, Status: "proposed", CreatedAt: time.Now().UTC()}
	if err := repo.SaveConsolidationProposal(cmd.Context(), proposal); err != nil {
		return err
	}
	eventStore, err := team.OpenEventStore(getContextWorkspace())
	if err != nil {
		return fmt.Errorf("open event store for consolidation telemetry: %w", err)
	}
	defer func() { _ = eventStore.Close() }()
	eventPayload, err := json.Marshal(map[string]any{"schema_version": 1, "proposal_id": proposal.ID, "candidate_context_item_id": candidate.ID, "source_ids": ids, "policy_version": contextPolicyVersion})
	if err != nil {
		return err
	}
	if err := eventStore.Append(team.RunEvent{Type: "memory_consolidation_proposed", Actor: "maintenance", IdempotencyKey: "memory:consolidation_proposed:" + proposal.ID, Payload: eventPayload}); err != nil {
		return fmt.Errorf("record consolidation proposal: %w", err)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context consolidate: proposal=%s candidate=%s status=proposed (explicit approval required)\n", proposal.ID, candidate.ID)
	return err
}

func consolidationClusters(items []contextstore.ContextItem) [][]string {
	groups := map[string][]string{}
	for _, item := range items {
		if item.Lifecycle != contextstore.LifecycleConfirmed || item.SupersededBy != "" {
			continue
		}
		key := item.Scope.ProjectID + "\x00" + item.Scope.TeamID + "\x00" + item.Scope.AgentID + "\x00" + string(item.Kind) + "\x00" + consolidationSignature(item)
		groups[key] = append(groups[key], item.ID)
	}
	var clusters [][]string
	for _, ids := range groups {
		if len(ids) >= 2 {
			sort.Strings(ids)
			clusters = append(clusters, ids)
		}
	}
	sort.Slice(clusters, func(i, j int) bool { return strings.Join(clusters[i], "\x00") < strings.Join(clusters[j], "\x00") })
	return clusters
}

func consolidationSignature(item contextstore.ContextItem) string {
	for _, key := range []string{"action_fingerprint", "tool_signature", "file_evidence"} {
		if value := strings.TrimSpace(item.Metadata[key]); value != "" {
			return key + ":" + value
		}
	}
	words := strings.Fields(strings.ToLower(item.Content))
	if len(words) > 6 {
		words = words[:6]
	}
	sort.Strings(words)
	digest := sha256.Sum256([]byte(strings.Join(words, "\x00")))
	return "semantic:" + hex.EncodeToString(digest[:8])
}

func validateConsolidationSupport(cmd *cobra.Command, repo *contextstore.SQLiteRepository, sources []contextstore.ContextItem, policy agent.MemoryLearningPolicy) error {
	for _, source := range sources {
		aggregate, err := repo.ExperienceAggregate(cmd.Context(), source.ID, policy.PolicyVersion)
		if err != nil {
			return fmt.Errorf("source %q has no verified experience aggregate under policy %q", source.ID, policy.PolicyVersion)
		}
		if aggregate.VerifiedSupportCount < policy.MinConfirmedSupport || aggregate.IndependentTaskCount < policy.MinIndependentTasks || aggregate.CausalFailureCount > 0 {
			return fmt.Errorf("source %q lacks stable verified cross-task support", source.ID)
		}
	}
	return nil
}

func validateConsolidationSources(items []contextstore.ContextItem, projectID, teamID string) error {
	if len(items) < 2 {
		return fmt.Errorf("at least two source items are required")
	}
	first := items[0]
	selected := map[string]bool{}
	for _, item := range items {
		selected[item.ID] = true
	}
	for _, item := range items {
		if item.Lifecycle != contextstore.LifecycleConfirmed || item.SupersededBy != "" {
			return fmt.Errorf("source %q is not current confirmed knowledge", item.ID)
		}
		if item.Scope.ProjectID != projectID || item.Scope.TeamID != teamID || item.Scope != first.Scope || item.Kind != first.Kind {
			return fmt.Errorf("source %q would widen or mix scope/kind", item.ID)
		}
		for _, contradiction := range splitConsolidationIDs(item.Metadata["contradicts_ids"]) {
			if selected[contradiction] {
				return fmt.Errorf("contradictory sources %q and %q cannot be merged", item.ID, contradiction)
			}
		}
	}
	return nil
}

func splitConsolidationIDs(value string) []string {
	var result []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func minimumSourceConfidence(items []contextstore.ContextItem) float64 {
	value := 1.0
	for _, item := range items {
		if item.Confidence < value {
			value = item.Confidence
		}
	}
	return value
}

func loadConsolidationRepo(cmd *cobra.Command, id string) (*contextstore.SQLiteRepository, contextstore.ConsolidationProposal, error) {
	if contextProject == "" {
		return nil, contextstore.ConsolidationProposal{}, fmt.Errorf("--project is required")
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return nil, contextstore.ConsolidationProposal{}, err
	}
	proposal, err := repo.GetConsolidationProposal(cmd.Context(), id)
	if err != nil || proposal.ProjectID != contextProject {
		_ = repo.Close()
		if err == nil {
			err = fmt.Errorf("proposal %q is outside project %q", id, contextProject)
		}
		return nil, proposal, err
	}
	return repo, proposal, nil
}

func runContextConsolidationShow(cmd *cobra.Command, args []string) error {
	repo, proposal, err := loadConsolidationRepo(cmd, args[0])
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	if contextQueryJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(proposal)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "proposal: %s\nstatus: %s\ncandidate: %s\nsources: %s\n", proposal.ID, proposal.Status, proposal.CandidateContextItemID, strings.Join(proposal.SourceIDs, ","))
	return err
}

func runContextConsolidationApprove(cmd *cobra.Command, args []string) error {
	repo, proposal, err := loadConsolidationRepo(cmd, args[0])
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	if proposal.Status != "proposed" {
		return fmt.Errorf("proposal %q status is %s", proposal.ID, proposal.Status)
	}
	if err := validateConsolidationProposalCurrent(cmd, repo, proposal); err != nil {
		return err
	}
	if err := repo.ConfirmCandidates(cmd.Context(), []string{proposal.CandidateContextItemID}, contextstore.CandidateBinding{Evidence: contextstore.EvidenceRef{Type: "operator_approval", Ref: proposal.ID}, Metadata: map[string]string{"approved_by": "hufu context consolidation approve"}}); err != nil {
		return err
	}
	if err := repo.UpdateConsolidationProposal(cmd.Context(), proposal.ID, "approved", "explicit operator approval"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context consolidation approve: %s confirmed candidate %s\n", proposal.ID, proposal.CandidateContextItemID)
	return err
}

func validateConsolidationProposalCurrent(cmd *cobra.Command, repo *contextstore.SQLiteRepository, proposal contextstore.ConsolidationProposal) error {
	sources, err := repo.GetMany(cmd.Context(), proposal.SourceIDs)
	if err != nil {
		return err
	}
	if err := validateConsolidationSources(sources, proposal.ProjectID, proposal.TeamID); err != nil {
		return fmt.Errorf("proposal source validation changed: %w", err)
	}
	for _, source := range sources {
		if proposal.SourceRevisions[source.ID] != source.ContentHash {
			return fmt.Errorf("proposal source %q revision changed; create a new proposal", source.ID)
		}
		currentRevision := int64(0)
		aggregate, aggregateErr := repo.ExperienceAggregate(cmd.Context(), source.ID, contextPolicyVersion)
		if aggregateErr == nil {
			currentRevision = aggregate.Revision
		} else if !errors.Is(aggregateErr, sql.ErrNoRows) {
			return aggregateErr
		}
		if proposal.AggregateRevisions[source.ID] != currentRevision {
			return fmt.Errorf("proposal source %q aggregate revision changed; create a new proposal", source.ID)
		}
	}
	return nil
}

func runContextConsolidationReject(cmd *cobra.Command, args []string) error {
	repo, proposal, err := loadConsolidationRepo(cmd, args[0])
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	if err := repo.UpdateLifecycle(cmd.Context(), []string{proposal.CandidateContextItemID}, contextstore.LifecycleRejected); err != nil {
		return err
	}
	if err := repo.UpdateConsolidationProposal(cmd.Context(), proposal.ID, "rejected", "explicit operator rejection"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context consolidation reject: %s\n", proposal.ID)
	return err
}
