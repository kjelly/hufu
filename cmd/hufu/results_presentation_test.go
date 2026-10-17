package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

func TestResultsSourceIdentifiesSavedRunWithoutChangingStdout(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	for _, operation := range []string{"", "final"} {
		for _, test := range []struct {
			flags    []string
			metadata bool
		}{{nil, true}, {[]string{"--quiet"}, false}, {[]string{"--output", "json"}, false}} {
			command := newResultsCommandWithDeps(resultsTestDeps(t))
			var stdout, stderr bytes.Buffer
			command.SetOut(&stdout)
			command.SetErr(&stderr)
			args := []string{"--team", "review", "--workspace", workspace}
			if operation != "" {
				args = append([]string{operation}, args...)
			}
			command.SetArgs(append(args, test.flags...))
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			if test.metadata {
				for _, want := range []string{"Original request: 請調查原始連線逾時問題", "team=review", "completed=2026-10-13T10:00:00Z", "Invocation: inv-results", "Run: run-review", "source event:"} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("missing %q: %s", want, stderr.String())
					}
				}
			} else if stderr.Len() != 0 {
				t.Fatalf("machine/quiet metadata: %s", stderr.String())
			}
			if len(test.flags) == 0 && stdout.String() != "coordinator final response\n" {
				t.Fatalf("stdout changed: %q", stdout.String())
			}
		}
	}
}

func TestResultsSourcePreservesOriginalRequestAndHandlesMissingHistory(t *testing.T) {
	for _, prompt := range []string{"", strings.Repeat("原", 350) + "\n第二行 END_OF_REQUEST", "安全\x1b]52;c;UNSAFE_CLIPBOARD\a內容"} {
		var stderr bytes.Buffer
		row := inspectpkg.HistoricalResult{OriginalPrompt: prompt}
		if err := writeResultsSource(&stderr, resultsEnvelope{Results: []inspectpkg.HistoricalResult{row}}, true); err != nil {
			t.Fatal(err)
		}
		want := "Original request: " + prompt
		if prompt == "" {
			want = "Original request: (Not saved in this run's canonical history.)"
		} else if strings.ContainsRune(prompt, '\x1b') {
			want = "Original request: 安全內容"
		}
		if !strings.Contains(stderr.String(), want) || strings.Contains(stderr.String(), "UNSAFE_CLIPBOARD") || strings.ContainsRune(stderr.String(), '\x1b') {
			t.Fatalf("original request was truncated, invented or unsafe: %q", stderr.String())
		}
	}
}

func TestResultsFullPayloadPreservesLargeJSONAndScope(t *testing.T) {
	workspace, ids := resultsTestFixture(t)
	// Preserve a number beyond float64's exact integer range and JSON escapes.
	raw := []byte(`{"number":9007199254740993,"text":"` + strings.Repeat("界", 7000) + `","control":"\u001b[31m","path":"/must/not/open"}`)
	deps := resultsTestDeps(t)
	deps.load = func(_ context.Context, scope inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error) {
		return inspectpkg.HistoricalSnapshot{Coverage: inspectpkg.HistoricalCoverage{Scope: scope}, Results: []inspectpkg.HistoricalResult{{
			ResultID: ids[0], Team: "review", Kind: "task", CompletedAt: time.Unix(1, 0),
			TypedResult: &team.TaskResult{StructuredPayload: &team.ResultPayload{Value: raw, SHA256: "original-sha"}},
		}}}, nil
	}
	command := newResultsCommandWithDeps(deps)
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"show", ids[0], "--payload", "--team", "review", "--workspace", workspace})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	got, err := jsontext.AppendFormat(nil, stdout.Bytes())
	want, wantErr := jsontext.AppendFormat(nil, raw)
	if err != nil || wantErr != nil || !bytes.Equal(got, want) || stderr.Len() != 0 {
		t.Fatalf("payload changed or metadata leaked: %v, %v", err, wantErr)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("9007199254740993")) || bytes.Contains(stdout.Bytes(), []byte{27}) {
		t.Fatal("number precision or JSON control escaping changed")
	}
	if _, _, err := executeResultsTest(t, deps, "show", ids[0], "--payload", "--team", "other", "--workspace", workspace); err == nil {
		t.Fatal("payload bypassed team filter")
	}
	if _, _, err := executeResultsTest(t, deps, "show", ids[0], "--payload", "--since", "2027-01-01T00:00:00Z", "--workspace", workspace); err == nil {
		t.Fatal("payload bypassed date filter")
	}
	if !bytes.Equal(raw, depsPayloadBytes(t, deps, workspace)) {
		t.Fatal("payload projection was mutated")
	}
}

func depsPayloadBytes(t *testing.T, deps resultsDeps, workspace string) []byte {
	t.Helper()
	snapshot, err := deps.load(t.Context(), inspectpkg.HistoricalResultScope{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.Results[0].TypedResult.StructuredPayload.Value
}

func TestResultsPayloadMissingInvalidAndConflictingModesFailBeforeOutput(t *testing.T) {
	workspace, ids := resultsTestFixture(t)
	for _, flags := range [][]string{{"--payload"}, {"--payload", "--output", "json"}, {"--payload", "--render", "markdown"}, {"--render", "bad"}, {"--theme", "bad"}, {"--display-mode", "bad"}} {
		_, output, err := executeResultsTest(t, resultsTestDeps(t), append([]string{"show", ids[0], "--workspace", workspace}, flags...)...)
		if err == nil || output != "" {
			t.Fatalf("invalid mode emitted content: %v %q %v", flags, output, err)
		}
	}
	for _, raw := range []string{"", "null", "{invalid}"} {
		var stdout bytes.Buffer
		err := writeResultsPayload(&stdout, resultsEnvelope{Results: []inspectpkg.HistoricalResult{{TypedResult: &team.TaskResult{StructuredPayload: &team.ResultPayload{Value: []byte(raw)}}}}})
		if err == nil || stdout.Len() != 0 {
			t.Fatal("invalid payload emitted content")
		}
	}
}

func TestResultsDeliverablePreservesDetailsAndHumanFields(t *testing.T) {
	result := &team.TaskResult{Summary: "summary", Details: "```go\n\t    a  b  \n```\n\x1b]52;c;evil\a",
		Findings:  []team.Finding{{Severity: "warning", Category: "correctness", Summary: "finding", Detail: "impact"}},
		Decisions: []team.Decision{{Topic: "topic", Choice: "choice", Reason: "because"}},
		Artifacts: []team.ArtifactRef{{ID: "opaque-id", Path: "/must/not/read", Description: "reference"}},
		Risks:     []team.Risk{{Description: "risk", Impact: "failure", Mitigation: "check"}}, OpenQuestions: team.OpenQuestions{"unknown"},
		StructuredPayload: &team.ResultPayload{Value: []byte(strings.Repeat("x", 17000)), SHA256: "sha"},
	}
	got := formatHistoricalDeliverable(inspectpkg.HistoricalResult{Kind: "task", TypedResult: result})
	for _, want := range []string{"\t    a  b  \n", "[warning/correctness]", "impact", "Reason: because", "opaque-id", "reference", "Mitigation: check", "Open question: unknown", "17000 bytes; use show --payload"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "evil") || strings.Contains(got, strings.Repeat("x", 100)) {
		t.Fatal("control or unbounded payload leaked")
	}
}

func TestResultsMarkdownPolicyAndRenderer(t *testing.T) {
	for _, test := range []struct {
		terminal, noColor bool
		env, mode, preset string
		theme             tuipkg.ThemeMode
		want              bool
	}{
		{true, false, "", "auto", "default", tuipkg.ThemeDark, true},
		{false, false, "", "terminal", "default", tuipkg.ThemeDark, false},
		{true, true, "", "auto", "default", tuipkg.ThemeDark, false},
		{true, false, "1", "auto", "default", tuipkg.ThemeDark, false},
		{true, false, "", "plain", "default", tuipkg.ThemeDark, false},
		{true, false, "", "auto", "epaper", tuipkg.ThemeLight, false},
		{true, false, "", "auto", "default", tuipkg.ThemeMono, false},
	} {
		if got := resultsMarkdownEnabled(test.terminal, test.noColor, test.env, test.mode, test.preset, test.theme); got != test.want {
			t.Fatalf("policy: %+v got %t", test, got)
		}
	}
	for _, theme := range []tuipkg.ThemeMode{tuipkg.ThemeLight, tuipkg.ThemeDark} {
		got, err := formatResultsMarkdown("## 結論\n\n- finding\n\n```go\nvar answer = 42\n```\n\x1b]52;c;evil\a", 60, theme)
		if err != nil || !strings.Contains(got, "\x1b[") || !strings.Contains(got, "finding") || strings.Contains(got, "evil") {
			t.Fatalf("Markdown failed: %q, %v", got, err)
		}
	}
	if _, err := formatResultsMarkdown(strings.Repeat("x", (1<<20)+1), 80, tuipkg.ThemeDark); err == nil {
		t.Fatal("renderer accepted unbounded input")
	}
}

func TestResultsDeliverableRetainsLegacyTextBeyondTypedSummary(t *testing.T) {
	text := "original deliverable\n\t    a  b  \n" + strings.Repeat("full evidence\n", 2000)
	got := formatHistoricalDeliverable(inspectpkg.HistoricalResult{Kind: "task", Output: text,
		TypedResult: &team.TaskResult{Summary: "bounded summary", Source: "promoted_free_text"}})
	if !strings.Contains(got, text) {
		t.Fatal("legacy saved output was replaced by the typed summary")
	}
}
