package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func TestInspectRoutesCLITextAndJSONAgree(t *testing.T) {
	workspace, runID, taskID := buildInspectCommandFixture(t)
	run := func(format string) string {
		t.Helper()
		command := newInspectCommand()
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"routes", runID, "--workspace", workspace, "--task", taskID, "--output", format})
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), "private worker output") || strings.Contains(output.String(), "sk-fixture-secret") {
			t.Fatal("route output leaked content")
		}
		return output.String()
	}
	var envelope struct {
		Kind inspectpkg.Kind       `json:"kind"`
		Data inspectpkg.RoutesData `json:"data"`
	}
	if err := json.Unmarshal([]byte(run("json")), &envelope); err != nil {
		t.Fatal(err)
	}
	text := run("text")
	want := fmt.Sprintf("Tasks: %d total, %d verified, %d not verified, %d unavailable, %d reused",
		envelope.Data.Coverage.Tasks, envelope.Data.Coverage.Verified, envelope.Data.Coverage.NotVerified,
		envelope.Data.Coverage.Unavailable, envelope.Data.Coverage.Reused)
	if envelope.Kind != inspectpkg.KindRoutes || !strings.Contains(text, want) || strings.Contains(text, "\x1b") {
		t.Fatalf("JSON/text disagree: %s", text)
	}
}
