package improve

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	promotionVersion  = 1
	adoptionVersion   = 1
	monitoringVersion = 1
	knowledgeVersion  = 1
)

type PullRequestPlan struct {
	ExperimentID        string
	Team                string
	Repository          string
	BaseBranch          string
	Branch              string
	Title               string
	Body                string
	PatchPath           string
	CandidateSnapshotID string
	CandidateRevision   string
	BaselineRevision    string
}

// PromotionRecord records a PR created by hufu. It deliberately has no merge
// state: merges remain a human-controlled repository action.
type PromotionRecord struct {
	Version             int    `json:"version"`
	ExperimentID        string `json:"experiment_id"`
	Team                string `json:"team"`
	PullRequestURL      string `json:"pull_request_url"`
	Branch              string `json:"branch"`
	BaseBranch          string `json:"base_branch"`
	CandidateSnapshotID string `json:"candidate_snapshot_id"`
	CandidateRevision   string `json:"candidate_revision"`
	BaselineRevision    string `json:"baseline_revision"`
	CreatedAt           string `json:"created_at"`
}

// Adoption records the human-confirmed rollout of a promoted candidate. Its
// baseline revision is the only rollback target suggested by monitoring.
type Adoption struct {
	Version             int      `json:"version"`
	ID                  string   `json:"id"`
	ExperimentID        string   `json:"experiment_id"`
	Team                string   `json:"team"`
	PullRequestURL      string   `json:"pull_request_url"`
	BaselineSnapshotID  string   `json:"baseline_snapshot_id"`
	CandidateSnapshotID string   `json:"candidate_snapshot_id"`
	RollbackRevision    string   `json:"rollback_revision"`
	CandidateRevision   string   `json:"candidate_revision"`
	BaselineMetrics     Metrics  `json:"baseline_metrics"`
	CandidateMetrics    Metrics  `json:"candidate_metrics"`
	IssueType           string   `json:"issue_type"`
	ChangeSummary       string   `json:"change_summary"`
	Conditions          []string `json:"conditions"`
	AdoptedAt           string   `json:"adopted_at"`
}

type KnowledgeEntry struct {
	Version           int      `json:"version"`
	ID                string   `json:"id"`
	AddedAt           string   `json:"added_at"`
	Team              string   `json:"team"`
	IssueType         string   `json:"issue_type"`
	EffectiveChange   string   `json:"effective_change"`
	Conditions        []string `json:"conditions"`
	ExperimentID      string   `json:"experiment_id"`
	AdoptionID        string   `json:"adoption_id"`
	BaselineRevision  string   `json:"baseline_revision"`
	CandidateRevision string   `json:"candidate_revision"`
	Outcome           string   `json:"outcome"`
}

type MonitoringIssue struct {
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
}

type RollbackSuggestion struct {
	AdoptionID         string `json:"adoption_id"`
	Team               string `json:"team"`
	BaselineSnapshotID string `json:"baseline_snapshot_id"`
	RollbackRevision   string `json:"rollback_revision"`
	Reason             string `json:"reason"`
	Action             string `json:"action"`
}

// MonitoringReport summarizes post-adoption production telemetry without
// storing prompts, outputs, tool arguments, or tool results.
type MonitoringReport struct {
	Version            int                 `json:"version"`
	AdoptionID         string              `json:"adoption_id"`
	Team               string              `json:"team"`
	GeneratedAt        string              `json:"generated_at"`
	ProductionRunIDs   []string            `json:"production_run_ids"`
	ExpectedRevision   string              `json:"expected_revision"`
	BaselineMetrics    Metrics             `json:"baseline_metrics"`
	ProductionMetrics  Metrics             `json:"production_metrics"`
	AcceptancePassed   bool                `json:"acceptance_passed"`
	SafetyViolations   int                 `json:"safety_violations"`
	Status             string              `json:"status"`
	Issues             []MonitoringIssue   `json:"issues"`
	RollbackSuggestion *RollbackSuggestion `json:"rollback_suggestion,omitempty"`
}

type CommandRunner interface {
	Run(dir, name string, args ...string) (string, error)
}

type osCommandRunner struct{}

func (osCommandRunner) Run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(output), nil
}

func ExperimentReportPath(workspace, id string) string {
	return filepath.Join(ImprovementRoot(workspace), "experiments", id, "report.json")
}

func LoadExperimentReport(workspace, id string) (ExperimentReport, error) {
	if err := validateArtifactID(id); err != nil {
		return ExperimentReport{}, err
	}
	data, err := os.ReadFile(ExperimentReportPath(workspace, id))
	if err != nil {
		return ExperimentReport{}, fmt.Errorf("read experiment report: %w", err)
	}
	var report ExperimentReport
	if err := json.Unmarshal(data, &report); err != nil {
		return ExperimentReport{}, fmt.Errorf("parse experiment report: %w", err)
	}
	if report.Version != experimentVersion || report.ID != id {
		return ExperimentReport{}, fmt.Errorf("invalid experiment report %q", id)
	}
	return report, nil
}

// PreparePullRequest validates that an experiment is eligible for review and
// constructs the exact local Git/GitHub work needed to create a PR. It makes
// no repository changes itself.
func PreparePullRequest(workspace, experimentID, repository, baseBranch, title string) (PullRequestPlan, error) {
	report, err := LoadExperimentReport(workspace, experimentID)
	if err != nil {
		return PullRequestPlan{}, err
	}
	if report.Status != "passed" || report.Decision != "eligible_for_review" {
		return PullRequestPlan{}, fmt.Errorf("experiment %q is not eligible for a pull request (status=%s decision=%s)", experimentID, report.Status, report.Decision)
	}
	candidate, candidateDir, err := LoadCandidateSnapshot(workspace, report.Candidate.SnapshotID)
	if err != nil {
		return PullRequestPlan{}, fmt.Errorf("load candidate snapshot: %w", err)
	}
	patchPath := filepath.Join(candidateDir, "candidate.patch")
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		return PullRequestPlan{}, fmt.Errorf("read candidate patch: %w", err)
	}
	if strings.TrimSpace(string(patch)) == "" {
		return PullRequestPlan{}, fmt.Errorf("candidate patch is empty")
	}
	repository, err = filepath.Abs(repository)
	if err != nil {
		return PullRequestPlan{}, fmt.Errorf("resolve repository: %w", err)
	}
	info, err := os.Stat(repository)
	if err != nil {
		return PullRequestPlan{}, fmt.Errorf("inspect repository: %w", err)
	}
	if !info.IsDir() {
		return PullRequestPlan{}, fmt.Errorf("repository %s is not a directory", repository)
	}
	baseBranch = strings.TrimSpace(baseBranch)
	if baseBranch == "" {
		return PullRequestPlan{}, fmt.Errorf("base branch is required")
	}
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("feat(%s): adopt %s experiment", report.Candidate.SnapshotID, experimentID)
	}
	return PullRequestPlan{
		ExperimentID: experimentID, Team: candidate.Team, Repository: repository, BaseBranch: baseBranch,
		Branch: "hufu/improve/" + experimentID, Title: title,
		Body: pullRequestBody(report, candidate), PatchPath: patchPath,
		CandidateSnapshotID: candidate.ID, CandidateRevision: candidate.DefinitionRevision, BaselineRevision: report.Baseline.DefinitionRevision,
	}, nil
}

// CreatePullRequest applies an already-reviewed candidate patch in a clean
// repository, commits it on a dedicated branch, and invokes `gh pr create`.
// It never invokes any merge command. If an external command fails after the
// branch is created, the branch and commit are intentionally left intact for
// human inspection instead of being forcefully reset.
func CreatePullRequest(plan PullRequestPlan) (PromotionRecord, error) {
	return CreatePullRequestWithRunner(plan, osCommandRunner{})
}

func CreatePullRequestWithRunner(plan PullRequestPlan, runner CommandRunner) (PromotionRecord, error) {
	if runner == nil {
		return PromotionRecord{}, fmt.Errorf("command runner is required")
	}
	if strings.TrimSpace(plan.Repository) == "" || strings.TrimSpace(plan.PatchPath) == "" || strings.TrimSpace(plan.Branch) == "" {
		return PromotionRecord{}, fmt.Errorf("incomplete pull request plan")
	}
	if status, err := runner.Run(plan.Repository, "git", "status", "--porcelain"); err != nil {
		return PromotionRecord{}, commandError("check repository status", err, status)
	} else if strings.TrimSpace(status) != "" {
		return PromotionRecord{}, fmt.Errorf("repository has uncommitted changes; refusing to create a pull request")
	}
	if output, err := runner.Run(plan.Repository, "git", "apply", "--check", plan.PatchPath); err != nil {
		return PromotionRecord{}, commandError("validate candidate patch", err, output)
	}
	if output, err := runner.Run(plan.Repository, "git", "switch", "-c", plan.Branch, plan.BaseBranch); err != nil {
		return PromotionRecord{}, commandError("create pull request branch", err, output)
	}
	if output, err := runner.Run(plan.Repository, "git", "apply", plan.PatchPath); err != nil {
		return PromotionRecord{}, commandError("apply candidate patch", err, output)
	}
	if output, err := runner.Run(plan.Repository, "git", "add", "-A"); err != nil {
		return PromotionRecord{}, commandError("stage candidate patch", err, output)
	}
	if output, err := runner.Run(plan.Repository, "git", "commit", "-m", plan.Title); err != nil {
		return PromotionRecord{}, commandError("commit candidate patch", err, output)
	}
	if output, err := runner.Run(plan.Repository, "git", "push", "--set-upstream", "origin", plan.Branch); err != nil {
		return PromotionRecord{}, commandError("push pull request branch", err, output)
	}
	url, err := runner.Run(plan.Repository, "gh", "pr", "create", "--base", plan.BaseBranch, "--head", plan.Branch, "--title", plan.Title, "--body", plan.Body)
	if err != nil {
		return PromotionRecord{}, commandError("create pull request", err, url)
	}
	url = strings.TrimSpace(url)
	if url == "" {
		return PromotionRecord{}, fmt.Errorf("GitHub CLI created no pull request URL")
	}
	return PromotionRecord{
		Version: promotionVersion, ExperimentID: plan.ExperimentID, Team: plan.Team, PullRequestURL: url,
		Branch: plan.Branch, BaseBranch: plan.BaseBranch, CandidateSnapshotID: plan.CandidateSnapshotID,
		CandidateRevision: plan.CandidateRevision, BaselineRevision: plan.BaselineRevision,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func WritePromotionRecord(workspace string, record PromotionRecord) (string, error) {
	if err := validateArtifactID(record.ExperimentID); err != nil {
		return "", err
	}
	if record.Version != promotionVersion || strings.TrimSpace(record.PullRequestURL) == "" {
		return "", fmt.Errorf("invalid promotion record")
	}
	path := filepath.Join(ImprovementRoot(workspace), "experiments", record.ExperimentID, "promotion.json")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("promotion record already exists; refusing to overwrite it")
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect promotion record: %w", err)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal promotion record: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write promotion record: %w", err)
	}
	return path, nil
}

func CreateAdoption(workspace, id, experimentID, pullRequestURL, changeSummary string, conditions []string) (Adoption, string, error) {
	if err := validateArtifactID(id); err != nil {
		return Adoption{}, "", err
	}
	if strings.TrimSpace(pullRequestURL) == "" {
		return Adoption{}, "", fmt.Errorf("pull request URL is required")
	}
	if strings.TrimSpace(changeSummary) == "" {
		return Adoption{}, "", fmt.Errorf("change summary is required")
	}
	report, err := LoadExperimentReport(workspace, experimentID)
	if err != nil {
		return Adoption{}, "", err
	}
	if report.Status != "passed" || report.Decision != "eligible_for_review" {
		return Adoption{}, "", fmt.Errorf("experiment %q is not eligible for adoption", experimentID)
	}
	candidate, _, err := LoadCandidateSnapshot(workspace, report.Candidate.SnapshotID)
	if err != nil {
		return Adoption{}, "", err
	}
	conditions = normalizedConditions(conditions, report, candidate)
	dir := filepath.Join(ImprovementRoot(workspace), "adoptions", id)
	if err := createArtifactDir(dir); err != nil {
		return Adoption{}, "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	adoption := Adoption{
		Version: adoptionVersion, ID: id, ExperimentID: experimentID, Team: candidate.Team, PullRequestURL: pullRequestURL,
		BaselineSnapshotID: report.Baseline.SnapshotID, CandidateSnapshotID: candidate.ID,
		RollbackRevision: report.Baseline.DefinitionRevision, CandidateRevision: candidate.DefinitionRevision,
		BaselineMetrics: report.Baseline.Metrics, CandidateMetrics: report.Candidate.Metrics,
		IssueType: report.Benchmark.Category, ChangeSummary: strings.TrimSpace(changeSummary), Conditions: conditions,
		AdoptedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(adoption, "", "  ")
	if err != nil {
		return Adoption{}, "", fmt.Errorf("marshal adoption: %w", err)
	}
	path := filepath.Join(dir, "adoption.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return Adoption{}, "", fmt.Errorf("write adoption: %w", err)
	}
	if err := appendKnowledgeEntry(workspace, knowledgeFromAdoption(adoption)); err != nil {
		return Adoption{}, "", err
	}
	complete = true
	return adoption, path, nil
}

func LoadAdoption(workspace, id string) (Adoption, error) {
	if err := validateArtifactID(id); err != nil {
		return Adoption{}, err
	}
	path := filepath.Join(ImprovementRoot(workspace), "adoptions", id, "adoption.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return Adoption{}, fmt.Errorf("read adoption: %w", err)
	}
	var adoption Adoption
	if err := json.Unmarshal(data, &adoption); err != nil {
		return Adoption{}, fmt.Errorf("parse adoption: %w", err)
	}
	if adoption.Version != adoptionVersion || adoption.ID != id {
		return Adoption{}, fmt.Errorf("invalid adoption %q", id)
	}
	return adoption, nil
}

// EvaluateMonitoring compares production telemetry with the baseline captured
// before adoption. It proposes rollback; it never applies one.
func EvaluateMonitoring(adoption Adoption, production *Report, acceptancePassed bool, safetyViolations int) (MonitoringReport, error) {
	if production == nil {
		return MonitoringReport{}, fmt.Errorf("production report is required")
	}
	if safetyViolations < 0 {
		return MonitoringReport{}, fmt.Errorf("safety violations cannot be negative")
	}
	if !strings.EqualFold(adoption.Team, production.Team) {
		return MonitoringReport{}, fmt.Errorf("production report team %q does not match adoption team %q", production.Team, adoption.Team)
	}
	issues := make([]MonitoringIssue, 0)
	revisionMatches := contains(production.TeamRevisions, adoption.CandidateRevision)
	if !revisionMatches {
		issues = append(issues, MonitoringIssue{Type: "revision_drift", Severity: "warning", Detail: "production report does not reference the adopted candidate revision"})
	}
	if safetyViolations > 0 {
		issues = append(issues, MonitoringIssue{Type: "safety_regression", Severity: "critical", Detail: fmt.Sprintf("%d production safety violations", safetyViolations)})
	}
	if !acceptancePassed {
		issues = append(issues, MonitoringIssue{Type: "acceptance_regression", Severity: "critical", Detail: "production acceptance gate failed"})
	}
	if completionRate(production.Metrics) < completionRate(adoption.BaselineMetrics) {
		issues = append(issues, MonitoringIssue{Type: "completion_regression", Severity: "warning", Detail: fmt.Sprintf("production completion %.2f%% is below baseline %.2f%%", completionRate(production.Metrics)*100, completionRate(adoption.BaselineMetrics)*100)})
	}
	if production.Metrics.Error > adoption.BaselineMetrics.Error {
		issues = append(issues, MonitoringIssue{Type: "error_regression", Severity: "warning", Detail: fmt.Sprintf("production errors %d exceed baseline %d", production.Metrics.Error, adoption.BaselineMetrics.Error)})
	}

	status := "healthy"
	for _, issue := range issues {
		if issue.Severity == "critical" || issue.Type == "completion_regression" || issue.Type == "error_regression" {
			status = "degraded"
			break
		}
	}
	if status == "healthy" && !revisionMatches {
		status = "inconclusive"
	}
	report := MonitoringReport{
		Version: monitoringVersion, AdoptionID: adoption.ID, Team: adoption.Team, GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		ProductionRunIDs: append([]string(nil), production.RunIDs...), ExpectedRevision: adoption.CandidateRevision,
		BaselineMetrics: adoption.BaselineMetrics, ProductionMetrics: production.Metrics,
		AcceptancePassed: acceptancePassed, SafetyViolations: safetyViolations, Status: status, Issues: issues,
	}
	if status == "degraded" {
		report.RollbackSuggestion = &RollbackSuggestion{
			AdoptionID: adoption.ID, Team: adoption.Team, BaselineSnapshotID: adoption.BaselineSnapshotID,
			RollbackRevision: adoption.RollbackRevision, Reason: monitoringReason(issues),
			Action: "Create a review patch from the immutable baseline snapshot and restore it through the normal PR workflow; do not reset or merge automatically.",
		}
	}
	return report, nil
}

func WriteMonitoringReport(workspace string, report MonitoringReport) (string, error) {
	if err := validateArtifactID(report.AdoptionID); err != nil {
		return "", err
	}
	dir := filepath.Join(ImprovementRoot(workspace), "monitoring", report.AdoptionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create monitoring directory: %w", err)
	}
	name := "report-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	jsonPath := filepath.Join(dir, name+".json")
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal monitoring report: %w", err)
	}
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		return "", fmt.Errorf("write monitoring report: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(MonitoringMarkdown(report)), 0o644); err != nil {
		return "", fmt.Errorf("write monitoring markdown: %w", err)
	}
	return jsonPath, nil
}

func MonitoringMarkdown(report MonitoringReport) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Agent Team Production Monitoring Report")
	fmt.Fprintf(&b, "\n- **Adoption**: %s\n- **Team**: %s\n- **Status**: %s\n- **Expected revision**: %s\n- **Production runs**: %s\n", report.AdoptionID, report.Team, report.Status, report.ExpectedRevision, strings.Join(report.ProductionRunIDs, ", "))
	fmt.Fprint(&b, "\n## Metrics\n\n")
	fmt.Fprintln(&b, "| Metric | Baseline | Production |")
	fmt.Fprintln(&b, "|---|---:|---:|")
	fmt.Fprintf(&b, "| Completion rate | %.2f%% | %.2f%% |\n", completionRate(report.BaselineMetrics)*100, completionRate(report.ProductionMetrics)*100)
	fmt.Fprintf(&b, "| Errors | %d | %d |\n", report.BaselineMetrics.Error, report.ProductionMetrics.Error)
	fmt.Fprintf(&b, "| Retried tasks | %d | %d |\n", report.BaselineMetrics.RetriedTasks, report.ProductionMetrics.RetriedTasks)
	fmt.Fprintf(&b, "| Total tokens | %d | %d |\n", report.BaselineMetrics.TotalTokens, report.ProductionMetrics.TotalTokens)
	fmt.Fprint(&b, "\n## Issues\n\n")
	if len(report.Issues) == 0 {
		fmt.Fprintln(&b, "No regressions detected in the supplied production telemetry.")
	} else {
		for _, issue := range report.Issues {
			fmt.Fprintf(&b, "- **%s** `%s`: %s\n", strings.ToUpper(issue.Severity), issue.Type, issue.Detail)
		}
	}
	if report.RollbackSuggestion != nil {
		suggestion := report.RollbackSuggestion
		fmt.Fprint(&b, "\n## Rollback Suggestion\n\n")
		fmt.Fprintf(&b, "- **Baseline snapshot**: %s\n- **Rollback revision**: %s\n- **Reason**: %s\n- **Action**: %s\n", suggestion.BaselineSnapshotID, suggestion.RollbackRevision, suggestion.Reason, suggestion.Action)
	}
	return b.String()
}

func ListKnowledge(workspace, issueType string) ([]KnowledgeEntry, error) {
	path := filepath.Join(ImprovementRoot(workspace), "knowledge", "entries.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read knowledge base: %w", err)
	}
	defer func() { _ = f.Close() }()
	entries := make([]KnowledgeEntry, 0)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		var entry KnowledgeEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil || entry.Version != knowledgeVersion {
			continue
		}
		if issueType != "" && !strings.EqualFold(entry.IssueType, issueType) {
			continue
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("read knowledge base: %w", err)
	}
	return entries, nil
}

func KnowledgeMarkdown(entries []KnowledgeEntry) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Agent Team Improvement Knowledge")
	if len(entries) == 0 {
		fmt.Fprintln(&b, "\nNo adopted improvement knowledge has been recorded.")
		return b.String()
	}
	fmt.Fprintln(&b, "\n| Issue type | Effective change | Conditions | Experiment | Adoption |")
	fmt.Fprintln(&b, "|---|---|---|---|---|")
	for _, entry := range entries {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", entry.IssueType, entry.EffectiveChange, strings.Join(entry.Conditions, "; "), entry.ExperimentID, entry.AdoptionID)
	}
	return b.String()
}

func pullRequestBody(report ExperimentReport, candidate TeamSnapshot) string {
	return fmt.Sprintf("## Hufu improvement experiment\n\n- Experiment: `%s`\n- Benchmark: `%s` (%s)\n- Candidate snapshot: `%s`\n- Candidate revision: `%s`\n- Baseline revision: `%s`\n\nAll deterministic gates passed. This PR was created for human review and is not eligible for automatic merge.\n", report.ID, report.Benchmark.Name, report.Benchmark.Revision, candidate.ID, candidate.DefinitionRevision, report.Baseline.DefinitionRevision)
}

func commandError(action string, err error, output string) error {
	output = strings.TrimSpace(output)
	if output == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, output)
}

func normalizedConditions(conditions []string, report ExperimentReport, candidate TeamSnapshot) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(conditions)+2)
	for _, condition := range append(conditions, "benchmark:"+report.Benchmark.Name, "candidate_revision:"+candidate.DefinitionRevision) {
		condition = strings.TrimSpace(condition)
		if condition == "" {
			continue
		}
		if _, exists := seen[condition]; exists {
			continue
		}
		seen[condition] = struct{}{}
		result = append(result, condition)
	}
	return result
}

func knowledgeFromAdoption(adoption Adoption) KnowledgeEntry {
	return KnowledgeEntry{
		Version: knowledgeVersion, ID: adoption.ID, AddedAt: adoption.AdoptedAt, Team: adoption.Team,
		IssueType: adoption.IssueType, EffectiveChange: adoption.ChangeSummary, Conditions: append([]string(nil), adoption.Conditions...),
		ExperimentID: adoption.ExperimentID, AdoptionID: adoption.ID, BaselineRevision: adoption.RollbackRevision,
		CandidateRevision: adoption.CandidateRevision, Outcome: "adopted",
	}
}

func appendKnowledgeEntry(workspace string, entry KnowledgeEntry) error {
	dir := filepath.Join(ImprovementRoot(workspace), "knowledge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create knowledge directory: %w", err)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal knowledge entry: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "entries.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open knowledge base: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append knowledge entry: %w", err)
	}
	return nil
}

func monitoringReason(issues []MonitoringIssue) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		if issue.Severity == "critical" || issue.Type == "completion_regression" || issue.Type == "error_regression" {
			parts = append(parts, issue.Type)
		}
	}
	return strings.Join(parts, ", ")
}
