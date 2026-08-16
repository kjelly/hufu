package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/promotion"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

var promotionWorkspace, promotionProject, promotionTeam, promotionSearchPath, promotionPolicyVersion, promotionType, promotionAgent, promotionModel, promotionDraftFile, promotionRejectReason string
var promotionJSON, ltmPromotionDryRun, promotionShowContent bool

var contextPromotionCmd = &cobra.Command{Use: "promotion", Short: "Promote proven long-term context through an explicit review workflow"}
var contextPromotionAnalyzeCmd = &cobra.Command{Use: "analyze", Short: "Analyze eligible LTM and create reviewable proposals", Args: cobra.NoArgs, RunE: runPromotionAnalyze}
var contextPromotionListCmd = &cobra.Command{Use: "list", Short: "List promotion proposals without draft content", Args: cobra.NoArgs, RunE: runPromotionList}
var contextPromotionShowCmd = &cobra.Command{Use: "show <proposal-id>", Short: "Show a promotion proposal", Args: cobra.ExactArgs(1), RunE: runPromotionShow}
var contextPromotionEditCmd = &cobra.Command{Use: "edit <proposal-id>", Short: "Replace a proposed draft from a file", Args: cobra.ExactArgs(1), RunE: runPromotionEdit}
var contextPromotionApproveCmd = &cobra.Command{Use: "approve <proposal-id>", Short: "Explicitly approve a proposed promotion", Args: cobra.ExactArgs(1), RunE: runPromotionApprove}
var contextPromotionRejectCmd = &cobra.Command{Use: "reject <proposal-id>", Short: "Reject a proposed promotion", Args: cobra.ExactArgs(1), RunE: runPromotionReject}
var contextPromotionApplyCmd = &cobra.Command{Use: "apply <proposal-id>", Short: "Apply an approved promotion after stale-data preflight", Args: cobra.ExactArgs(1), RunE: runPromotionApply}

func init() {
	contextCmd.AddCommand(contextPromotionCmd)
	contextPromotionCmd.PersistentFlags().StringVarP(&promotionWorkspace, "workspace", "w", "", "Workspace containing context.sqlite and logs/event_store.jsonl")
	contextPromotionCmd.PersistentFlags().StringVar(&promotionProject, "project", "", "Canonical context project scope (required)")
	contextPromotionCmd.PersistentFlags().StringVar(&promotionTeam, "team", "", "Canonical context and target team (required)")
	contextPromotionCmd.PersistentFlags().StringVar(&promotionSearchPath, "team-search-path", "", "Comma-separated team search paths")
	contextPromotionCmd.PersistentFlags().StringVar(&promotionPolicyVersion, "policy-version", "memory-policy-v1", "Experience policy version")
	contextPromotionCmd.PersistentFlags().BoolVar(&promotionJSON, "json", false, "Emit stable JSON")
	contextPromotionAnalyzeCmd.Flags().StringVar(&promotionType, "type", "", "Promotion type: skill, team-policy, or agent-policy")
	contextPromotionAnalyzeCmd.Flags().StringVar(&promotionAgent, "agent", "", "Agent name for agent-policy analysis")
	contextPromotionAnalyzeCmd.Flags().StringVar(&promotionModel, "model", "", "Model used to classify and draft proposals")
	contextPromotionAnalyzeCmd.Flags().BoolVar(&ltmPromotionDryRun, "dry-run", false, "List eligible source metadata without calling a model or creating proposals")
	contextPromotionShowCmd.Flags().BoolVar(&promotionShowContent, "show-content", false, "Show the redacted draft and source summaries")
	contextPromotionEditCmd.Flags().StringVar(&promotionDraftFile, "draft-file", "", "File containing the complete replacement draft (required)")
	contextPromotionRejectCmd.Flags().StringVar(&promotionRejectReason, "reason", "", "Operator rejection reason (required)")
	contextPromotionCmd.AddCommand(contextPromotionAnalyzeCmd, contextPromotionListCmd, contextPromotionShowCmd, contextPromotionEditCmd, contextPromotionApproveCmd, contextPromotionRejectCmd, contextPromotionApplyCmd)
}

func promotionScopeValid() error {
	if strings.TrimSpace(promotionProject) == "" {
		return fmt.Errorf("--project is required")
	}
	if strings.TrimSpace(promotionTeam) == "" {
		return fmt.Errorf("--team is required")
	}
	return nil
}
func promotionWorkspacePath() string {
	if promotionWorkspace != "" {
		return promotionWorkspace
	}
	return getContextWorkspace()
}
func promotionRegistry() *team.TeamRegistry {
	paths := team.DefaultSearchPaths()
	if promotionSearchPath != "" {
		paths = strings.Split(promotionSearchPath, ",")
	}
	return team.NewTeamRegistry(paths)
}
func openPromotion(cmd *cobra.Command) (*contextstore.SQLiteRepository, promotion.Service, error) {
	if err := promotionScopeValid(); err != nil {
		return nil, promotion.Service{}, err
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(promotionWorkspacePath(), "context.sqlite"))
	if err != nil {
		return nil, promotion.Service{}, err
	}
	if err = flushPromotionEvents(cmd.Context(), repo); err != nil {
		_ = repo.Close()
		return nil, promotion.Service{}, err
	}
	return repo, promotion.Service{Repo: repo}, nil
}
func finishPromotion(ctx context.Context, repo *contextstore.SQLiteRepository) error {
	return flushPromotionEvents(ctx, repo)
}

func runPromotionAnalyze(cmd *cobra.Command, _ []string) error {
	repo, _, err := openPromotion(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	registry := promotionRegistry()
	if err = registry.Discover(); err != nil {
		return err
	}
	dir, err := registry.Resolve(promotionTeam)
	if err != nil {
		return err
	}
	typ, err := parsePromotionType(promotionType)
	if err != nil {
		return err
	}
	var generator promotion.DraftGenerator
	if !ltmPromotionDryRun {
		g, e := newPromotionGenerator(cmd.Context(), dir)
		if e != nil {
			return e
		}
		generator = g
	}
	analyzer := promotion.Analyzer{Repo: repo, Generator: generator, Policy: agent.DefaultMemoryLearningPolicy()}
	result, err := analyzer.Analyze(cmd.Context(), promotion.AnalyzeOptions{ProjectID: promotionProject, TeamID: promotionTeam, PolicyVersion: promotionPolicyVersion, AgentID: promotionAgent, TeamDir: dir, Type: typ, DryRun: ltmPromotionDryRun})
	if err != nil {
		return err
	}
	if err = finishPromotion(cmd.Context(), repo); err != nil {
		return err
	}
	if len(result.Eligible) == 0 {
		if promotionJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "eligible": []any{}, "diagnostics": result.Diagnostics, "proposals": []any{}, "message": "No suitable LTM entries found for promotion."})
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "No suitable LTM entries found for promotion.")
		return err
	}
	views := make([]promotionProposalView, 0, len(result.Proposals))
	for _, p := range result.Proposals {
		views = append(views, newPromotionView(p, false))
	}
	if promotionJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "eligible": eligibleViews(result.Eligible), "diagnostics": result.Diagnostics, "proposals": views, "dry_run": ltmPromotionDryRun})
	}
	if ltmPromotionDryRun {
		for _, e := range result.Eligible {
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tverified=%d\tindependent_tasks=%d\n", e.Item.ID, e.Item.Kind, e.Aggregate.VerifiedSupportCount, e.Aggregate.IndependentTaskCount); err != nil {
				return err
			}
		}
		return nil
	}
	for _, p := range views {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", p.ID, p.Type, p.Status, p.TargetPath); err != nil {
			return err
		}
	}
	return nil
}
func runPromotionList(cmd *cobra.Command, _ []string) error {
	repo, svc, err := openPromotion(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	items, err := svc.List(cmd.Context(), promotionProject, promotionTeam)
	if err != nil {
		return err
	}
	views := make([]promotionProposalView, 0, len(items))
	for _, p := range items {
		views = append(views, newPromotionView(p, false))
	}
	if promotionJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "proposals": views})
	}
	for _, p := range views {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", p.ID, p.Type, p.Status, p.TargetPath); err != nil {
			return err
		}
	}
	return nil
}
func runPromotionShow(cmd *cobra.Command, args []string) error {
	repo, svc, err := openPromotion(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	p, err := svc.Get(cmd.Context(), args[0], promotionProject, promotionTeam)
	if err != nil {
		return err
	}
	view := newPromotionView(p, promotionShowContent)
	if promotionShowContent {
		for _, s := range p.Sources {
			item, e := repo.Get(cmd.Context(), s.ContextItemID)
			if e == nil {
				summary := utils.RedactSecrets(item.Content)
				if len([]rune(summary)) > 240 {
					summary = string([]rune(summary)[:240]) + "…"
				}
				view.SourceSummaries = append(view.SourceSummaries, sourceSummary{ID: item.ID, Kind: string(item.Kind), Summary: summary})
			}
		}
	}
	if promotionJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "proposal": view})
	}
	if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\nstatus: %s\ntype: %s\ntarget: %s\n", view.ID, view.Status, view.Type, view.TargetPath); err != nil {
		return err
	}
	if promotionShowContent {
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "draft:\n%s\n", view.Draft); err != nil {
			return err
		}
		for _, s := range view.SourceSummaries {
			if _, err = fmt.Fprintf(cmd.OutOrStdout(), "source %s (%s): %s\n", s.ID, s.Kind, s.Summary); err != nil {
				return err
			}
		}
	}
	return nil
}
func runPromotionEdit(cmd *cobra.Command, args []string) error {
	if promotionDraftFile == "" {
		return fmt.Errorf("--draft-file is required")
	}
	return runPromotionMutation(cmd, args[0], func(ctx context.Context, s promotion.Service) (promotion.Proposal, error) {
		return s.Edit(ctx, args[0], promotionProject, promotionTeam, promotionDraftFile)
	})
}
func runPromotionApprove(cmd *cobra.Command, args []string) error {
	return runPromotionMutation(cmd, args[0], func(ctx context.Context, s promotion.Service) (promotion.Proposal, error) {
		return s.Approve(ctx, args[0], promotionProject, promotionTeam)
	})
}
func runPromotionReject(cmd *cobra.Command, args []string) error {
	if promotionRejectReason == "" {
		return fmt.Errorf("--reason is required")
	}
	return runPromotionMutation(cmd, args[0], func(ctx context.Context, s promotion.Service) (promotion.Proposal, error) {
		return s.Reject(ctx, args[0], promotionProject, promotionTeam, promotionRejectReason)
	})
}
func runPromotionMutation(cmd *cobra.Command, id string, fn func(context.Context, promotion.Service) (promotion.Proposal, error)) error {
	repo, svc, err := openPromotion(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	p, err := fn(cmd.Context(), svc)
	if err != nil {
		return err
	}
	if err = finishPromotion(cmd.Context(), repo); err != nil {
		return err
	}
	view := newPromotionView(p, false)
	if promotionJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "proposal": view})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", id, p.Status)
	return err
}
func runPromotionApply(cmd *cobra.Command, args []string) error {
	repo, svc, err := openPromotion(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = repo.Close() }()
	result, err := svc.Apply(cmd.Context(), args[0], promotionProject, promotionTeam, promotionRegistry())
	if flushErr := finishPromotion(cmd.Context(), repo); flushErr != nil && err == nil {
		err = flushErr
	}
	if err != nil {
		return err
	}
	view := newPromotionView(result.Proposal, false)
	if promotionJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "proposal": view, "already_applied": result.AlreadyApplied})
	}
	if result.AlreadyApplied {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\talready applied\n", args[0])
	} else {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\tapplied\n", args[0])
	}
	return err
}

func flushPromotionEvents(ctx context.Context, repo *contextstore.SQLiteRepository) error {
	events, err := repo.PendingPromotionEvents(ctx)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	store, err := team.NewEventStore(promotionWorkspacePath(), "promotion", "")
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	for _, event := range events {
		if err = store.Append(team.RunEvent{Type: event.EventType, Actor: "operator", IdempotencyKey: event.IdempotencyKey, Payload: event.Payload}); err != nil {
			return err
		}
		if err = repo.MarkPromotionEventDelivered(ctx, event.IdempotencyKey); err != nil {
			return err
		}
	}
	return nil
}

type sidecarTextGenerator struct{ s *sidecar.Sidecar }

func (g sidecarTextGenerator) GenerateText(ctx context.Context, prompt string) (string, error) {
	return g.s.ExecuteProfile(sidecar.WithPurpose(ctx, "promotion_draft"), prompt, sidecar.CompactorProfile)
}
func newPromotionGenerator(ctx context.Context, teamDir string) (promotion.DraftGenerator, error) {
	session, err := team.LoadTeam(teamDir, nil, nil, team.DefaultProviderRegistry)
	if err != nil {
		return nil, err
	}
	cfg := config.LoadConfig()
	model := firstNonEmpty(promotionModel, session.Config.SidecarModel, session.Config.Generation.Model, cfg.SidecarModel, cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("promotion analyze requires --model or a team/config sidecar/model")
	}
	url := config.ResolveProviderURL(opts.providerURL, session.Config.ProviderURL, "")
	key := config.ResolveProviderAPIKey(opts.providerAPIKey, session.Config.ProviderAPIKey)
	// Promotion analysis is a CLI model invocation, but it still needs the
	// same repository, compiler, redaction, manifest, and event boundary as a
	// coordinator sidecar. Bind this loaded team to the promotion workspace
	// before constructing the coordinator so the draft lineage is replayable
	// next to context.sqlite rather than in an ambient project workspace.
	session.Workspace = promotionWorkspacePath()
	coordinator, err := team.NewCoordinator(session, url, key, nil, nil, nil, team.RoleModels{Sidecar: model}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		return nil, err
	}
	if err := coordinator.PrepareContextPreflight(); err != nil {
		coordinator.CloseContextPreflight()
		return nil, err
	}
	sc := coordinator.Sidecar()
	if sc == nil {
		return nil, fmt.Errorf("promotion draft sidecar is unavailable after context preflight")
	}
	return promotion.JSONDraftGenerator{Generator: sidecarTextGenerator{s: sc}}, nil
}

func parsePromotionType(v string) (promotion.Type, error) {
	switch v {
	case "":
		return "", nil
	case "skill":
		return promotion.TypeSkill, nil
	case "team-policy", "team_policy":
		return promotion.TypeTeamPolicy, nil
	case "agent-policy", "agent_policy":
		return promotion.TypeAgentPolicy, nil
	default:
		return "", fmt.Errorf("--type must be skill, team-policy, or agent-policy")
	}
}

type eligibleView struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	AgentID      string            `json:"agent_id,omitempty"`
	AllowedTypes []promotion.Type  `json:"allowed_types"`
	Metrics      promotion.Metrics `json:"metrics"`
}

func eligibleViews(items []promotion.EligibleSource) []eligibleView {
	out := make([]eligibleView, 0, len(items))
	for _, e := range items {
		out = append(out, eligibleView{ID: e.Item.ID, Kind: string(e.Item.Kind), AgentID: e.Item.Scope.AgentID, AllowedTypes: e.AllowedTypes, Metrics: promotion.Metrics{UtilityLowerBound: e.Aggregate.UtilityLowerBound, AppliedCount: e.Aggregate.AppliedCount, RejectedCount: e.Aggregate.RejectedCount, VerifiedSupportCount: e.Aggregate.VerifiedSupportCount, CausalFailureCount: e.Aggregate.CausalFailureCount, IndependentTaskCount: e.Aggregate.IndependentTaskCount, IndependentProjectCount: e.Aggregate.IndependentProjectCount, AggregateRevision: e.Aggregate.Revision}})
	}
	return out
}

type sourceSummary struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}
type promotionProposalView struct {
	ID              string                     `json:"id"`
	ProjectID       string                     `json:"project_id"`
	TeamID          string                     `json:"team_id"`
	Type            promotion.Type             `json:"type"`
	AgentID         string                     `json:"agent_id,omitempty"`
	TargetPath      string                     `json:"target_path"`
	TargetBaseHash  string                     `json:"target_base_hash"`
	DraftHash       string                     `json:"draft_hash"`
	PolicyVersion   string                     `json:"policy_version"`
	Sources         []promotion.SourceSnapshot `json:"sources"`
	Metrics         promotion.Metrics          `json:"metrics"`
	Status          promotion.Status           `json:"status"`
	RejectionReason string                     `json:"rejection_reason,omitempty"`
	Draft           string                     `json:"draft,omitempty"`
	SourceSummaries []sourceSummary            `json:"source_summaries,omitempty"`
}

func newPromotionView(p promotion.Proposal, content bool) promotionProposalView {
	v := promotionProposalView{ID: p.ID, ProjectID: p.ProjectID, TeamID: p.TeamID, Type: p.Type, AgentID: p.AgentID, TargetPath: p.TargetPath, TargetBaseHash: p.TargetBaseHash, DraftHash: p.DraftHash, PolicyVersion: p.PolicyVersion, Sources: p.Sources, Metrics: p.Metrics, Status: p.Status, RejectionReason: p.RejectionReason}
	if content {
		v.Draft = utils.RedactSecrets(p.Draft)
	}
	return v
}
