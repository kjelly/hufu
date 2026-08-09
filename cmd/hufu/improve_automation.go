package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/improve"
)

var (
	promotionRepository string
	promotionBaseBranch string
	promotionTitle      string
	promotionDryRun     bool

	adoptionPullRequestURL string
	adoptionChangeSummary  string
	adoptionConditions     []string

	monitorRuns             int
	monitorAcceptancePassed bool
	monitorSafetyViolations int

	knowledgeIssueType string
	knowledgeFormat    string
)

var improveExperimentPRCmd = &cobra.Command{
	Use:   "pr <experiment-id>",
	Short: "Create a review-only pull request for a passed experiment",
	Long: `Create a dedicated Git branch, apply the candidate patch, commit it, and use
the GitHub CLI to create a pull request. The repository must be clean. This
command never runs a merge command and never changes a team automatically.`,
	Args: cobra.ExactArgs(1),
	RunE: runImproveExperimentPR,
}

var improveExperimentAdoptCmd = &cobra.Command{
	Use:   "adopt <adoption-id> <experiment-id>",
	Short: "Record a human-confirmed adoption and add improvement knowledge",
	Long: `Record that a human-reviewed experiment has been deployed. Adoption creates
a durable knowledge entry and establishes the immutable baseline snapshot that
production monitoring uses for rollback suggestions. It does not merge a PR.`,
	Args: cobra.ExactArgs(2),
	RunE: runImproveExperimentAdopt,
}

var improveMonitorCmd = &cobra.Command{
	Use:   "monitor <adoption-id>",
	Short: "Compare production telemetry with an adopted experiment baseline",
	Long: `Analyze recent production telemetry for an adopted team. Safety, acceptance,
completion, and error regressions write a rollback suggestion that references
the immutable baseline snapshot; no rollback command is executed.`,
	Args: cobra.ExactArgs(1),
	RunE: runImproveMonitor,
}

var improveKnowledgeCmd = &cobra.Command{
	Use:   "knowledge",
	Short: "Inspect adopted agent-team improvement knowledge",
}

var improveKnowledgeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List effective changes and their applicable conditions",
	Args:  cobra.NoArgs,
	RunE:  runImproveKnowledgeList,
}

func init() {
	improveExperimentCmd.AddCommand(improveExperimentPRCmd, improveExperimentAdoptCmd)
	improveCmd.AddCommand(improveMonitorCmd, improveKnowledgeCmd)
	improveKnowledgeCmd.AddCommand(improveKnowledgeListCmd)

	improveExperimentPRCmd.Flags().StringVar(&promotionRepository, "repo", ".", "Repository in which to create the pull request")
	improveExperimentPRCmd.Flags().StringVar(&promotionBaseBranch, "base", "main", "Base branch for the pull request")
	improveExperimentPRCmd.Flags().StringVar(&promotionTitle, "title", "", "Optional pull request title")
	improveExperimentPRCmd.Flags().BoolVar(&promotionDryRun, "dry-run", false, "Print the pull request plan without running Git or GitHub commands")

	improveExperimentAdoptCmd.Flags().StringVar(&adoptionPullRequestURL, "pr-url", "", "Merged or approved pull request URL (required)")
	improveExperimentAdoptCmd.Flags().StringVar(&adoptionChangeSummary, "change-summary", "", "Concise description of the effective change (required)")
	improveExperimentAdoptCmd.Flags().StringArrayVar(&adoptionConditions, "condition", nil, "Condition under which the change is effective (repeatable)")
	_ = improveExperimentAdoptCmd.MarkFlagRequired("pr-url")
	_ = improveExperimentAdoptCmd.MarkFlagRequired("change-summary")

	improveMonitorCmd.Flags().IntVar(&monitorRuns, "runs", 10, "Number of recent production runs to analyze")
	improveMonitorCmd.Flags().BoolVar(&monitorAcceptancePassed, "acceptance-passed", false, "Record that the production acceptance gate passed (must be stated explicitly)")
	improveMonitorCmd.Flags().IntVar(&monitorSafetyViolations, "safety-violations", 0, "Observed production safety violations")

	improveKnowledgeListCmd.Flags().StringVar(&knowledgeIssueType, "issue-type", "", "Filter by benchmark category / issue type")
	improveKnowledgeListCmd.Flags().StringVar(&knowledgeFormat, "format", "markdown", "Output format: markdown or json")
}

func runImproveExperimentPR(_ *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	plan, err := improve.PreparePullRequest(workspace, args[0], promotionRepository, promotionBaseBranch, promotionTitle)
	if err != nil {
		return err
	}
	if promotionDryRun {
		fmt.Printf("experiment: %s\nrepository: %s\nbranch: %s\nbase: %s\ntitle: %s\npatch: %s\n", plan.ExperimentID, plan.Repository, plan.Branch, plan.BaseBranch, plan.Title, plan.PatchPath)
		return nil
	}
	record, err := improve.CreatePullRequest(plan)
	if err != nil {
		return err
	}
	path, err := improve.WritePromotionRecord(workspace, record)
	if err != nil {
		return err
	}
	fmt.Printf("%s\npull request: %s\n", path, record.PullRequestURL)
	return nil
}

func runImproveExperimentAdopt(_ *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	adoption, path, err := improve.CreateAdoption(workspace, args[0], args[1], adoptionPullRequestURL, adoptionChangeSummary, adoptionConditions)
	if err != nil {
		return err
	}
	fmt.Printf("%s\nteam: %s\nrollback revision: %s\n", path, adoption.Team, adoption.RollbackRevision)
	return nil
}

func runImproveMonitor(cmd *cobra.Command, args []string) error {
	if monitorRuns < 1 {
		return fmt.Errorf("runs must be at least 1")
	}
	if !cmd.Flags().Changed("acceptance-passed") {
		return fmt.Errorf("--acceptance-passed must be stated explicitly")
	}
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	adoption, err := improve.LoadAdoption(workspace, args[0])
	if err != nil {
		return err
	}
	if strings.TrimSpace(improveTeam) != "" && !strings.EqualFold(improveTeam, adoption.Team) {
		return fmt.Errorf("--team %q does not match adoption team %q", improveTeam, adoption.Team)
	}
	teamDir, err := resolveImproveTeamDir(adoption.Team)
	if err != nil {
		return err
	}
	production, err := improve.AnalyzeRecent(workspace, adoption.Team, teamDir, monitorRuns)
	if err != nil {
		return err
	}
	report, err := improve.EvaluateMonitoring(adoption, production, monitorAcceptancePassed, monitorSafetyViolations)
	if err != nil {
		return err
	}
	path, err := improve.WriteMonitoringReport(workspace, report)
	if err != nil {
		return err
	}
	fmt.Printf("%s\nstatus: %s\n", path, report.Status)
	if report.RollbackSuggestion != nil {
		fmt.Printf("rollback suggestion: baseline snapshot %s (%s)\n", report.RollbackSuggestion.BaselineSnapshotID, report.RollbackSuggestion.RollbackRevision)
	}
	return nil
}

func runImproveKnowledgeList(_ *cobra.Command, _ []string) error {
	format := strings.ToLower(strings.TrimSpace(knowledgeFormat))
	if format != "markdown" && format != "json" {
		return fmt.Errorf("unsupported format %q (want markdown or json)", knowledgeFormat)
	}
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	entries, err := improve.ListKnowledge(workspace, knowledgeIssueType)
	if err != nil {
		return err
	}
	if format == "json" {
		data, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Print(improve.KnowledgeMarkdown(entries))
	return nil
}
