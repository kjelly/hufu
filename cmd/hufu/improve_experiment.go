package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/improve"
	"github.com/anomalyco/hufu/internal/team"
)

var (
	benchmarkCategory    string
	benchmarkDescription string
	benchmarkCases       []string

	candidateBaselineID string
	candidateSourceDir  string
	candidatePatchPath  string

	experimentBaselineID                string
	experimentCandidateID               string
	experimentBenchmark                 string
	experimentBaselineReport            string
	experimentCandidateReport           string
	experimentBaselineAccepted          bool
	experimentCandidateAccepted         bool
	experimentBaselineSafetyViolations  int
	experimentCandidateSafetyViolations int
)

var improveBenchmarkCmd = &cobra.Command{
	Use:   "benchmark",
	Short: "Create and inspect versioned benchmark fixtures",
}

var improveBenchmarkCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create an immutable benchmark fixture",
	Long: `Create a benchmark fixture under workspace/improvement/benchmarks.
Each --case uses the format id::type::prompt, where type is happy, failure,
edge, or safety. Fixtures are deliberately authored artifacts; their prompts
are never copied into telemetry or improvement reports.`,
	Example: `  hufu improve benchmark create refactor-smoke --team dev --category refactor \
    --case api-compat::happy::"Refactor the API without breaking callers" \
    --case invalid-input::edge::"Handle an invalid request safely"`,
	Args: cobra.ExactArgs(1),
	RunE: runImproveBenchmarkCreate,
}

var improveExperimentCmd = &cobra.Command{
	Use:   "experiment",
	Short: "Create immutable team snapshots and evaluate A/B experiments",
}

var improveExperimentSnapshotCmd = &cobra.Command{
	Use:   "snapshot <id>",
	Short: "Capture an immutable baseline team snapshot",
	Args:  cobra.ExactArgs(1),
	RunE:  runImproveExperimentSnapshot,
}

var improveExperimentCandidateCmd = &cobra.Command{
	Use:   "candidate <id>",
	Short: "Capture a candidate team and its review patch",
	Long: `Copy a candidate team into the workspace without touching the formal team.
The candidate must use the same team name as its baseline, differ in content,
and include a non-empty patch for human review.`,
	Args: cobra.ExactArgs(1),
	RunE: runImproveExperimentCandidate,
}

var improveExperimentCompareCmd = &cobra.Command{
	Use:   "compare <id>",
	Short: "Evaluate captured baseline and candidate benchmark reports",
	Long: `Evaluate two reports produced from immutable snapshots on the same benchmark.
The command writes JSON and Markdown evidence under workspace/improvement/
experiments. A passing result is eligible for human review only; it never
changes a team, creates a pull request, or merges anything.`,
	Args: cobra.ExactArgs(1),
	RunE: runImproveExperimentCompare,
}

func init() {
	improveCmd.AddCommand(improveBenchmarkCmd, improveExperimentCmd)
	improveBenchmarkCmd.AddCommand(improveBenchmarkCreateCmd)
	improveExperimentCmd.AddCommand(improveExperimentSnapshotCmd, improveExperimentCandidateCmd, improveExperimentCompareCmd)

	improveBenchmarkCreateCmd.Flags().StringVar(&benchmarkCategory, "category", "", "Task category represented by the fixture (required)")
	improveBenchmarkCreateCmd.Flags().StringVar(&benchmarkDescription, "description", "", "Optional benchmark description")
	improveBenchmarkCreateCmd.Flags().StringArrayVar(&benchmarkCases, "case", nil, "Benchmark case: id::type::prompt (repeatable, required)")

	improveExperimentCandidateCmd.Flags().StringVar(&candidateBaselineID, "baseline", "", "Baseline snapshot ID (required)")
	improveExperimentCandidateCmd.Flags().StringVar(&candidateSourceDir, "from", "", "Candidate team directory to snapshot (required)")
	improveExperimentCandidateCmd.Flags().StringVar(&candidatePatchPath, "patch", "", "Review patch representing the candidate change (required)")
	_ = improveExperimentCandidateCmd.MarkFlagRequired("baseline")
	_ = improveExperimentCandidateCmd.MarkFlagRequired("from")
	_ = improveExperimentCandidateCmd.MarkFlagRequired("patch")

	improveExperimentCompareCmd.Flags().StringVar(&experimentBaselineID, "baseline", "", "Baseline snapshot ID (required)")
	improveExperimentCompareCmd.Flags().StringVar(&experimentCandidateID, "candidate", "", "Candidate snapshot ID (required)")
	improveExperimentCompareCmd.Flags().StringVar(&experimentBenchmark, "benchmark", "", "Benchmark name or benchmark.yaml path (required)")
	improveExperimentCompareCmd.Flags().StringVar(&experimentBaselineReport, "baseline-report", "", "Baseline hufu improve JSON report (required)")
	improveExperimentCompareCmd.Flags().StringVar(&experimentCandidateReport, "candidate-report", "", "Candidate hufu improve JSON report (required)")
	improveExperimentCompareCmd.Flags().BoolVar(&experimentBaselineAccepted, "baseline-accepted", false, "Record that the baseline acceptance gate passed (required)")
	improveExperimentCompareCmd.Flags().BoolVar(&experimentCandidateAccepted, "candidate-accepted", false, "Record that the candidate acceptance gate passed (required)")
	improveExperimentCompareCmd.Flags().IntVar(&experimentBaselineSafetyViolations, "baseline-safety-violations", 0, "Observed baseline safety violations")
	improveExperimentCompareCmd.Flags().IntVar(&experimentCandidateSafetyViolations, "candidate-safety-violations", 0, "Observed candidate safety violations")
	for _, name := range []string{"baseline", "candidate", "benchmark", "baseline-report", "candidate-report"} {
		_ = improveExperimentCompareCmd.MarkFlagRequired(name)
	}
}

func runImproveBenchmarkCreate(_ *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	if strings.TrimSpace(improveTeam) == "" {
		return fmt.Errorf("--team is required to create a benchmark")
	}
	cases := make([]improve.BenchmarkCase, 0, len(benchmarkCases))
	for _, raw := range benchmarkCases {
		item, err := parseBenchmarkCase(raw)
		if err != nil {
			return err
		}
		cases = append(cases, item)
	}
	fixture := improve.BenchmarkFixture{
		Version: 1, Name: args[0], Team: strings.TrimSpace(improveTeam), Category: strings.TrimSpace(benchmarkCategory), Description: strings.TrimSpace(benchmarkDescription), Cases: cases,
	}
	path, revision, err := improve.CreateBenchmark(workspace, fixture)
	if err != nil {
		return err
	}
	fmt.Printf("%s\nrevision: %s\n", path, revision)
	return nil
}

func runImproveExperimentSnapshot(_ *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	if strings.TrimSpace(improveTeam) == "" {
		return fmt.Errorf("--team is required to create a baseline snapshot")
	}
	teamDir, err := resolveImproveTeamDir(improveTeam)
	if err != nil {
		return err
	}
	snapshot, dir, err := improve.CreateBaselineSnapshot(workspace, args[0], teamDir)
	if err != nil {
		return err
	}
	fmt.Printf("%s\ndefinition revision: %s\ncontent revision: %s\n", dir, snapshot.DefinitionRevision, snapshot.ContentRevision)
	return nil
}

func runImproveExperimentCandidate(_ *cobra.Command, args []string) error {
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	snapshot, dir, err := improve.CreateCandidateSnapshot(workspace, args[0], candidateBaselineID, candidateSourceDir, candidatePatchPath)
	if err != nil {
		return err
	}
	fmt.Printf("%s\ndefinition revision: %s\ncontent revision: %s\npatch revision: %s\n", dir, snapshot.DefinitionRevision, snapshot.ContentRevision, snapshot.PatchRevision)
	return nil
}

func runImproveExperimentCompare(cmd *cobra.Command, args []string) error {
	if !cmd.Flags().Changed("baseline-accepted") || !cmd.Flags().Changed("candidate-accepted") {
		return fmt.Errorf("--baseline-accepted and --candidate-accepted must be stated explicitly")
	}
	workspace, err := resolveImproveWorkspace(improveWorkspace)
	if err != nil {
		return err
	}
	benchmarkPath, err := resolveBenchmarkPath(workspace, experimentBenchmark)
	if err != nil {
		return err
	}
	fixture, err := improve.LoadBenchmark(benchmarkPath)
	if err != nil {
		return err
	}
	baselineSnapshot, _, err := improve.LoadBaselineSnapshot(workspace, experimentBaselineID)
	if err != nil {
		return err
	}
	candidateSnapshot, _, err := improve.LoadCandidateSnapshot(workspace, experimentCandidateID)
	if err != nil {
		return err
	}
	baselineReport, err := readImproveReport(experimentBaselineReport)
	if err != nil {
		return fmt.Errorf("read baseline report: %w", err)
	}
	candidateReport, err := readImproveReport(experimentCandidateReport)
	if err != nil {
		return fmt.Errorf("read candidate report: %w", err)
	}
	report, err := improve.EvaluateExperiment(args[0], fixture,
		improve.ExperimentInput{Snapshot: baselineSnapshot, Report: baselineReport, AcceptancePassed: experimentBaselineAccepted, SafetyViolations: experimentBaselineSafetyViolations},
		improve.ExperimentInput{Snapshot: candidateSnapshot, Report: candidateReport, AcceptancePassed: experimentCandidateAccepted, SafetyViolations: experimentCandidateSafetyViolations},
	)
	if err != nil {
		return err
	}
	path, err := improve.WriteExperimentReport(workspace, report)
	if err != nil {
		return err
	}
	fmt.Printf("%s\nstatus: %s\ndecision: %s\n", path, report.Status, report.Decision)
	return nil
}

func resolveImproveTeamDir(teamName string) (string, error) {
	paths := team.DefaultSearchPaths()
	if strings.TrimSpace(improveSearchPath) != "" {
		paths = strings.Split(improveSearchPath, ",")
	}
	registry := team.NewTeamRegistry(paths)
	if err := registry.Discover(); err != nil {
		return "", fmt.Errorf("discover teams: %w", err)
	}
	dir, err := registry.Resolve(teamName)
	if err != nil {
		return "", fmt.Errorf("team %q not found. Available: %s", teamName, strings.Join(registry.ListTeams(), ", "))
	}
	return dir, nil
}

func resolveBenchmarkPath(workspace, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("benchmark is required")
	}
	if info, err := os.Stat(value); err == nil && !info.IsDir() {
		return filepath.Abs(value)
	}
	return improve.BenchmarkPath(workspace, value), nil
}

func readImproveReport(path string) (*improve.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var report improve.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	if strings.TrimSpace(report.Team) == "" {
		return nil, fmt.Errorf("report team is required")
	}
	return &report, nil
}

func parseBenchmarkCase(value string) (improve.BenchmarkCase, error) {
	parts := strings.SplitN(value, "::", 3)
	if len(parts) != 3 {
		return improve.BenchmarkCase{}, fmt.Errorf("invalid --case %q; use id::type::prompt", value)
	}
	return improve.BenchmarkCase{ID: strings.TrimSpace(parts[0]), Type: strings.TrimSpace(parts[1]), Prompt: strings.TrimSpace(parts[2])}, nil
}
