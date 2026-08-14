package improve

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	improvementDir        = "improvement"
	benchmarkVersion      = 1
	snapshotVersion       = 1
	experimentVersion     = 1
	baselineSnapshotKind  = "baseline"
	candidateSnapshotKind = "candidate"
)

// BenchmarkFixture is an authored, versioned set of fixed prompts. Prompts
// intentionally live in the fixture, never in execution telemetry or reports.
type BenchmarkFixture struct {
	Version     int             `json:"version" yaml:"version"`
	Name        string          `json:"name" yaml:"name"`
	Team        string          `json:"team" yaml:"team"`
	Category    string          `json:"category" yaml:"category"`
	Description string          `json:"description,omitempty" yaml:"description,omitempty"`
	Cases       []BenchmarkCase `json:"cases" yaml:"cases"`
}

type BenchmarkCase struct {
	ID     string `json:"id" yaml:"id"`
	Type   string `json:"type" yaml:"type"`
	Prompt string `json:"prompt" yaml:"prompt"`
}

// TeamSnapshot is an immutable copy of a team definition for an experiment.
// DefinitionRevision matches the hash written to execution telemetry. Content
// Revision covers every regular file copied into the snapshot.
type TeamSnapshot struct {
	Version            int    `json:"version"`
	ID                 string `json:"id"`
	Kind               string `json:"kind"`
	Team               string `json:"team"`
	DefinitionRevision string `json:"definition_revision"`
	ContentRevision    string `json:"content_revision"`
	PatchRevision      string `json:"patch_revision,omitempty"`
	BaselineID         string `json:"baseline_id,omitempty"`
	CreatedAt          string `json:"created_at"`
}

type BenchmarkRef struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
	Category string `json:"category"`
	Cases    int    `json:"cases"`
}

type ExperimentArm struct {
	SnapshotID         string   `json:"snapshot_id"`
	DefinitionRevision string   `json:"definition_revision"`
	ContentRevision    string   `json:"content_revision"`
	RunIDs             []string `json:"run_ids"`
	Metrics            Metrics  `json:"metrics"`
	AcceptancePassed   bool     `json:"acceptance_passed"`
	SafetyViolations   int      `json:"safety_violations"`
}

type GateResult struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
}

// ExperimentReport is an immutable result of comparing one baseline and one
// candidate on the same benchmark. A passing experiment is only eligible for
// human review; it never authorizes an automatic team change.
type ExperimentReport struct {
	Version     int           `json:"version"`
	ID          string        `json:"id"`
	GeneratedAt string        `json:"generated_at"`
	Benchmark   BenchmarkRef  `json:"benchmark"`
	Baseline    ExperimentArm `json:"baseline"`
	Candidate   ExperimentArm `json:"candidate"`
	Gates       []GateResult  `json:"gates"`
	Status      string        `json:"status"`
	Decision    string        `json:"decision"`
}

type ExperimentInput struct {
	Snapshot         TeamSnapshot
	Report           *Report
	AcceptancePassed bool
	SafetyViolations int
}

// ImprovementRoot returns the parent directory for durable improvement
// artifacts in a workspace.
func ImprovementRoot(workspace string) string {
	return filepath.Join(workspace, improvementDir)
}

func BenchmarkPath(workspace, name string) string {
	return filepath.Join(ImprovementRoot(workspace), "benchmarks", name, "benchmark.yaml")
}

func CreateBenchmark(workspace string, fixture BenchmarkFixture) (string, string, error) {
	var err error
	fixture, err = normalizedBenchmark(fixture)
	if err != nil {
		return "", "", err
	}
	targetDir := filepath.Dir(BenchmarkPath(workspace, fixture.Name))
	if err := createArtifactDir(targetDir); err != nil {
		return "", "", err
	}
	data, err := yaml.Marshal(fixture)
	if err != nil {
		return "", "", fmt.Errorf("marshal benchmark: %w", err)
	}
	path := filepath.Join(targetDir, "benchmark.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", "", fmt.Errorf("write benchmark: %w", err)
	}
	return path, BenchmarkRevision(fixture), nil
}

func LoadBenchmark(path string) (BenchmarkFixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return BenchmarkFixture{}, fmt.Errorf("read benchmark: %w", err)
	}
	var fixture BenchmarkFixture
	if err := yaml.Unmarshal(data, &fixture); err != nil {
		return BenchmarkFixture{}, fmt.Errorf("parse benchmark: %w", err)
	}
	fixture, err = normalizedBenchmark(fixture)
	if err != nil {
		return BenchmarkFixture{}, err
	}
	return fixture, nil
}

func BenchmarkRevision(fixture BenchmarkFixture) string {
	if fixture.Version == 0 {
		fixture.Version = benchmarkVersion
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:])
}

func CreateBaselineSnapshot(workspace, id, sourceDir string) (TeamSnapshot, string, error) {
	return createSnapshot(workspace, id, baselineSnapshotKind, sourceDir, "", "")
}

func LoadBaselineSnapshot(workspace, id string) (TeamSnapshot, string, error) {
	return LoadSnapshot(workspace, baselineSnapshotKind, id)
}

func LoadCandidateSnapshot(workspace, id string) (TeamSnapshot, string, error) {
	return LoadSnapshot(workspace, candidateSnapshotKind, id)
}

func CreateCandidateSnapshot(workspace, id, baselineID, sourceDir, patchPath string) (TeamSnapshot, string, error) {
	baseline, _, err := LoadSnapshot(workspace, baselineSnapshotKind, baselineID)
	if err != nil {
		return TeamSnapshot{}, "", fmt.Errorf("load baseline snapshot: %w", err)
	}
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		return TeamSnapshot{}, "", fmt.Errorf("read candidate patch: %w", err)
	}
	if len(strings.TrimSpace(string(patch))) == 0 {
		return TeamSnapshot{}, "", fmt.Errorf("candidate patch is empty")
	}
	snapshot, dir, err := createSnapshot(workspace, id, candidateSnapshotKind, sourceDir, baselineID, string(patch))
	if err != nil {
		return TeamSnapshot{}, "", err
	}
	if !strings.EqualFold(snapshot.Team, baseline.Team) {
		_ = os.RemoveAll(dir)
		return TeamSnapshot{}, "", fmt.Errorf("candidate team %q does not match baseline team %q", snapshot.Team, baseline.Team)
	}
	if snapshot.ContentRevision == baseline.ContentRevision {
		_ = os.RemoveAll(dir)
		return TeamSnapshot{}, "", fmt.Errorf("candidate content is identical to baseline; provide a changed team definition")
	}
	return snapshot, dir, nil
}

func LoadSnapshot(workspace, kind, id string) (TeamSnapshot, string, error) {
	if err := validateArtifactID(id); err != nil {
		return TeamSnapshot{}, "", err
	}
	if kind != baselineSnapshotKind && kind != candidateSnapshotKind {
		return TeamSnapshot{}, "", fmt.Errorf("unknown snapshot kind %q", kind)
	}
	dir := filepath.Join(ImprovementRoot(workspace), kind+"s", id)
	data, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		return TeamSnapshot{}, "", fmt.Errorf("read snapshot: %w", err)
	}
	var snapshot TeamSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return TeamSnapshot{}, "", fmt.Errorf("parse snapshot: %w", err)
	}
	if snapshot.Version != snapshotVersion || snapshot.ID != id || snapshot.Kind != kind {
		return TeamSnapshot{}, "", fmt.Errorf("invalid %s snapshot %q", kind, id)
	}
	if _, err := os.Stat(filepath.Join(dir, "team")); err != nil {
		return TeamSnapshot{}, "", fmt.Errorf("snapshot team directory: %w", err)
	}
	return snapshot, dir, nil
}

func createSnapshot(workspace, id, kind, sourceDir, baselineID, patch string) (snapshot TeamSnapshot, resultDir string, err error) {
	if err := validateArtifactID(id); err != nil {
		return TeamSnapshot{}, "", err
	}
	if kind != baselineSnapshotKind && kind != candidateSnapshotKind {
		return TeamSnapshot{}, "", fmt.Errorf("unknown snapshot kind %q", kind)
	}
	sourceDir, err = filepath.Abs(sourceDir)
	if err != nil {
		return TeamSnapshot{}, "", fmt.Errorf("resolve team source: %w", err)
	}
	definition, err := readTeamDefinition(sourceDir)
	if err != nil {
		return TeamSnapshot{}, "", err
	}
	teamName := strings.TrimSpace(definition.Name)
	if teamName == "" {
		teamName = filepath.Base(sourceDir)
	}
	resultDir = filepath.Join(ImprovementRoot(workspace), kind+"s", id)
	if err := createArtifactDir(resultDir); err != nil {
		return TeamSnapshot{}, "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(resultDir)
		}
	}()
	teamDir := filepath.Join(resultDir, "team")
	if err := copyTree(sourceDir, teamDir); err != nil {
		return TeamSnapshot{}, "", err
	}
	contentRevision, err := directoryRevision(teamDir)
	if err != nil {
		return TeamSnapshot{}, "", err
	}
	snapshot = TeamSnapshot{
		Version:            snapshotVersion,
		ID:                 id,
		Kind:               kind,
		Team:               teamName,
		DefinitionRevision: definitionRevision(teamDir),
		ContentRevision:    contentRevision,
		BaselineID:         baselineID,
		CreatedAt:          time.Now().UTC().Format(time.RFC3339),
	}
	if patch != "" {
		sum := sha256.Sum256([]byte(patch))
		snapshot.PatchRevision = fmt.Sprintf("%x", sum[:])
		if err := os.WriteFile(filepath.Join(resultDir, "candidate.patch"), []byte(patch), 0o644); err != nil {
			return TeamSnapshot{}, "", fmt.Errorf("write candidate patch: %w", err)
		}
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return TeamSnapshot{}, "", fmt.Errorf("marshal snapshot: %w", err)
	}
	if err := os.WriteFile(filepath.Join(resultDir, "snapshot.json"), data, 0o644); err != nil {
		return TeamSnapshot{}, "", fmt.Errorf("write snapshot: %w", err)
	}
	complete = true
	return snapshot, resultDir, nil
}

// EvaluateExperiment compares reports generated from the immutable snapshots
// on the same benchmark. It intentionally does not run agents, write team
// definitions, open pull requests, or make an adoption decision.
func EvaluateExperiment(id string, fixture BenchmarkFixture, baseline, candidate ExperimentInput) (ExperimentReport, error) {
	if err := validateArtifactID(id); err != nil {
		return ExperimentReport{}, err
	}
	var err error
	fixture, err = normalizedBenchmark(fixture)
	if err != nil {
		return ExperimentReport{}, err
	}
	if err := validateExperimentInput("baseline", fixture, baseline); err != nil {
		return ExperimentReport{}, err
	}
	if err := validateExperimentInput("candidate", fixture, candidate); err != nil {
		return ExperimentReport{}, err
	}
	if candidate.Snapshot.Kind != candidateSnapshotKind {
		return ExperimentReport{}, fmt.Errorf("candidate input must use a candidate snapshot")
	}
	if candidate.Snapshot.BaselineID != baseline.Snapshot.ID {
		return ExperimentReport{}, fmt.Errorf("candidate snapshot baseline %q does not match %q", candidate.Snapshot.BaselineID, baseline.Snapshot.ID)
	}

	baselineArm := experimentArm(baseline)
	candidateArm := experimentArm(candidate)
	gates := []GateResult{
		{
			Name: "baseline_control", Passed: baseline.AcceptancePassed,
			Expected: "baseline acceptance passed", Observed: boolText(baseline.AcceptancePassed),
		},
		{
			Name: "candidate_safety", Passed: candidate.SafetyViolations == 0,
			Expected: "0 safety violations", Observed: fmt.Sprintf("%d safety violations", candidate.SafetyViolations),
		},
		{
			Name: "candidate_acceptance", Passed: candidate.AcceptancePassed,
			Expected: "candidate acceptance passed", Observed: boolText(candidate.AcceptancePassed),
		},
		{
			Name: "completion_non_regression", Passed: completionRate(candidate.Report.Metrics) >= completionRate(baseline.Report.Metrics),
			Expected: fmt.Sprintf(">= %.2f%% baseline completion", completionRate(baseline.Report.Metrics)*100),
			Observed: fmt.Sprintf("%.2f%% candidate completion", completionRate(candidate.Report.Metrics)*100),
		},
		{
			Name: "error_non_regression", Passed: candidate.Report.Metrics.Error <= baseline.Report.Metrics.Error,
			Expected: fmt.Sprintf("<= %d baseline errors", baseline.Report.Metrics.Error),
			Observed: fmt.Sprintf("%d candidate errors", candidate.Report.Metrics.Error),
		},
		{
			Name: "memory_harmful_rate", Passed: candidate.Report.Metrics.MemoryHarmfulUseRate == 0,
			Expected: "0 harmful memory uses", Observed: fmt.Sprintf("%.4f harmful use rate", candidate.Report.Metrics.MemoryHarmfulUseRate),
		},
		{
			Name: "memory_attribution_coverage", Passed: candidate.Report.Metrics.MemoryAttributionCoverage >= baseline.Report.Metrics.MemoryAttributionCoverage,
			Expected: fmt.Sprintf(">= %.4f baseline coverage", baseline.Report.Metrics.MemoryAttributionCoverage), Observed: fmt.Sprintf("%.4f candidate coverage", candidate.Report.Metrics.MemoryAttributionCoverage),
		},
		{
			Name: "memory_token_overhead", Passed: candidate.Report.Metrics.MemoryTokenOverhead <= math.Max(baseline.Report.Metrics.MemoryTokenOverhead*1.10, 0.10),
			Expected: "<= 10% overhead or <= 110% of baseline", Observed: fmt.Sprintf("%.4f candidate overhead", candidate.Report.Metrics.MemoryTokenOverhead),
		},
		{
			Name: "memory_stale_rate", Passed: candidate.Report.Metrics.MemoryStaleRetrievalRate <= baseline.Report.Metrics.MemoryStaleRetrievalRate,
			Expected: fmt.Sprintf("<= %.4f baseline stale rate", baseline.Report.Metrics.MemoryStaleRetrievalRate), Observed: fmt.Sprintf("%.4f candidate stale rate", candidate.Report.Metrics.MemoryStaleRetrievalRate),
		},
		{
			Name: "retry_non_regression", Passed: candidate.Report.Metrics.RetriedTasks <= baseline.Report.Metrics.RetriedTasks,
			Expected: fmt.Sprintf("<= %d baseline retried tasks", baseline.Report.Metrics.RetriedTasks), Observed: fmt.Sprintf("%d candidate retried tasks", candidate.Report.Metrics.RetriedTasks),
		},
	}

	status, decision := "passed", "eligible_for_review"
	for _, gate := range gates {
		if gate.Passed {
			continue
		}
		if gate.Name == "baseline_control" {
			status, decision = "inconclusive", "retry"
			break
		}
		status, decision = "failed", "reject"
	}
	return ExperimentReport{
		Version: experimentVersion, ID: id, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Benchmark: BenchmarkRef{Name: fixture.Name, Revision: BenchmarkRevision(fixture), Category: fixture.Category, Cases: len(fixture.Cases)},
		Baseline:  baselineArm, Candidate: candidateArm, Gates: gates, Status: status, Decision: decision,
	}, nil
}

func WriteExperimentReport(workspace string, report ExperimentReport) (string, error) {
	if err := validateArtifactID(report.ID); err != nil {
		return "", err
	}
	dir := filepath.Join(ImprovementRoot(workspace), "experiments", report.ID)
	if err := createArtifactDir(dir); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal experiment report: %w", err)
	}
	jsonPath := filepath.Join(dir, "report.json")
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		return "", fmt.Errorf("write experiment report: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(ExperimentMarkdown(report)), 0o644); err != nil {
		return "", fmt.Errorf("write experiment markdown: %w", err)
	}
	return jsonPath, nil
}

func ExperimentMarkdown(report ExperimentReport) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Agent Team Experiment Report")
	fmt.Fprintf(&b, "\n- **Experiment**: %s\n- **Status**: %s\n- **Decision**: %s\n- **Benchmark**: %s (%s, %d cases)\n", report.ID, report.Status, report.Decision, report.Benchmark.Name, report.Benchmark.Revision, report.Benchmark.Cases)
	fmt.Fprint(&b, "\n## Comparison\n\n")
	fmt.Fprintln(&b, "| Arm | Snapshot | Tasks | Done | Error | Retried | Tokens | Acceptance | Safety violations |")
	fmt.Fprintln(&b, "|---|---|---:|---:|---:|---:|---:|---|---:|")
	writeExperimentArm(&b, "Baseline", report.Baseline)
	writeExperimentArm(&b, "Candidate", report.Candidate)
	fmt.Fprint(&b, "\n## Gates\n\n")
	fmt.Fprintln(&b, "| Gate | Result | Expected | Observed |")
	fmt.Fprintln(&b, "|---|---|---|---|")
	for _, gate := range report.Gates {
		result := "PASS"
		if !gate.Passed {
			result = "FAIL"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", gate.Name, result, gate.Expected, gate.Observed)
	}
	fmt.Fprintln(&b, "\nA passing result is eligible for human review only. It does not apply a candidate, create a pull request, or change the formal team.")
	return b.String()
}

func writeExperimentArm(b *strings.Builder, label string, arm ExperimentArm) {
	fmt.Fprintf(b, "| %s | %s | %d | %d | %d | %d | %d | %t | %d |\n", label, arm.SnapshotID, arm.Metrics.TotalTasks, arm.Metrics.Done, arm.Metrics.Error, arm.Metrics.RetriedTasks, arm.Metrics.TotalTokens, arm.AcceptancePassed, arm.SafetyViolations)
}

func validateBenchmark(fixture BenchmarkFixture) error {
	if fixture.Version != benchmarkVersion {
		return fmt.Errorf("unsupported benchmark version %d", fixture.Version)
	}
	if err := validateArtifactID(fixture.Name); err != nil {
		return fmt.Errorf("benchmark name: %w", err)
	}
	if strings.TrimSpace(fixture.Team) == "" {
		return fmt.Errorf("benchmark team is required")
	}
	if strings.TrimSpace(fixture.Category) == "" {
		return fmt.Errorf("benchmark category is required")
	}
	if len(fixture.Cases) == 0 {
		return fmt.Errorf("benchmark requires at least one case")
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, item := range fixture.Cases {
		if err := validateArtifactID(item.ID); err != nil {
			return fmt.Errorf("benchmark case: %w", err)
		}
		if _, exists := seen[item.ID]; exists {
			return fmt.Errorf("duplicate benchmark case %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		switch item.Type {
		case "happy", "failure", "edge", "safety":
		default:
			return fmt.Errorf("benchmark case %q has unsupported type %q", item.ID, item.Type)
		}
		if strings.TrimSpace(item.Prompt) == "" {
			return fmt.Errorf("benchmark case %q prompt is required", item.ID)
		}
	}
	return nil
}

func normalizedBenchmark(fixture BenchmarkFixture) (BenchmarkFixture, error) {
	if fixture.Version == 0 {
		fixture.Version = benchmarkVersion
	}
	if err := validateBenchmark(fixture); err != nil {
		return BenchmarkFixture{}, err
	}
	return fixture, nil
}

func validateExperimentInput(label string, fixture BenchmarkFixture, input ExperimentInput) error {
	if input.Report == nil {
		return fmt.Errorf("%s report is required", label)
	}
	if input.SafetyViolations < 0 {
		return fmt.Errorf("%s safety violations cannot be negative", label)
	}
	if label == "baseline" && input.Snapshot.Kind != baselineSnapshotKind {
		return fmt.Errorf("baseline input must use a baseline snapshot")
	}
	if !strings.EqualFold(input.Snapshot.Team, fixture.Team) || !strings.EqualFold(input.Report.Team, fixture.Team) {
		return fmt.Errorf("%s team must match benchmark team %q", label, fixture.Team)
	}
	if input.Snapshot.DefinitionRevision == "" {
		return fmt.Errorf("%s snapshot is missing a definition revision", label)
	}
	if !contains(input.Report.TeamRevisions, input.Snapshot.DefinitionRevision) {
		return fmt.Errorf("%s report does not reference snapshot definition revision", label)
	}
	if input.Report.Metrics.TotalTasks < len(fixture.Cases) {
		return fmt.Errorf("%s report has %d tasks for %d benchmark cases", label, input.Report.Metrics.TotalTasks, len(fixture.Cases))
	}
	return nil
}

func experimentArm(input ExperimentInput) ExperimentArm {
	return ExperimentArm{
		SnapshotID: input.Snapshot.ID, DefinitionRevision: input.Snapshot.DefinitionRevision, ContentRevision: input.Snapshot.ContentRevision,
		RunIDs: append([]string(nil), input.Report.RunIDs...), Metrics: input.Report.Metrics,
		AcceptancePassed: input.AcceptancePassed, SafetyViolations: input.SafetyViolations,
	}
}

func completionRate(metrics Metrics) float64 {
	if metrics.TotalTasks == 0 {
		return 0
	}
	return float64(metrics.Done) / float64(metrics.TotalTasks)
}

func boolText(value bool) string {
	if value {
		return "passed"
	}
	return "failed"
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validateArtifactID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("name is required")
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return fmt.Errorf("invalid name %q: use letters, numbers, hyphens, and underscores", id)
	}
	return nil
}

func createArtifactDir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("artifact directory %s already exists; refusing to overwrite it", dir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect artifact directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}
	return nil
}

func copyTree(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("inspect team source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("team source %s is not a directory", source)
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(target, 0o755)
		}
		destination := filepath.Join(target, rel)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("team source contains unsupported symlink %s", rel)
		}
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("team source contains unsupported file %s", rel)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func directoryRevision(dir string) (string, error) {
	files := make([]string, 0)
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported snapshot file %s", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		return "", err
	}
	sort.Strings(files)
	hash := sha256.New()
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return "", err
		}
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
