package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNonReviewActionsDoNotFetchDiff(t *testing.T) {
	pr := target{Repository: "example/project", Number: 53}
	head, base := strings.Repeat("b", 40), strings.Repeat("a", 40)
	const jobURL = "https://github.com/example/project/actions/runs/100/job/123"
	const foreignJob = "https://github.com/someone/other/actions/runs/200/job/456"
	for _, test := range []struct {
		action, evidence, readiness string
		jobCalls, threadCalls       int
		gap                         bool
	}{
		{action: operationSummary, evidence: "Author-provided description and testing claims"},
		{action: operationCI, evidence: "Failed or cancelled step", readiness: "not_assessed", jobCalls: 2, gap: true},
		{action: operationReadiness, evidence: "Unresolved thread at", readiness: "blocked", threadCalls: 1},
	} {
		t.Run(test.action, func(t *testing.T) {
			pr.Action = test.action
			workspace := t.TempDir()
			jobCalls, threadCalls := 0, 0
			run := func(_ context.Context, _ string, args []string) ([]byte, error) {
				t.Helper()
				if args[0] == "pr" && args[1] == "view" {
					check := githubCheck{Status: "COMPLETED", Conclusion: "SUCCESS"}
					metadata := pullRequest{
						Number: 53, URL: "https://github.com/example/project/pull/53",
						BaseSHA: base, HeadSHA: head, ChangedFiles: maxFiles + 40, State: "OPEN",
						Title: "Large PR", Body: "Authors say they ran npm test.", Mergeable: "MERGEABLE", MergeState: "CLEAN",
						Checks: []githubCheck{check},
					}
					if test.action == operationCI {
						metadata.Checks = []githubCheck{
							{Status: "COMPLETED", Conclusion: "CANCELLED", DetailsURL: jobURL},
							{Conclusion: "FAILURE", DetailsURL: foreignJob},
						}
					}
					return json.Marshal(metadata)
				}
				if args[1] == "graphql" && test.action == operationReadiness {
					threadCalls++
					return fmt.Appendf(nil, `{"data":{"repository":{"pullRequest":{"number":53,"baseRefOid":%q,"headRefOid":%q,"reviewThreads":{"totalCount":1,"pageInfo":{"hasNextPage":false},"nodes":[{"isResolved":false,"path":"src/main.go","line":12}]}}}}}`, base, head), nil
				}
				endpoint := args[len(args)-1]
				if endpoint == "repos/example/project/actions/jobs/123" && test.action == operationCI {
					jobCalls++
					return fmt.Appendf(nil, `{"id":123,"html_url":%q,"name":"CI","conclusion":"cancelled","steps":[{"name":"Run tests","conclusion":"cancelled"}]}`, jobURL), nil
				}
				if endpoint == "repos/example/project/actions/jobs/123/logs" && test.action == operationCI {
					jobCalls++
					return []byte("FAIL: TestExample"), nil
				}
				t.Fatalf("unexpected GitHub command: %v", args)
				return nil, nil
			}
			got, err := fetchPR(t.Context(), pr, workspace, run)
			if err != nil {
				t.Fatal(err)
			}
			scope := got.Outputs["scope"].(prScope)
			if scope.Requested != pr || scope.ChangedFiles <= maxFiles ||
				(test.readiness != "" && scope.Readiness != test.readiness) || (len(scope.Gaps) > 0) != test.gap ||
				jobCalls != test.jobCalls || threadCalls != test.threadCalls {
				t.Fatalf("unexpected collection: %+v, %d jobs, %d threads", scope, jobCalls, threadCalls)
			}
			out, err := os.ReadFile(filepath.Join(workspace, got.Artifacts[0].Path))
			if err != nil || !strings.Contains(string(out), test.evidence) ||
				strings.Contains(string(out), "Changed files and unified patches") {
				t.Fatalf("unexpected evidence %q: %v", out, err)
			}
		})
	}
}

func TestReadinessVerdictUsesObservedSignals(t *testing.T) {
	passed := githubCheck{Status: "COMPLETED", Conclusion: "SUCCESS"}
	skipped := githubCheck{Status: "COMPLETED", Conclusion: "SKIPPED"}
	neutral := githubCheck{Status: "COMPLETED", Conclusion: "NEUTRAL"}
	for _, test := range []struct {
		name, want string
		decision   string
		checks     []githubCheck
		counts     checkCounts
		threadsOK  bool
	}{
		{name: "all observed gates pass", want: "ready", threadsOK: true, checks: []githubCheck{passed, skipped, neutral}, counts: checkCounts{Passed: 3}},
		{name: "skipped and neutral satisfy checks", want: "ready", threadsOK: true, checks: []githubCheck{skipped, neutral}, counts: checkCounts{Passed: 2}},
		{name: "review required", want: "blocked", decision: "REVIEW_REQUIRED", threadsOK: true, checks: []githubCheck{passed}, counts: checkCounts{Passed: 1}},
		{name: "failed status context", want: "blocked", threadsOK: true, checks: []githubCheck{passed, {State: "FAILURE"}}, counts: checkCounts{Passed: 1, Failed: 1}},
		{name: "cancelled check", want: "blocked", threadsOK: true, checks: []githubCheck{passed, {Status: "COMPLETED", Conclusion: "CANCELLED"}}, counts: checkCounts{Passed: 1, Failed: 1}},
		{name: "pending check", want: "blocked", threadsOK: true, checks: []githubCheck{passed, {Status: "IN_PROGRESS"}}, counts: checkCounts{Passed: 1, Pending: 1}},
		{name: "unknown conclusion", want: "undetermined", threadsOK: true, checks: []githubCheck{passed, {Status: "COMPLETED", Conclusion: "UNKNOWN"}}, counts: checkCounts{Passed: 1, Other: 1}},
		{name: "missing checks", want: "undetermined", threadsOK: true},
		{name: "partial thread coverage", want: "undetermined", checks: []githubCheck{passed}, counts: checkCounts{Passed: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pr := pullRequest{State: "OPEN", Mergeable: "MERGEABLE", MergeState: "CLEAN", ReviewDecision: test.decision}
			counts := countChecks(test.checks)
			if counts != test.counts {
				t.Errorf("check counts = %+v, want %+v", counts, test.counts)
			}
			if got := assessReadiness(pr, counts, 0, test.threadsOK); got != test.want {
				t.Fatalf("readiness = %q, want %q", got, test.want)
			}
		})
	}
}

func TestJobLogKeepsEarlierDiagnosticsAndMasksCredentials(t *testing.T) {
	var log jobLog
	for _, chunk := range []string{
		"\x1b[31mFA", "IL: regression token=examplecredential\x1b[0m\n",
		strings.Repeat("ordinary output\n", 2000), "job cleanup finished\n",
		"authorization: Basic fixturepassword\n",
	} {
		if written, err := log.Write([]byte(chunk)); err != nil || written != len(chunk) {
			t.Fatalf("write = %d, %v", written, err)
		}
	}
	evidence := string(log.excerpt())
	if !strings.Contains(evidence, "FAIL: regression [REDACTED]") ||
		!strings.Contains(evidence, "job cleanup finished") || strings.Contains(evidence, "examplecredential") ||
		strings.Contains(evidence, "fixturepassword") || strings.Contains(evidence, "\x1b") || len(evidence) > maxLogSignals+maxLogTail+80 {
		t.Fatal("log excerpt did not retain bounded, redacted evidence")
	}
}
