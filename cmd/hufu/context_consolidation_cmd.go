package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

var contextApplyProposal bool
var contextProposalText string
var contextProposalSources string
var contextConsolidateDraft bool
var contextConsolidateModel, contextConsolidateSearchPath string

var contextConsolidationCmd = &cobra.Command{Use: "consolidation", Short: "Review memory consolidation proposals"}
var contextConsolidationShowCmd = &cobra.Command{Use: "show <proposal-id>", Args: cobra.ExactArgs(1), RunE: runContextConsolidationShow}
var contextConsolidationApproveCmd = &cobra.Command{Use: "approve <proposal-id>", Args: cobra.ExactArgs(1), RunE: runContextConsolidationApprove}
var contextConsolidationRejectCmd = &cobra.Command{Use: "reject <proposal-id>", Args: cobra.ExactArgs(1), RunE: runContextConsolidationReject}

func init() {
	contextConsolidateCmd.Flags().BoolVar(&contextApplyProposal, "apply-proposal", false, "Persist a validated candidate proposal; never confirms it")
	contextConsolidateCmd.Flags().StringVar(&contextProposalText, "proposal-text", "", "Candidate text produced by the proposal stage (required with --apply-proposal)")
	contextConsolidateCmd.Flags().StringVar(&contextProposalSources, "source", "", "Comma-separated source ContextItem IDs (required with --apply-proposal)")
	contextConsolidateCmd.Flags().StringVar(&contextPolicyVersion, "policy-version", "memory-policy-v1", "Memory policy version for aggregate revisions")
	contextConsolidateCmd.Flags().BoolVar(&contextConsolidateDraft, "draft", false, "Draft the candidate text with the team sidecar model (requires --apply-proposal, --source, and --team; excludes --proposal-text)")
	contextConsolidateCmd.Flags().StringVar(&contextConsolidateModel, "model", "", "Model used with --draft")
	contextConsolidateCmd.Flags().StringVar(&contextConsolidateSearchPath, "team-search-path", "", "Comma-separated team search paths used with --draft")
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
	if err := validateConsolidateDraftFlags(); err != nil {
		return err
	}
	repo, err := openExistingContextRepository(getContextWorkspace())
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	if !contextApplyProposal {
		builder := newConsolidationClusterBuilder()
		if iterateErr := repo.Iterate(cmd.Context(), contextstore.RepositoryQuery{Scope: contextReadScope(), Visibility: contextstore.VisibilitySubtree}, builder.Add); iterateErr != nil {
			return iterateErr
		}
		clusters := builder.Clusters()
		conflicted, conflictErr := conflictedClusterIDs(cmd.Context(), repo, builder, clusters)
		if conflictErr != nil {
			return conflictErr
		}
		if contextQueryJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"dry_run": true, "clusters": clusters, "conflicted_ids": conflicted})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "context consolidate dry-run: %d eligible cluster(s); use --apply-proposal --source <ids> --proposal-text <text> to persist a candidate; %d item(s) with unresolved conflicts\n", len(clusters), len(conflicted))
		return err
	}
	if contextConsolidateDraft {
		return runContextConsolidateDraft(cmd, repo)
	}
	if strings.TrimSpace(contextProposalText) == "" || strings.TrimSpace(contextProposalSources) == "" {
		return fmt.Errorf("--proposal-text and --source are required with --apply-proposal")
	}
	if utils.RedactSecrets(contextProposalText) != contextProposalText {
		return fmt.Errorf("proposal text contains secret-like material")
	}
	_, ids, err := loadConsolidationSources(cmd, repo)
	if err != nil {
		return err
	}
	return persistConsolidationProposal(cmd, repo, ids, contextProposalText, "operator", "")
}

// consolidationSelection is the source selection named by --source under the
// memory learning policy's support thresholds.
func consolidationSelection() contextstore.ConsolidationSourceSelection {
	policy := agent.DefaultMemoryLearningPolicy()
	return contextstore.ConsolidationSourceSelection{
		ProjectID: contextProject, TeamID: contextTeam, SourceIDs: splitConsolidationIDs(contextProposalSources),
		PolicyVersion: contextPolicyVersion,
		Support:       contextstore.ConsolidationSupportPolicy{MinConfirmedSupport: policy.MinConfirmedSupport, MinIndependentTasks: policy.MinIndependentTasks},
	}
}

// loadConsolidationSources resolves --source and applies every source gate
// shared by the operator and model drafting paths. The returned IDs are
// sorted. CreateConsolidationProposal repeats the gates in its transaction.
func loadConsolidationSources(cmd *cobra.Command, repo *contextstore.SQLiteRepository) ([]contextstore.ContextItem, []string, error) {
	return repo.ValidateConsolidationSources(cmd.Context(), consolidationSelection())
}

// persistConsolidationProposal stores text as a candidate derived from the
// sources plus a pending proposal in one transaction, then records the
// idempotent proposal event. origin is "operator" for --proposal-text or
// "model" for --draft (with draftModel). Rerunning the same proposal records a
// missed event without writing context records again.
func persistConsolidationProposal(cmd *cobra.Command, repo *contextstore.SQLiteRepository, ids []string, text, origin, draftModel string) error {
	selection := consolidationSelection()
	selection.SourceIDs = ids
	proposal, created, err := repo.CreateConsolidationProposal(cmd.Context(), contextstore.ConsolidationCreateInput{ConsolidationSourceSelection: selection, Text: text, Origin: origin, DraftModel: draftModel})
	if err != nil {
		return err
	}
	eventStore, err := team.OpenEventStore(getContextWorkspace())
	if err != nil {
		return fmt.Errorf("open event store for consolidation telemetry: %w", err)
	}
	defer func() { _ = eventStore.Close() }()
	eventPayload, err := json.Marshal(map[string]any{"schema_version": 1, "proposal_id": proposal.ID, "candidate_context_item_id": proposal.CandidateContextItemID, "source_ids": proposal.SourceIDs, "policy_version": contextPolicyVersion, "proposal_origin": origin})
	if err != nil {
		return err
	}
	if err := eventStore.Append(team.RunEvent{Type: "memory_consolidation_proposed", Actor: "maintenance", IdempotencyKey: "memory:consolidation_proposed:" + proposal.ID, Payload: eventPayload}); err != nil {
		return fmt.Errorf("record consolidation proposal: %w", err)
	}
	pending := "explicit approval required"
	if !created {
		pending = "already pending; explicit approval required"
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context consolidate: proposal=%s candidate=%s status=proposed (%s)\n", proposal.ID, proposal.CandidateContextItemID, pending)
	return err
}

type consolidationClusterBuilder struct {
	groups map[string][]string
	scopes map[string]contextstore.Scope
}

func newConsolidationClusterBuilder() *consolidationClusterBuilder {
	return &consolidationClusterBuilder{groups: make(map[string][]string), scopes: make(map[string]contextstore.Scope)}
}

func (b *consolidationClusterBuilder) Add(item contextstore.ContextItem) error {
	if item.Lifecycle != contextstore.LifecycleConfirmed || item.SupersededBy != "" {
		return nil
	}
	key := item.Scope.ProjectID + "\x00" + item.Scope.TeamID + "\x00" + item.Scope.AgentID + "\x00" + string(item.Kind) + "\x00" + consolidationSignature(item)
	b.groups[key] = append(b.groups[key], item.ID)
	b.scopes[item.ID] = item.Scope
	return nil
}

func (b *consolidationClusterBuilder) Clusters() [][]string {
	var clusters [][]string
	for _, ids := range b.groups {
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

// refuseConsolidationCandidates points generic confirm/reject at the
// consolidation review commands, which revalidate the proposal atomically.
func refuseConsolidationCandidates(items []contextstore.ContextItem) error {
	for _, item := range items {
		if contextstore.IsReservedSourceType(item.Source.Type) {
			return fmt.Errorf("context item %q belongs to consolidation proposal %q; use \"hufu context consolidation approve|reject %s\"", item.ID, item.Source.Ref, item.Source.Ref)
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

func loadConsolidationRepo(cmd *cobra.Command, id string) (*contextstore.SQLiteRepository, contextstore.ConsolidationProposal, error) {
	if contextProject == "" {
		return nil, contextstore.ConsolidationProposal{}, fmt.Errorf("--project is required")
	}
	repo, err := openExistingContextRepository(getContextWorkspace())
	if err != nil {
		return nil, contextstore.ConsolidationProposal{}, err
	}
	proposal, err := repo.GetConsolidationProposal(cmd.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && proposal.ProjectID != contextProject) {
		err = fmt.Errorf("%w: %q in project %q", contextstore.ErrConsolidationNotFound, id, contextProject)
	}
	if err != nil {
		_ = repo.Close()
		return nil, contextstore.ConsolidationProposal{}, err
	}
	return repo, proposal, nil
}

func runContextConsolidationShow(cmd *cobra.Command, args []string) error {
	repo, proposal, err := loadConsolidationRepo(cmd, args[0])
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	freshness, err := repo.EvaluateConsolidationProposal(cmd.Context(), proposal, contextPolicyVersion, false)
	if err != nil {
		return err
	}
	if contextQueryJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			contextstore.ConsolidationProposal
			Freshness contextstore.ConsolidationFreshness `json:"freshness"`
		}{proposal, freshness})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "proposal: %s\nstatus: %s\ncandidate: %s\nsources: %s\nfreshness: %s reasons=%s\n", proposal.ID, proposal.Status, proposal.CandidateContextItemID, strings.Join(proposal.SourceIDs, ","), freshness.State, consolidationReasonList(freshness.Reasons))
	return err
}

func consolidationReviewInput(id, actor, reason string) contextstore.ConsolidationReviewInput {
	return contextstore.ConsolidationReviewInput{ProposalID: id, ProjectID: contextProject, PolicyVersion: contextPolicyVersion, Actor: actor, Reason: reason}
}

func runContextConsolidationApprove(cmd *cobra.Command, args []string) error {
	repo, proposal, err := loadConsolidationRepo(cmd, args[0])
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	if _, err := repo.ApproveConsolidationProposal(cmd.Context(), consolidationReviewInput(proposal.ID, "hufu context consolidation approve", "explicit operator approval")); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context consolidation approve: %s confirmed candidate %s\n", proposal.ID, proposal.CandidateContextItemID)
	return err
}

func runContextConsolidationReject(cmd *cobra.Command, args []string) error {
	repo, proposal, err := loadConsolidationRepo(cmd, args[0])
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	if _, err := repo.RejectConsolidationProposal(cmd.Context(), consolidationReviewInput(proposal.ID, "hufu context consolidation reject", "explicit operator rejection")); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context consolidation reject: %s\n", proposal.ID)
	return err
}
