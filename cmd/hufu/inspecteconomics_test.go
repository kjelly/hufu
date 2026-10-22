package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func TestInspectEconomicsCLITextAndJSONAgree(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	run := func(format string) string {
		t.Helper()
		command := newInspectCommand()
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"economics", runID, "--workspace", workspace, "--output", format})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		for _, content := range []string{"private worker output", "sk-fixture-secret", "\x1b"} {
			if strings.Contains(output.String(), content) {
				t.Fatal("economics output contains content or terminal controls")
			}
		}
		return output.String()
	}
	var envelope struct {
		Kind inspectpkg.Kind          `json:"kind"`
		Data inspectpkg.EconomicsData `json:"data"`
	}
	if err := json.Unmarshal([]byte(run("json")), &envelope); err != nil {
		t.Fatal(err)
	}
	text := run("text")
	if envelope.Kind != inspectpkg.KindEconomics ||
		!strings.Contains(text, fmt.Sprintf("Tasks: %d total, %d verified", envelope.Data.TaskCount, envelope.Data.VerifiedTaskCount)) ||
		!strings.Contains(text, "Cost per verified task (USD micros): unavailable") {
		t.Fatalf("JSON/text disagree: %s", text)
	}
	command := newInspectCommand()
	command.SetArgs([]string{"economics", runID, "--workspace", workspace, "--task", "task"})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); err == nil {
		t.Fatal("economics accepted partial task scope")
	}
}
