package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/processutil"
	"github.com/kjelly/hufu/internal/tools"
)

const (
	maxFiles           = 256
	maxEvidenceBytes   = 128 << 10
	maxGitHubBytes     = 2 << 20
	operationReview    = "review"
	operationSummary   = "summarize"
	operationCI        = "triage_ci"
	operationReadiness = "assess_readiness"
)

var (
	repositoryName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,38}/[a-zA-Z0-9_.-]{1,100}$`)
	gitSHA         = regexp.MustCompile(`^[a-f0-9]{40}$`)
	hunkHeader     = regexp.MustCompile(`^@@ -[0-9]+(,[0-9]+)? \+[0-9]+(,[0-9]+)? @@`)
)

type target struct {
	Action     string `json:"action"`
	Repository string `json:"repository"`
	Number     int    `json:"number"`
}

func (t target) validate() error {
	_, name, _ := strings.Cut(t.Repository, "/")
	if !repositoryName.MatchString(t.Repository) || name == "." || name == ".." || t.Number < 1 || t.Number > 2147483647 {
		return fmt.Errorf("provide a GitHub PR URL or owner/repo#number")
	}
	switch t.Action {
	case operationReview, operationSummary, operationCI, operationReadiness:
	default:
		return fmt.Errorf("choose one read-only action: review, summarize, triage_ci, or assess_readiness")
	}
	return nil
}

type request struct {
	Type           string          `json:"type"`
	InputName      string          `json:"input_name"`
	ResolverID     string          `json:"resolver_id"`
	CandidateValue json.RawMessage `json:"candidate_value"`
	ExplicitValue  json.RawMessage `json:"explicit_value"`
	Payload        string          `json:"payload"`
}

type resolverResult struct {
	Status     string          `json:"status"`
	Version    string          `json:"resolver_version"`
	Value      json.RawMessage `json:"value,omitempty"`
	Diagnostic string          `json:"diagnostic,omitempty"`
}

func resolve(req request) resolverResult {
	result := resolverResult{Status: "no_match", Version: "1"}
	if req.InputName != "pr.target" || req.ResolverID != "github-pr-v2" {
		result.Status = "invalid"
		result.Diagnostic = "unsupported PR resolver request"
		return result
	}
	for _, value := range []json.RawMessage{req.ExplicitValue, req.CandidateValue} {
		if len(value) == 0 || string(value) == "null" {
			continue
		}
		var pr target
		if json.Unmarshal(value, &pr) != nil {
			result.Status = "invalid"
			result.Diagnostic = "provide a GitHub PR URL or owner/repo#number"
			return result
		}
		if err := pr.validate(); err != nil {
			result.Status = "invalid"
			result.Diagnostic = err.Error()
			return result
		}
	}
	if len(req.CandidateValue) > 0 && string(req.CandidateValue) != "null" {
		result.Status = "matched"
		result.Value = req.CandidateValue
	}
	return result
}

type pullRequest struct {
	Number         int           `json:"number"`
	URL            string        `json:"url"`
	BaseSHA        string        `json:"baseRefOid"`
	HeadSHA        string        `json:"headRefOid"`
	ChangedFiles   int           `json:"changedFiles"`
	State          string        `json:"state"`
	Title          string        `json:"title"`
	Body           string        `json:"body"`
	Checks         []githubCheck `json:"statusCheckRollup"`
	Additions      int           `json:"additions"`
	Deletions      int           `json:"deletions"`
	BaseRef        string        `json:"baseRefName"`
	HeadRef        string        `json:"headRefName"`
	MergeState     string        `json:"mergeStateStatus"`
	Mergeable      string        `json:"mergeable"`
	ReviewDecision string        `json:"reviewDecision"`
	Draft          bool          `json:"isDraft"`
}

type githubCheck struct {
	Name       string `json:"name,omitempty"`
	Workflow   string `json:"workflowName,omitempty"`
	Context    string `json:"context,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	State      string `json:"state,omitempty"`
	DetailsURL string `json:"detailsUrl,omitempty"`
	TargetURL  string `json:"targetUrl,omitempty"`
}

type changedFile struct {
	Path         string `json:"filename"`
	PreviousPath string `json:"previous_filename,omitempty"`
	Status       string `json:"status"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	Patch        string `json:"patch,omitempty"`
}

type prScope struct {
	Requested        target      `json:"requested"`
	RequestedHash    string      `json:"requested_input_hash"`
	Satisfied        bool        `json:"satisfied"`
	URL              string      `json:"url"`
	BaseSHA          string      `json:"base_sha"`
	HeadSHA          string      `json:"head_sha"`
	ChangedFiles     int         `json:"changed_files"`
	UnavailableFiles []string    `json:"unavailable_files"`
	Checks           checkCounts `json:"checks"`
	Gaps             []string    `json:"evidence_gaps"`
	Readiness        string      `json:"readiness"`
}

type checkCounts struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Pending int `json:"pending"`
	Other   int `json:"other"`
}

func countChecks(checks []githubCheck) checkCounts {
	var counts checkCounts
	for _, check := range checks {
		switch checkResult(check) {
		case "passed":
			counts.Passed++
		case "failed":
			counts.Failed++
		case "pending":
			counts.Pending++
		default:
			counts.Other++
		}
	}
	return counts
}

func checkResult(check githubCheck) string {
	switch strings.ToUpper(check.Conclusion + check.State) {
	case "SUCCESS", "SKIPPED", "NEUTRAL":
		return "passed"
	case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "CANCELLED":
		return "failed"
	case "PENDING", "EXPECTED":
		return "pending"
	}
	if check.Status != "" && check.Status != "COMPLETED" {
		return "pending"
	}
	return "other"
}

type artifact struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	SHA256      string `json:"sha256"`
	Bytes       int    `json:"bytes"`
}

type result struct {
	Outputs   map[string]any `json:"outputs"`
	Artifacts []artifact     `json:"artifacts"`
}

// Run is the entry point of the team-owned trusted Go adapter.
func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(in, (512<<10)+1))
	if err != nil || len(raw) > 512<<10 {
		return fmt.Errorf("PR adapter request is unreadable or exceeds 512 KiB")
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("invalid PR adapter request")
	}
	if req.Type == "resolve_run_input" {
		return json.NewEncoder(out).Encode(resolve(req))
	}
	if req.Type != "fetch_github_pr" {
		return fmt.Errorf("unsupported PR adapter action")
	}
	var payload struct {
		Target target `json:"target"`
	}
	if err := json.Unmarshal([]byte(req.Payload), &payload); err != nil {
		return fmt.Errorf("invalid PR action payload")
	}
	if err := payload.Target.validate(); err != nil {
		return err
	}
	workspace := os.Getenv("HUFU_WORKSPACE")
	if !filepath.IsAbs(workspace) {
		return fmt.Errorf("PR action requires its invocation workspace")
	}
	accepted, err := fetchPR(ctx, payload.Target, workspace, runGitHub)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(accepted)
}

type githubCommand func(context.Context, string, []string) ([]byte, error)

func fetchPR(ctx context.Context, pr target, workspace string, run githubCommand) (result, error) {
	before, err := readPR(ctx, pr, workspace, run)
	if err != nil {
		return result{}, err
	}
	scope := newScope(pr, before)
	var section string
	switch pr.Action {
	case operationReview:
		section, err = collectDiff(ctx, workspace, &scope, run)
	case operationSummary:
		section = summarizeInput(before, &scope)
	case operationCI:
		section, err = collectCI(ctx, workspace, before, &scope, run)
	case operationReadiness:
		section, err = collectReadiness(ctx, workspace, before, &scope, run)
	default:
		return result{}, fmt.Errorf("unsupported PR action")
	}
	if err != nil {
		return result{}, err
	}
	// Summary metadata is one observation. The other operations use multiple
	// requests and need to detect a revision change before publishing them.
	if pr.Action != operationSummary {
		after, err := readPR(ctx, pr, workspace, run)
		if err != nil {
			return result{}, err
		}
		if before.HeadSHA != after.HeadSHA || before.BaseSHA != after.BaseSHA || before.ChangedFiles != after.ChangedFiles {
			return result{}, fmt.Errorf("PR base or head changed during evidence collection; start a new run")
		}
	}
	return writeEvidence(workspace, scope, before, section)
}

func collectDiff(ctx context.Context, workspace string, scope *prScope, run githubCommand) (string, error) {
	if scope.ChangedFiles > maxFiles {
		return "", fmt.Errorf("PR has %d changed files; the review limit is %d", scope.ChangedFiles, maxFiles)
	}
	pr := scope.Requested
	endpoint := fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", pr.Repository, pr.Number)
	raw, err := run(ctx, workspace, []string{"api", "--hostname", "github.com", "--method", "GET", "--paginate", "--slurp", endpoint})
	if err != nil {
		return "", fmt.Errorf("read PR changed files: %w", err)
	}
	var pages [][]changedFile
	if err := json.Unmarshal(raw, &pages); err != nil || len(pages) == 0 {
		return "", fmt.Errorf("GitHub returned invalid PR file pages")
	}
	diff, err := renderDiff(scope, pages)
	if err != nil {
		return "", err
	}
	if len(scope.UnavailableFiles) > 0 {
		scope.Gaps = append(scope.Gaps, "GitHub omitted or truncated one or more file patches. The affected paths are listed in unavailable_files.")
	}
	return "Changed files and unified patches\n" + diff, nil
}

func readPR(ctx context.Context, pr target, workspace string, run githubCommand) (pullRequest, error) {
	fields := "number,url,baseRefOid,headRefOid,changedFiles,state,title"
	switch pr.Action {
	case operationReview:
		fields += ",body,statusCheckRollup"
	case operationSummary:
		fields += ",body,additions,deletions,baseRefName,headRefName"
	case operationCI:
		fields += ",statusCheckRollup"
	case operationReadiness:
		fields += ",statusCheckRollup,isDraft,mergeStateStatus,mergeable,reviewDecision"
	}
	raw, err := run(ctx, workspace, []string{
		"pr", "view", strconv.Itoa(pr.Number), "--repo", "github.com/" + pr.Repository, "--json", fields,
	})
	if err != nil {
		return pullRequest{}, fmt.Errorf("read PR metadata and checks: %w", err)
	}
	var observed pullRequest
	wantURL := fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repository, pr.Number)
	if err := json.Unmarshal(raw, &observed); err != nil || observed.Number != pr.Number ||
		!strings.EqualFold(observed.URL, wantURL) || !gitSHA.MatchString(observed.BaseSHA) ||
		!gitSHA.MatchString(observed.HeadSHA) || observed.ChangedFiles < 0 {
		return pullRequest{}, fmt.Errorf("GitHub returned an invalid identity for the requested PR")
	}
	return observed, nil
}

func newScope(pr target, observed pullRequest) prScope {
	// Sorted object keys match Hufu's canonical JSON for this integer and ASCII slug.
	input := fmt.Appendf(nil, `{"action":%q,"number":%d,"repository":%q}`, pr.Action, pr.Number, pr.Repository)
	digest := sha256.Sum256(input)
	return prScope{
		Requested: pr, URL: observed.URL, BaseSHA: observed.BaseSHA, HeadSHA: observed.HeadSHA,
		ChangedFiles: observed.ChangedFiles, UnavailableFiles: []string{}, Checks: countChecks(observed.Checks),
		RequestedHash: "sha256:" + hex.EncodeToString(digest[:]), Satisfied: true,
		Gaps: []string{}, Readiness: "not_assessed",
	}
}

func renderDiff(scope *prScope, pages [][]changedFile) (string, error) {
	seen := make(map[string]bool)
	var diff strings.Builder
	for _, page := range pages {
		for _, file := range page {
			if file.Path == "" || seen[file.Path] || len(seen) == maxFiles {
				return "", fmt.Errorf("GitHub returned a missing, duplicate, or out-of-budget PR file")
			}
			seen[file.Path] = true
			fmt.Fprintf(&diff, "\nFile %q (%s, +%d/-%d)\n", file.Path, file.Status, file.Additions, file.Deletions)
			if file.PreviousPath != "" {
				fmt.Fprintf(&diff, "Previous path %q\n", file.PreviousPath)
			}
			if !completePatch(file) {
				scope.UnavailableFiles = append(scope.UnavailableFiles, file.Path)
				diff.WriteString("Patch omitted or incomplete in GitHub's response.\n")
			} else {
				diff.WriteString(file.Patch)
				diff.WriteByte('\n')
			}
		}
	}
	if len(seen) != scope.ChangedFiles {
		return "", fmt.Errorf("GitHub reported %d changed files but delivered %d", scope.ChangedFiles, len(seen))
	}
	return diff.String(), nil
}

func completePatch(file changedFile) bool {
	if !strings.HasPrefix(file.Patch, "@@ ") {
		return false
	}
	added, deleted := 0, 0
	oldLeft, newLeft := 0, 0
	for _, line := range strings.Split(strings.TrimSuffix(file.Patch, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "@@ "):
			match := hunkHeader.FindStringSubmatch(line)
			if len(match) != 3 || oldLeft != 0 || newLeft != 0 {
				return false
			}
			oldLeft, newLeft = hunkCount(match[1]), hunkCount(match[2])
		case strings.HasPrefix(line, "+"):
			added++
			newLeft--
		case strings.HasPrefix(line, "-"):
			deleted++
			oldLeft--
		case strings.HasPrefix(line, " "):
			oldLeft--
			newLeft--
		case line == `\ No newline at end of file`:
		default:
			return false
		}
		if oldLeft < 0 || newLeft < 0 {
			return false
		}
	}
	return oldLeft == 0 && newLeft == 0 && added == file.Additions && deleted == file.Deletions
}

func hunkCount(suffix string) int {
	if suffix == "" {
		return 1
	}
	count, err := strconv.Atoi(suffix[1:])
	if err != nil {
		return -1
	}
	return count
}

func writeEvidence(workspace string, scope prScope, observed pullRequest, section string) (result, error) {
	checks, err := json.Marshal(observed.Checks)
	if err != nil {
		return result{}, err
	}
	data := fmt.Appendf(nil, "GitHub PR %s at %s (base %s)\nAction: %s\nState: %s\nTitle: %q\n\n",
		scope.URL, scope.HeadSHA, scope.BaseSHA, scope.Requested.Action, observed.State, observed.Title)
	if scope.Requested.Action == operationReview {
		data = fmt.Appendf(data, "Description: %q\n", observed.Body)
	}
	if scope.Requested.Action != operationSummary {
		data = fmt.Appendf(data, "Recorded GitHub checks: %s\n\n", checks)
	}
	data = append(data, section...)
	if len(data) > maxEvidenceBytes {
		return result{}, fmt.Errorf("PR evidence exceeds the %d-byte publication limit", maxEvidenceBytes)
	}
	evidence, err := writeArtifact(workspace, "github-pr-evidence.txt", "github_pr_evidence", "GitHub PR task evidence", data)
	if err != nil {
		return result{}, fmt.Errorf("write PR evidence: %w", err)
	}
	coverage := tools.ArtifactReadCoverage{
		ArtifactID: evidence.ID, SHA256: evidence.SHA256, ByteSize: int64(evidence.Bytes),
		Ranges: []tools.ArtifactByteRange{{Start: 0, End: int64(evidence.Bytes)}},
	}
	manifest, err := json.Marshal(map[string]any{
		"schema_version": 2,
		"items": []map[string]any{{
			"key":            fmt.Sprintf("%s#%d", scope.Requested.Repository, scope.Requested.Number),
			"bindings":       map[string]string{"pr_url": scope.URL, "pr_action": scope.Requested.Action},
			"inputs":         []artifact{evidence},
			"required_reads": []tools.ArtifactReadCoverage{coverage},
		}},
	})
	if err != nil {
		return result{}, err
	}
	workset, err := writeArtifact(workspace, "github-pr-workset.json", "workset_manifest", "GitHub PR workset", manifest)
	if err != nil {
		return result{}, fmt.Errorf("write PR review workset: %w", err)
	}
	return result{Outputs: map[string]any{"scope": scope}, Artifacts: []artifact{evidence, workset}}, nil
}

func writeArtifact(workspace, path, kind, description string, data []byte) (artifact, error) {
	if err := os.WriteFile(filepath.Join(workspace, path), data, 0o600); err != nil {
		return artifact{}, err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	return artifact{ID: "sha256-" + digest, Path: path, Kind: kind, Description: description, SHA256: digest, Bytes: len(data)}, nil
}

type boundedOutput struct {
	data bytes.Buffer
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > maxGitHubBytes-b.data.Len() {
		return 0, fmt.Errorf("GitHub response exceeds %d bytes", maxGitHubBytes)
	}
	return b.data.Write(p)
}

func runGitHub(ctx context.Context, workspace string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1")
	var stdout boundedOutput
	cmd.Stdout = &stdout
	var logs jobLog
	logRequest := strings.HasSuffix(args[len(args)-1], "/logs")
	if logRequest {
		cmd.Stdout = &logs
	}
	if err := processutil.RunContextCommand(cmd, 500*time.Millisecond); err != nil {
		return nil, fmt.Errorf("GitHub CLI request failed; verify gh installation, GitHub authentication and PR access: %w", err)
	}
	if logRequest {
		return logs.excerpt(), nil
	}
	return stdout.data.Bytes(), nil
}
