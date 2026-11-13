package main

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

func TestStructuredStepTextJSONAndReportPreserveExecutionMeaning(t *testing.T) {
	steps := []team.StructuredStepMetadata{
		{ReceiptID: "tool", TaskID: "task", StepID: "read", Attempt: 1, ExecutionKind: "tool_call", ProposedModel: "proposal", ExitCode: new(0), Invocations: []team.StepModelExecution{}},
		{ReceiptID: "generation", TaskID: "task", StepID: "produce", Attempt: 1, ExecutionKind: "model_generation", ProposedModel: "proposal", ExitCode: new(0), ModelExecutionIDs: []string{"invocation"},
			Invocations: []team.StepModelExecution{{ID: "invocation", Model: "actual", Provider: "provider", ExecutionTarget: "provider/actual"}}},
		{ReceiptID: "legacy", TaskID: "old-task", ExecutionKind: "unknown", UnavailableReason: "historical_attribution_unavailable", Invocations: []team.StepModelExecution{}},
	}
	var text bytes.Buffer
	if err := renderStructuredStepsText(&text, steps); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"execution_kind=tool_call", "execution_kind=model_generation", "execution_kind=unknown", "proposed_model=proposal", "model=actual", "execution_target=provider/actual", "historical_attribution_unavailable"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("text omitted %q: %s", want, text.String())
		}
	}
	encoded, err := json.Marshal(inspectpkg.TaskData{StructuredSteps: steps})
	if err != nil {
		t.Fatal(err)
	}
	var decoded inspectpkg.TaskData
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.StructuredSteps[0].ExecutionKind != "tool_call" || decoded.StructuredSteps[1].Invocations[0].Model != "actual" || decoded.StructuredSteps[2].ExitCode != nil {
		t.Fatalf("JSON changed attribution: %s", encoded)
	}
	var report strings.Builder
	writeStructuredStepReport(&report, steps)
	for _, want := range []string{"| tool_call | proposal | none for direct step |", "| model_generation | proposal | invocation: provider / actual; target=provider/actual |", "| unknown | unavailable | unavailable | historical_attribution_unavailable |"} {
		if !strings.Contains(report.String(), want) {
			t.Fatalf("report omitted %q: %s", want, report.String())
		}
	}
}
