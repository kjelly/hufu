package team

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

// TestReadOnlyGateAdmitsStderrMergeAndExplainsDenials pins the gate side of
// the read-only grammar: merged stderr and filtering pipelines reach the tool,
// and a rejected command names the construct instead of claiming that bash
// itself is forbidden.
func TestReadOnlyGateAdmitsStderrMergeAndExplainsDenials(t *testing.T) {
	c := newDirectTypedCoordinator(t, "bash", nil, nil)
	cases := []struct {
		name       string
		command    string
		wantRan    bool
		wantDetail string
	}{
		{name: "merged stderr with a filter", command: "go test -v ./internal/tools/ 2>&1 | grep -c '^--- PASS'", wantRan: true},
		{name: "sequenced inspection", command: "cd internal/tools; go vet ./...", wantRan: true},
		{name: "tee writes a file", command: "go test ./... 2>&1 | tee out.txt", wantDetail: `command "tee"`},
		{name: "stdout redirect", command: "go test ./... > out.txt", wantDetail: "output redirection (>)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := &offloadProbeTool{name: "bash"}
			var dispositions []tools.ToolExecutionDisposition
			ctx := gatewayTestContext(t, c, "bash")
			ctx = context.WithValue(ctx, tools.AgentReadOnlyExecutionKey, true)
			ctx = context.WithValue(ctx, tools.ToolExecutionDispositionReporterKey, tools.ToolExecutionDispositionReporter(func(d tools.ToolExecutionDisposition) {
				dispositions = append(dispositions, d)
			}))
			input := `{"command":` + strconv.Quote(tc.command) + `}`
			response, err := (&policyGatedTool{inner: probe, coordinator: c}).Run(ctx, fantasy.ToolCall{ID: "call", Name: "bash", Input: input})
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRan {
				if probe.ran != 1 || response.IsError {
					t.Fatalf("command was not admitted: %q", response.Content)
				}
				return
			}
			if probe.ran != 0 || !response.IsError || !strings.Contains(response.Content, tc.wantDetail) || !strings.Contains(response.Content, "Merge stderr with 2>&1") {
				t.Fatalf("denial = %q, want it to name %q", response.Content, tc.wantDetail)
			}
			if len(dispositions) != 1 || dispositions[0].ReasonCode != "read_only_tool_denied" || dispositions[0].Executed {
				t.Fatalf("dispositions = %#v", dispositions)
			}
		})
	}
}
