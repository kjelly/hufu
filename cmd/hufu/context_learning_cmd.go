package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

var contextMemoryQuery string
var contextPolicyVersion string
var contextLearningCheck bool

var contextOutcomesCmd = &cobra.Command{
	Use:   "outcomes <id>",
	Short: "Show content-free outcome and utility aggregates for one context item",
	Args:  cobra.ExactArgs(1),
	RunE:  runContextOutcomes,
}

var contextExplainMemoryCmd = &cobra.Command{
	Use:   "explain-memory <id>",
	Short: "Explain retrieval and reinforced ranking without displaying memory content",
	Args:  cobra.ExactArgs(1),
	RunE:  runContextExplainMemory,
}

var contextLearningDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check the outcome-driven memory event chain and aggregate projection",
	Args:  cobra.NoArgs,
	RunE:  runContextLearningDoctor,
}

func init() {
	for _, command := range []*cobra.Command{contextOutcomesCmd, contextExplainMemoryCmd, contextLearningDoctorCmd} {
		command.Flags().StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace containing context.sqlite and event_store.jsonl")
		command.Flags().StringVar(&contextPolicyVersion, "policy-version", "memory-policy-v1", "Memory policy version")
		command.Flags().BoolVar(&contextQueryJSON, "json", false, "Emit JSON")
		contextCmd.AddCommand(command)
	}
	contextExplainMemoryCmd.Flags().StringVar(&contextProject, "project", "", "Canonical project ID (required)")
	contextExplainMemoryCmd.Flags().StringVar(&contextTeam, "team", "", "Optional team scope")
	contextExplainMemoryCmd.Flags().StringVar(&contextMemoryQuery, "query", "", "Goal/query used to calculate base relevance (required)")
	contextLearningDoctorCmd.Flags().BoolVar(&contextLearningCheck, "learning", false, "Check outcome-driven memory learning state")
}

func runContextOutcomes(cmd *cobra.Command, args []string) error {
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	aggregate, err := repo.ExperienceAggregate(cmd.Context(), args[0], contextPolicyVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no outcomes for context item %q under policy %q", args[0], contextPolicyVersion)
	}
	if err != nil {
		return err
	}
	if contextQueryJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(aggregate)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context_item_id: %s\npolicy_version: %s\npositive_weight: %.6f\nnegative_weight: %.6f\nutility_lower_bound: %.6f\nexposures: %d\napplied: %d\nconsulted: %d\nrejected: %d\nverified_support: %d\ncausal_failures: %d\nindependent_tasks: %d\nindependent_projects: %d\nrevision: %d\n", aggregate.ContextItemID, aggregate.PolicyVersion, aggregate.PositiveWeight, aggregate.NegativeWeight, aggregate.UtilityLowerBound, aggregate.ExposureCount, aggregate.AppliedCount, aggregate.ConsultedCount, aggregate.RejectedCount, aggregate.VerifiedSupportCount, aggregate.CausalFailureCount, aggregate.IndependentTaskCount, aggregate.IndependentProjectCount, aggregate.Revision)
	return err
}

func runContextExplainMemory(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(contextProject) == "" || strings.TrimSpace(contextMemoryQuery) == "" {
		return fmt.Errorf("--project and --query are required")
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	item, err := repo.Get(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	// Resolve the policy to its own immutable snapshot and runtime parameters so
	// the explained final score, the aggregate, and the retrieval ID all come
	// from the same policy. An explicitly passed --policy-version is resolved to
	// that version's snapshot (rejected if it is not recorded); otherwise the
	// active policy is authoritative (spec §7 HF-MEM4-005).
	policyVersion := ""
	if cmd.Flags().Changed("policy-version") {
		policyVersion = contextPolicyVersion
	}
	policy, runtimePolicy, err := team.LoadMemoryPolicy(cmd.Context(), repo, policyVersion)
	if err != nil {
		return err
	}
	results, _, err := contextstore.HybridRetrieve(cmd.Context(), repo, nil, contextstore.SearchRequest{Query: contextMemoryQuery, Scope: contextstore.Scope{ProjectID: contextProject, TeamID: contextTeam}, Limit: 100})
	if err != nil {
		return err
	}
	base := 0.0
	for _, result := range results {
		if result.Item.ID == item.ID {
			base = result.Score
			break
		}
	}
	aggregate, aggregateErr := repo.ExperienceAggregate(cmd.Context(), item.ID, policy.PolicyVersion)
	if aggregateErr != nil && !errors.Is(aggregateErr, sql.ErrNoRows) {
		return aggregateErr
	}
	explanation := team.ExplainMemoryScoreWithPolicy(item, base, aggregate, policy, runtimePolicy)
	// The retrieval ID is the durable manifest's attempt-scoped binding for
	// this item under the exact policy version and query being explained. When
	// no manifest matches, the explanation is counterfactual and carries no
	// execution retrieval ID (spec §5.1, §7 HF-MEM4-005).
	explanation.RetrievalID = team.RetrievalIDForItem(getContextWorkspace(), policy.PolicyVersion, team.QueryHash(contextMemoryQuery), item.ID)
	if contextQueryJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(explanation)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context_item_id: %s\npolicy_version: %s\nretrieval_id: %s\nbase_relevance: %.6f\napplicability: %.6f\nutility_lower_bound: %.6f\nfreshness: %.6f\ntrust_factor: %.6f\nharm_penalty: %.6f\nstale_environment_penalty: %.6f\nfinal_score: %.6f\nexposures: %d\napplied: %d\nverified_support: %d\ncausal_failures: %d\n", explanation.ContextItemID, explanation.PolicyVersion, explanation.RetrievalID, explanation.ScoreParts.BaseRelevance, explanation.ScoreParts.Applicability, explanation.ScoreParts.UtilityLowerBound, explanation.ScoreParts.Freshness, explanation.ScoreParts.TrustFactor, explanation.ScoreParts.HarmfulUsePenalty, explanation.ScoreParts.StaleEnvironmentPenalty, explanation.FinalScore, explanation.ExposureCount, explanation.AppliedCount, explanation.VerifiedSupportCount, explanation.CausalFailureCount)
	return err
}

func runContextLearningDoctor(cmd *cobra.Command, _ []string) error {
	if !contextLearningCheck {
		return fmt.Errorf("--learning is required")
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(getContextWorkspace(), "context.sqlite"))
	if err != nil {
		return fmt.Errorf("learning database: %w", err)
	}
	defer func() { _ = repo.Close() }()
	store, err := team.OpenEventStore(getContextWorkspace())
	if err != nil {
		return fmt.Errorf("learning event store: %w", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.VerifyHashChain(); err != nil {
		return fmt.Errorf("learning event hash chain: %w", err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		return err
	}
	policy := agent.DefaultMemoryLearningPolicy()
	policy.PolicyVersion = contextPolicyVersion
	observations := team.ExperienceObservationsFromEvents(events, policy)
	processed, err := repo.ExperienceProcessedCount(cmd.Context())
	if err != nil {
		return err
	}
	uniqueObservationKeys := make(map[string]struct{}, len(observations))
	for _, observation := range observations {
		uniqueObservationKeys[observation.IdempotencyKey] = struct{}{}
	}
	if processed < len(uniqueObservationKeys) {
		return fmt.Errorf("learning projection is degraded: processed_events=%d durable_memory_events=%d; run context rebuild --aggregates", processed, len(uniqueObservationKeys))
	}
	aggregates, err := repo.ListExperienceAggregates(cmd.Context(), contextPolicyVersion)
	if err != nil {
		return err
	}
	result := map[string]any{"status": "ok", "policy_version": contextPolicyVersion, "memory_events": len(observations), "processed_events": processed, "aggregates": len(aggregates), "hash_chain": "valid"}
	if contextQueryJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "context doctor --learning: ok; policy=%s memory_events=%d processed_events=%d aggregates=%d hash_chain=valid\n", contextPolicyVersion, len(observations), processed, len(aggregates))
	return err
}
