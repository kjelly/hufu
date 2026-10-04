package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
)

// runContextExplainMemory recomputes the runtime shared-persistent ranking for
// the requested scope, query, and policy and explains one item. It reports a
// historical retrieval ID only when a durable manifest matches; the rest is a
// recomputation from current data, never a record of a past run.
func runContextExplainMemory(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(contextProject) == "" || strings.TrimSpace(contextTeam) == "" || strings.TrimSpace(contextMemoryQuery) == "" {
		return fmt.Errorf("--project, --team, and --query are required")
	}
	repo, err := openExistingContextRepository(getContextWorkspace())
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	policyVersion := ""
	if cmd.Flags().Changed("policy-version") {
		policyVersion = contextPolicyVersion
	}
	explanation, err := team.ExplainPersistentMemory(cmd.Context(), repo, team.MemoryExplainInput{
		Workspace: getContextWorkspace(), ItemID: args[0], ProjectID: contextProject, TeamID: contextTeam,
		Query: contextMemoryQuery, PolicyVersion: policyVersion, Now: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if contextQueryJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(explanation)
	}
	return writeMemoryExplanation(cmd.OutOrStdout(), explanation)
}

func writeMemoryExplanation(out io.Writer, e team.MemoryExplanation) error {
	parts := e.ScoreParts
	if _, err := fmt.Fprintf(out, "context_item_id: %s\npolicy_version: %s\nretrieval_id: %s\nbase_relevance: %.6f\napplicability: %.6f\nutility_lower_bound: %.6f\nfreshness: %.6f\ntrust_factor: %.6f\nharm_penalty: %.6f\nstale_environment_penalty: %.6f\nfinal_score: %.6f\nexposures: %d\napplied: %d\nverified_support: %d\ncausal_failures: %d\n",
		e.ContextItemID, e.PolicyVersion, e.RetrievalID, parts.BaseRelevance, parts.Applicability, parts.UtilityLowerBound, parts.Freshness, parts.TrustFactor, parts.HarmfulUsePenalty, parts.StaleEnvironmentPenalty, e.FinalScore, e.ExposureCount, e.AppliedCount, e.VerifiedSupportCount, e.CausalFailureCount); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "last_observed_at: %s\nlast_strong_evidence_at: %s\n", evidenceTime(e.LastObservedAt), evidenceTime(e.LastStrongEvidenceAt)); err != nil {
		return err
	}
	reasons := make([]string, len(e.Reasons))
	for i, reason := range e.Reasons {
		reasons[i] = string(reason)
	}
	if _, err := fmt.Fprintf(out, "recomputed: true\nscope: project=%s team=%s\nmode: %s (%s)\nselected: %t injected: %t\nreasons: %s\nnot_evaluated: %s\nused_goal_fallback: %t\nobserved_candidates: %d\n",
		e.Scope.ProjectID, e.Scope.TeamID, e.Mode, e.ModeSource, e.Ranking.Selected, e.Ranking.Injected, strings.Join(reasons, ","), strings.Join(e.NotEvaluated, ","), e.UsedGoalFallback, e.Retrieval.ObservedCandidateCount); err != nil {
		return err
	}
	for _, path := range e.Retrieval.Paths {
		line := fmt.Sprintf("path %s: executed=%t results=%d", path.Path, path.Executed, path.ResultCount)
		if path.UnavailableReason != "" {
			line += " unavailable_reason=" + string(path.UnavailableReason)
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	if c := e.Retrieval.Candidate; c != nil {
		if _, err := fmt.Fprintf(out, "candidate: exact_rank=%d lexical_rank=%d lexical_score=%.6f vector_rank=%d vector_score=%.6f carried_score=%.6f lexical_rrf=%.6f vector_rrf=%.6f fused_score=%.6f pre_mmr_rank=%d mmr_rank=%d mmr_penalty=%.6f file_path_boost=%.2f retrieval_rank=%d\n",
			c.ExactRank, c.LexicalRank, c.LexicalScore, c.VectorRank, c.VectorScore, c.CarriedScore, c.LexicalRRF, c.VectorRRF, c.FusedScore, c.PreMMRRank, c.MMRRank, c.MMRPenalty, c.FilePathBoost, c.RetrievalRank); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(out, "ranking: base_rank=%d relevance_rank=%d reinforced_rank=%d\n", e.Ranking.BaseRank, e.Ranking.RelevanceRank, e.Ranking.ReinforcedRank)
	return err
}
