package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func TestResultsDefaultReprintsCompleteLatestFinalResponse(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	_, output, err := executeResultsTest(t, deps, "--team", "review", "--workspace", workspace)
	if err != nil || output != "coordinator final response\n" {
		t.Fatalf("default output is not the final response body: %q, %v", output, err)
	}
	root := &cobra.Command{Use: "hufu"}
	root.AddCommand(newResultsCommandWithDeps(deps))
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"result", "--team", "review", "--workspace", workspace})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != output {
		t.Fatalf("singular alias changed the response: %q", stdout.String())
	}
}

func TestResultsDefaultDoesNotPreviewMergeOrReplaceFinalBody(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	body := "完整最終回覆\n\n```go\n\tif  ready {\n    value := \"a  b\"\n}\n```\nMarkdown line break  \n" + strings.Repeat("完整 findings 內容，不能截斷。\n", 2000) + "最後一行"
	deps.load = func(_ context.Context, scope inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error) {
		return inspectpkg.HistoricalSnapshot{Coverage: inspectpkg.HistoricalCoverage{Scope: scope}, Results: []inspectpkg.HistoricalResult{
			{Kind: "run_response", Team: "review", RunID: "old", Output: "OLD_RESPONSE", CompletedAt: time.Unix(1, 0)},
			{Kind: "run_response", Team: "review", RunID: "latest", Output: body, Summary: "SUMMARY_MUST_NOT_REPLACE_BODY", CompletedAt: time.Unix(2, 0)},
			{Kind: "task", Team: "review", RunID: "latest", Output: "WORKER_RECEIPT", CompletedAt: time.Unix(3, 0)},
		}}, nil
	}
	_, output, err := executeResultsTest(t, deps, "--team", "review", "--workspace", workspace)
	if err != nil || output != body+"\n" {
		t.Fatalf("default altered or truncated the final body (length %d): %v", len(output), err)
	}
	value, _, err := executeResultsTest(t, deps, "--team", "review", "--workspace", workspace, "--output", "json")
	if err != nil || len(value.Results) != 1 || value.Results[0].Output != body || value.Results[0].Kind != "run_response" {
		t.Fatalf("default JSON changed final body: %v", err)
	}
}

func TestResultsReadPreservesFormattingAndStripsTerminalControls(t *testing.T) {
	var output bytes.Buffer
	response := resultsEnvelope{Results: []inspectpkg.HistoricalResult{{Kind: "run_response", Output: "\x1b]52;c;clipboard-data\a## Final\n\t    a  b  \n\x1b[31mred\x1b[0m"}}}
	if err := writeFinalResponse(&output, "text", response); err != nil {
		t.Fatal(err)
	}
	if output.String() != "## Final\n\t    a  b  \nred\n" {
		t.Fatalf("body formatting or terminal safety changed: %q", output.String())
	}
}

func TestResultsDefaultHasNoWorkerOrDiagnosticFallback(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	for _, rows := range [][]inspectpkg.HistoricalResult{
		{},
		{{Kind: "task", Team: "review", Output: "WORKER_RECEIPT"}},
		{{Kind: "run_response", Team: "review", Summary: "REASON_IS_NOT_RESPONSE"}},
	} {
		deps := resultsTestDeps(t)
		deps.load = func(_ context.Context, scope inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error) {
			return inspectpkg.HistoricalSnapshot{Coverage: inspectpkg.HistoricalCoverage{Scope: scope}, Results: rows}, nil
		}
		for _, format := range []string{"text", "json"} {
			_, output, err := executeResultsTest(t, deps, "--team", "review", "--workspace", workspace, "--output", format)
			if err == nil || output != "" {
				t.Fatalf("missing final response printed a fallback in %s: %q, %v", format, output, err)
			}
		}
	}
	for _, flags := range [][]string{{"--agent", "helper"}, {"--status", "done"}, {"--cursor", "cursor"}} {
		if _, _, err := executeResultsTest(t, resultsTestDeps(t), append([]string{"--workspace", workspace}, flags...)...); err == nil {
			t.Fatalf("default accepted an irrelevant worker/pagination flag: %v", flags)
		}
	}
}
