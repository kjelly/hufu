package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

var (
	rankingReplayQueries     []string
	rankingReplayQueriesFile string
	rankingReplayFromSession bool
	rankingReplayFusion      string
	rankingReplayWeight      float64
)

var contextRankingReplayCmd = &cobra.Command{
	Use:   "ranking-replay",
	Short: "Compare shared-persistent memory selection under the current and a candidate fusion (read-only, content-free)",
	Args:  cobra.NoArgs,
	RunE:  runContextRankingReplay,
}

func init() {
	flags := contextRankingReplayCmd.Flags()
	flags.StringVarP(&contextWorkspace, "workspace", "w", "", "Workspace containing context.sqlite")
	flags.StringVar(&contextProject, "project", "", "Canonical project ID (required)")
	flags.StringVar(&contextTeam, "team", "", "Canonical team ID (required)")
	flags.StringArrayVar(&rankingReplayQueries, "query", nil, "Query to replay (repeatable)")
	flags.StringVar(&rankingReplayQueriesFile, "queries-file", "", "File with one query per line; blank lines and lines starting with # are ignored")
	flags.BoolVar(&rankingReplayFromSession, "from-session", false, "Rebuild dispatch queries from the goals of the tasks in the workspace session")
	flags.StringVar(&rankingReplayFusion, "candidate-fusion", string(contextstore.FusionRRFNormalized), "Candidate fusion: legacy, rrf_normalized, or score_normalized")
	flags.Float64Var(&rankingReplayWeight, "candidate-carried-weight", 0, "Carried-score weight in (0,1] for score_normalized (default 0.8)")
	flags.StringVar(&contextPolicyVersion, "policy-version", "memory-policy-v1", "Memory policy version (default: the active policy)")
	flags.BoolVar(&contextQueryJSON, "json", false, "Emit JSON")
	contextCmd.AddCommand(contextRankingReplayCmd)
}

// runContextRankingReplay opens the store read-only (mode=ro, query_only),
// never writes traces, and prints IDs, query hashes, counts, and scores only.
func runContextRankingReplay(cmd *cobra.Command, _ []string) error {
	if strings.TrimSpace(contextProject) == "" || strings.TrimSpace(contextTeam) == "" {
		return fmt.Errorf("--project and --team are required")
	}
	workspace := getContextWorkspace()
	queries, err := rankingReplayQuerySet(workspace)
	if err != nil {
		return err
	}
	if len(queries) == 0 {
		return fmt.Errorf("no queries: pass --query, --queries-file, or --from-session")
	}
	readOnly, err := contextstore.OpenSQLiteReadOnly(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		return err
	}
	defer func() { _ = readOnly.Close() }()
	repo, ok := readOnly.(*contextstore.SQLiteRepository)
	if !ok {
		return fmt.Errorf("ranking replay requires the SQLite context store")
	}
	policyVersion := ""
	if cmd.Flags().Changed("policy-version") {
		policyVersion = contextPolicyVersion
	}
	report, err := team.ReplayPersistentRanking(cmd.Context(), repo, team.RankingReplayInput{
		ProjectID: contextProject, TeamID: contextTeam, PolicyVersion: policyVersion, Queries: queries,
		CandidateFusion: contextstore.FusionMode(rankingReplayFusion), CandidateCarriedWeight: rankingReplayWeight, Now: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if contextQueryJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
	}
	return writeRankingReplay(cmd.OutOrStdout(), report)
}

func rankingReplayQuerySet(workspace string) ([]team.RankingReplayQuery, error) {
	var queries []team.RankingReplayQuery
	for _, query := range rankingReplayQueries {
		queries = append(queries, team.RankingReplayQuery{Text: query, Source: "flag"})
	}
	if path := strings.TrimSpace(rankingReplayQueriesFile); path != "" {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open queries file: %w", err)
		}
		defer func() { _ = file.Close() }()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" && !strings.HasPrefix(line, "#") {
				queries = append(queries, team.RankingReplayQuery{Text: line, Source: "file"})
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read queries file: %w", err)
		}
	}
	if rankingReplayFromSession {
		queries = append(queries, team.SessionReplayQueries(workspace)...)
	}
	return queries, nil
}

func writeRankingReplay(out io.Writer, report team.RankingReplayReport) error {
	metrics := "unavailable (no experience aggregates)"
	if report.OutcomeMetricsAvailable {
		metrics = "available"
	}
	candidate := string(report.CandidateFusion)
	if report.CandidateCarriedWeight > 0 {
		candidate += fmt.Sprintf("(carried_weight=%.2f)", report.CandidateCarriedWeight)
	}
	if _, err := fmt.Fprintf(out, "context ranking-replay: scope=%s/%s policy=%s (%s) baseline=%s candidate=%s\neligible=%d with_aggregates=%d outcome_metrics=%s queries=%d\n",
		report.Scope.ProjectID, report.Scope.TeamID, report.PolicyVersion, report.ModeSource, report.BaselineFusion, candidate,
		report.EligibleItems, report.ItemsWithAggregates, metrics, report.QueryCount); err != nil {
		return err
	}
	for _, ranker := range []struct {
		name    string
		summary team.RankingReplayRankerSummary
	}{{"relevance", report.Relevance}, {"reinforced", report.Reinforced}} {
		line := fmt.Sprintf("%s: set_changed=%d/%d order_changed=%d/%d mean_jaccard=%.3f", ranker.name, ranker.summary.SetChangedQueries, report.QueryCount, ranker.summary.OrderChangedQueries, report.QueryCount, ranker.summary.MeanJaccard)
		if base, cand := ranker.summary.BaselineOutcome, ranker.summary.CandidateOutcome; base != nil && cand != nil {
			line += fmt.Sprintf(" verified=%d->%d causal_failure=%d->%d negative_weight=%d->%d mean_utility=%.3f->%.3f",
				base.WithVerifiedSupport, cand.WithVerifiedSupport, base.WithCausalFailure, cand.WithCausalFailure, base.WithNegativeWeight, cand.WithNegativeWeight, base.MeanUtility, cand.MeanUtility)
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	for _, query := range report.Queries {
		if !query.Relevance.OrderChanged && !query.Reinforced.OrderChanged {
			continue
		}
		if _, err := fmt.Fprintf(out, "query=%s source=%s relevance: jaccard=%.3f added=%s removed=%s reinforced: jaccard=%.3f added=%s removed=%s\n",
			query.QueryHash, query.Source, query.Relevance.Jaccard, strings.Join(query.Relevance.Added, ","), strings.Join(query.Relevance.Removed, ","),
			query.Reinforced.Jaccard, strings.Join(query.Reinforced.Added, ","), strings.Join(query.Reinforced.Removed, ",")); err != nil {
			return err
		}
	}
	return nil
}
