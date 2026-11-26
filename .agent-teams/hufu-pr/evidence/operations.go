package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxSummaryBody = 16 << 10
	maxFailedLogs  = 3
	maxLogTail     = 8 << 10
	maxLogSignals  = 8 << 10
	maxLogLine     = 2048
)

func summarizeInput(pr pullRequest, scope *prScope) string {
	body := pr.Body
	if len(body) > maxSummaryBody {
		body = body[:maxSummaryBody]
		body = strings.ToValidUTF8(body, "")
		scope.Gaps = append(scope.Gaps, "PR description exceeds the summary budget. Only its beginning was collected.")
	}
	return fmt.Sprintf("Branch change: %q into %q\nDiff totals: %d files, +%d/-%d\nAuthor-provided description and testing claims: %q\n",
		pr.HeadRef, pr.BaseRef, pr.ChangedFiles, pr.Additions, pr.Deletions, body)
}

var actionJobPath = regexp.MustCompile(`^/actions/runs/[1-9][0-9]{0,18}/job/([1-9][0-9]{0,18})$`)

func actionsJobID(repository, url string) string {
	prefix := "https://github.com/" + repository
	if len(url) <= len(prefix) || !strings.EqualFold(url[:len(prefix)], prefix) {
		return ""
	}
	match := actionJobPath.FindStringSubmatch(url[len(prefix):])
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

type actionJob struct {
	ID      int64  `json:"id"`
	URL     string `json:"html_url"`
	Name    string `json:"name"`
	Outcome string `json:"conclusion"`
	Steps   []struct {
		Name    string `json:"name"`
		Outcome string `json:"conclusion"`
	} `json:"steps"`
}

func collectCI(ctx context.Context, workspace string, observed pullRequest, scope *prScope, run githubCommand) (string, error) {
	var section strings.Builder
	section.WriteString("CI diagnosis evidence\n")
	if len(observed.Checks) == 0 {
		scope.Gaps = append(scope.Gaps, "GitHub returned no check results for this PR revision.")
	}
	seen := make(map[string]bool)
	missing, tried := 0, 0
	for _, check := range observed.Checks {
		if checkResult(check) != "failed" {
			continue
		}
		id := actionsJobID(scope.Requested.Repository, check.DetailsURL)
		if id == "" {
			missing++
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if tried == maxFailedLogs {
			missing++
			continue
		}
		tried++
		if err := appendJobEvidence(ctx, workspace, scope.Requested.Repository, id, check.DetailsURL, run, &section); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			fmt.Fprintf(&section, "\nGitHub Actions job %s at %s: %s\n", id, check.DetailsURL, err.Error())
			missing++
		}
	}
	if missing > 0 {
		scope.Gaps = append(scope.Gaps, fmt.Sprintf("%d failing or cancelled checks had no collected log because of access, source, or the %d-job budget.", missing, maxFailedLogs))
	}
	return section.String(), nil
}

func appendJobEvidence(ctx context.Context, workspace, repo, id, observedURL string, run githubCommand, section *strings.Builder) error {
	endpoint := "repos/" + repo + "/actions/jobs/" + id
	args := []string{"api", "--hostname", "github.com", "--method", "GET"}
	raw, err := run(ctx, workspace, append(args, endpoint))
	if err != nil {
		return fmt.Errorf("job metadata unavailable")
	}
	var job actionJob
	if json.Unmarshal(raw, &job) != nil || strconv.FormatInt(job.ID, 10) != id || !strings.EqualFold(job.URL, observedURL) {
		return fmt.Errorf("job metadata does not match the PR check")
	}
	fmt.Fprintf(section, "\nGitHub Actions job %q (%s) %s\n", job.Name, job.Outcome, observedURL)
	for _, step := range job.Steps {
		if step.Outcome == "failure" || step.Outcome == "timed_out" || step.Outcome == "cancelled" {
			fmt.Fprintf(section, "Failed or cancelled step: %q (%s)\n", step.Name, step.Outcome)
		}
	}
	log, err := run(ctx, workspace, append(args, endpoint+"/logs"))
	if err != nil || len(log) == 0 {
		return fmt.Errorf("job log unavailable")
	}
	fmt.Fprintf(section, "Bounded log evidence (diagnostic lines and tail):\n%s\n", log)
	return nil
}

// Capture diagnostic lines from the entire stream and keep a bounded tail.
// GitHub Actions logs can be much larger than the ordinary JSON response limit.
type jobLog struct {
	tail    []byte
	line    []byte
	signals bytes.Buffer
	clipped bool
}

var (
	logSignal     = regexp.MustCompile(`(?i)(::error|\bFAIL\b|error:|panic:|data race|timed out|no space left|exception:|exit code [1-9])`)
	logANSI       = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")
	logCredential = regexp.MustCompile(`(?i)(github_pat_[A-Za-z0-9_]+|gh[pousr]_[A-Za-z0-9_]+|sk-[A-Za-z0-9_-]{16,}|AKIA[A-Z0-9]{16}|xox[baprs]-[A-Za-z0-9-]+|(authorization|token|secret|password|api[_-]?key)["']?[\t ]*[:=][\t ]*["']?((bearer|basic)[\t ]+)?[^\s"']+)`)
	logPrivateKey = regexp.MustCompile(`(?s)-----BEGIN [^\n]*PRIVATE KEY-----.*?(-----END [^\n]*PRIVATE KEY-----|$)`)
)

func (log *jobLog) Write(data []byte) (int, error) {
	length := len(data)
	if length >= maxLogTail {
		log.clipped = log.clipped || len(log.tail) > 0 || length > maxLogTail
		log.tail = append(log.tail[:0], data[length-maxLogTail:]...)
	} else {
		log.tail = append(log.tail, data...)
		if len(log.tail) > maxLogTail {
			log.clipped = true
			copy(log.tail, log.tail[len(log.tail)-maxLogTail:])
			log.tail = log.tail[:maxLogTail]
		}
	}
	for len(data) > 0 && log.signals.Len() < maxLogSignals {
		line, remaining, newline := bytes.Cut(data, []byte{'\n'})
		space := maxLogLine - len(log.line)
		if len(line) > space {
			line = line[:space]
		}
		log.line = append(log.line, line...)
		if newline {
			log.captureLine()
			log.line = log.line[:0]
		}
		data = remaining
	}
	return length, nil
}

func (log *jobLog) captureLine() {
	line := logANSI.ReplaceAll(log.line, nil)
	if !logSignal.Match(line) || log.signals.Len()+len(line)+1 > maxLogSignals {
		return
	}
	log.signals.Write(line)
	log.signals.WriteByte('\n')
}

func (log *jobLog) excerpt() []byte {
	if len(log.tail) == 0 {
		return nil
	}
	log.captureLine()
	tail := log.tail
	if log.clipped {
		// Skip an incomplete first line, which may also have lost the key
		// that identifies a credential on that line.
		_, tail, _ = bytes.Cut(tail, []byte{'\n'})
	}
	data := fmt.Appendf(nil, "Selected diagnostic lines:\n%s\nLog tail:\n%s", log.signals.String(), tail)
	data = logANSI.ReplaceAll(data, nil)
	data = logPrivateKey.ReplaceAll(data, []byte("[REDACTED]"))
	data = logCredential.ReplaceAll(data, []byte("[REDACTED]"))
	if !utf8.Valid(data) {
		data = bytes.ToValidUTF8(data, []byte("�"))
	}
	return bytes.Map(logTextRune, data)
}

func logTextRune(r rune) rune {
	if r != '\n' && r != '\t' && unicode.IsControl(r) {
		return -1
	}
	return r
}

const reviewThreadsQuery = `query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      number baseRefOid headRefOid
      reviewThreads(first: 100) {
        totalCount pageInfo { hasNextPage }
        nodes { isResolved path line comments(first: 1) { nodes { url } } }
      }
    }
  }
}`

type reviewThreads struct {
	Count    int `json:"totalCount"`
	PageInfo struct {
		HasNextPage bool `json:"hasNextPage"`
	} `json:"pageInfo"`
	Nodes []struct {
		Resolved bool   `json:"isResolved"`
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Comments struct {
			Nodes []struct {
				URL string `json:"url"`
			} `json:"nodes"`
		} `json:"comments"`
	} `json:"nodes"`
}

func readReviewThreads(ctx context.Context, workspace string, scope *prScope, run githubCommand) (*reviewThreads, error) {
	pr := scope.Requested
	owner, repo, _ := strings.Cut(pr.Repository, "/")
	raw, err := run(ctx, workspace, []string{
		"api", "graphql", "--hostname", "github.com", "-f", "query=" + reviewThreadsQuery,
		"-f", "owner=" + owner, "-f", "repo=" + repo, "-F", "number=" + strconv.Itoa(pr.Number),
	})
	if err != nil {
		return nil, err
	}
	var response struct {
		Data struct {
			Repository struct {
				PR struct {
					Number  int            `json:"number"`
					BaseSHA string         `json:"baseRefOid"`
					HeadSHA string         `json:"headRefOid"`
					Threads *reviewThreads `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Errors) > 0 {
		return nil, fmt.Errorf("invalid GitHub review thread response")
	}
	target := response.Data.Repository.PR
	if target.Number != pr.Number || target.BaseSHA != scope.BaseSHA || target.HeadSHA != scope.HeadSHA {
		return nil, fmt.Errorf("review threads do not match the PR revision")
	}
	return target.Threads, nil
}

func collectReadiness(ctx context.Context, workspace string, pr pullRequest, scope *prScope, run githubCommand) (string, error) {
	threads, err := readReviewThreads(ctx, workspace, scope, run)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	var section strings.Builder
	fmt.Fprintf(&section, "Draft: %t\nMergeable: %q\nMerge state: %q\nReview decision: %q\n", pr.Draft, pr.Mergeable, pr.MergeState, pr.ReviewDecision)
	unresolved := 0
	complete := err == nil && threads != nil && !threads.PageInfo.HasNextPage && len(threads.Nodes) == threads.Count
	if threads != nil && err == nil {
		fmt.Fprintf(&section, "Review threads: %d read out of %d\n", len(threads.Nodes), threads.Count)
		for _, thread := range threads.Nodes {
			if thread.Resolved {
				continue
			}
			unresolved++
			url := ""
			if len(thread.Comments.Nodes) > 0 {
				url = thread.Comments.Nodes[0].URL
			}
			fmt.Fprintf(&section, "Unresolved thread at %q line %d %q\n", thread.Path, thread.Line, url)
		}
	}
	if !complete {
		scope.Gaps = append(scope.Gaps, "GitHub review-thread coverage is incomplete. Up to 100 threads can be collected.")
	}
	if len(pr.Checks) == 0 {
		scope.Gaps = append(scope.Gaps, "GitHub returned no check results for this PR revision.")
	}
	scope.Readiness = assessReadiness(pr, scope.Checks, unresolved, complete)
	if scope.Readiness == "undetermined" && len(scope.Gaps) == 0 {
		scope.Gaps = append(scope.Gaps, "GitHub's merge state or check conclusions do not establish readiness.")
	}
	fmt.Fprintf(&section, "Readiness from observed GitHub signals: %s\n", scope.Readiness)
	return section.String(), nil
}

func assessReadiness(pr pullRequest, checks checkCounts, unresolved int, completeThreads bool) string {
	if pr.State == "CLOSED" || pr.State == "MERGED" || pr.Draft || pr.Mergeable == "CONFLICTING" || unresolved > 0 ||
		pr.ReviewDecision == "REVIEW_REQUIRED" || pr.ReviewDecision == "CHANGES_REQUESTED" ||
		checks.Failed > 0 || checks.Pending > 0 {
		return "blocked"
	}
	switch pr.MergeState {
	case "BLOCKED", "BEHIND", "DIRTY", "DRAFT", "UNSTABLE":
		return "blocked"
	}
	if pr.State == "OPEN" && pr.MergeState == "CLEAN" && pr.Mergeable == "MERGEABLE" &&
		completeThreads && checks.Passed > 0 && checks.Other == 0 {
		return "ready"
	}
	return "undetermined"
}
