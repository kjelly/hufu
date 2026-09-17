package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 5 (spec.md Specification 05 / Specification 03 §7): `hufu team
// explain`.

func writeExplainAgentFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runTeamExplainCaptured runs runTeamExplain, capturing stdout via the
// shared captureStdout helper (auditcmd_test.go) and returning both the
// output and the error runTeamExplain returned.
func runTeamExplainCaptured(t *testing.T, args []string) (string, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() { runErr = runTeamExplain(nil, args) })
	return out, runErr
}

func TestRunTeamExplain_TextShowsProvenanceForToolsModelAndTimeout(t *testing.T) {
	dir := t.TempDir()
	writeExplainAgentFile(t, dir, "team.yaml", "model: local-model\n")
	writeExplainAgentFile(t, dir, "developer.md", "---\npreset: coding\n---\nImplement the change.\n")

	teamExplainFormat = "text"
	out, err := runTeamExplainCaptured(t, []string{dir})
	if err != nil {
		t.Fatalf("runTeamExplain() error = %v", err)
	}

	for _, want := range []string{
		"model: local-model", "source: team (team.yaml)",
		"timeout: 600", "source: builtin (built-in default)",
		"developer", "name: developer", "source: filename (developer.md)",
		"source: preset (preset:coding)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("explain text output missing %q; got:\n%s", want, out)
		}
	}
}

func TestRunTeamExplain_JSONIsWellFormed(t *testing.T) {
	dir := t.TempDir()
	writeExplainAgentFile(t, dir, "developer.md", "---\nrole: worker\n---\nImplement the change.\n")

	teamExplainFormat = "json"
	t.Cleanup(func() { teamExplainFormat = "text" })
	out, err := runTeamExplainCaptured(t, []string{dir})
	if err != nil {
		t.Fatalf("runTeamExplain() error = %v", err)
	}
	if !strings.Contains(out, `"source": "filename"`) {
		t.Errorf("json output missing filename-sourced name; got:\n%s", out)
	}
	if !strings.Contains(out, `"key": "developer"`) {
		t.Errorf("json output missing developer agent entry; got:\n%s", out)
	}
	var projection explainOutput
	if err := json.Unmarshal([]byte(out), &projection); err != nil {
		t.Fatal(err)
	}
	if projection.Decision.SchemaVersion != 1 || projection.Decision.Kind != "decision_profile" || projection.Decision.Enabled {
		t.Fatalf("disabled decision projection = %#v", projection.Decision)
	}
	if projection.Request.Enabled || projection.Routing.HintCount != 0 {
		t.Fatalf("disabled request/routing projection = %#v / %#v", projection.Request, projection.Routing)
	}
}

func TestRunTeamExplain_ExactBuiltinUsesStableDecisionProjection(t *testing.T) {
	dir := t.TempDir()
	manifest := "request:\n  objective: keep service safe\n  success-criteria:\n    - id: safe\n      statement: service remains safe\ndecision:\n  profile: builtin/standard@v1\n  routing:\n    hints:\n      - when-goal-contains: storage\n        preferred-capabilities: [architecture]\n"
	writeExplainAgentFile(t, dir, "team.yaml", manifest)

	teamExplainFormat = "json"
	t.Cleanup(func() { teamExplainFormat = "text" })
	out, err := runTeamExplainCaptured(t, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	var projection explainOutput
	if err := json.Unmarshal([]byte(out), &projection); err != nil {
		t.Fatal(err)
	}
	if !projection.Decision.Enabled || projection.Decision.Identity.ResolvedRef != "builtin/standard@v1" || projection.Decision.Plan == nil {
		t.Fatalf("decision projection = %#v", projection.Decision)
	}
	if projection.Request.Source != "request" || !projection.Request.Enabled {
		t.Fatalf("request projection = %#v", projection.Request)
	}
	if projection.Routing.Source != "decision.routing.hints" || projection.Routing.HintCount != 1 {
		t.Fatalf("routing projection = %#v", projection.Routing)
	}
}

func TestRunTeamExplain_RedactsDecisionAuthoringProjection(t *testing.T) {
	dir := t.TempDir()
	secret := "SuperSecretToken-987654321"
	manifest := "request:\n  objective: api_token=" + secret + "\n  success-criteria:\n    - id: safe\n      statement: keep service safe\ndecision:\n  profile: standard\n"
	writeExplainAgentFile(t, dir, "team.yaml", manifest)

	for _, format := range []string{"json", "yaml", "text"} {
		t.Run(format, func(t *testing.T) {
			teamExplainFormat = format
			t.Cleanup(func() { teamExplainFormat = "text" })
			out, err := runTeamExplainCaptured(t, []string{dir})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, secret) {
				t.Fatalf("%s output exposed recognizable secret: %s", format, out)
			}
			if format == "json" && !json.Valid([]byte(out)) {
				t.Fatalf("redacted JSON is invalid: %s", out)
			}
		})
	}
}

func TestRunTeamExplain_DedupesAgentWithDifferentExplicitName(t *testing.T) {
	dir := t.TempDir()
	writeExplainAgentFile(t, dir, "developer.md", "---\nname: coder\nrole: worker\n---\nImplement the change.\n")

	teamExplainFormat = "text"
	out, err := runTeamExplainCaptured(t, []string{dir})
	if err != nil {
		t.Fatalf("runTeamExplain() error = %v", err)
	}
	if strings.Count(out, "name: coder") != 1 {
		t.Errorf("expected agent %q to appear exactly once despite dual file-alias/name keys; got:\n%s", "coder", out)
	}
}

func TestRunTeamExplain_UnknownFormatFails(t *testing.T) {
	dir := t.TempDir()
	writeExplainAgentFile(t, dir, "developer.md", "---\nrole: worker\n---\nImplement the change.\n")

	teamExplainFormat = "xml"
	t.Cleanup(func() { teamExplainFormat = "text" })
	if _, err := runTeamExplainCaptured(t, []string{dir}); err == nil {
		t.Fatal("runTeamExplain() error = nil, want an error for an unknown --format")
	}
}

func TestRunTeamExplain_PerformsNoModelCall(t *testing.T) {
	// CompileTeam wraps LoadTeam, which never calls a model; a bad preset
	// name should fail compilation before anything else runs.
	dir := t.TempDir()
	writeExplainAgentFile(t, dir, "developer.md", "---\npreset: not-a-real-preset\n---\nImplement the change.\n")

	teamExplainFormat = "text"
	if _, err := runTeamExplainCaptured(t, []string{dir}); err == nil {
		t.Fatal("runTeamExplain() error = nil, want unknown-preset compile failure")
	}
}
