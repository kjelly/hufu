package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func TestExplainCommandAIUsesPromptAndCommandLineModel(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	previous := explainAIAnalyze
	t.Cleanup(func() { explainAIAnalyze = previous })
	var capturedPrompt, capturedModel string
	explainAIAnalyze = func(_ context.Context, _ *operatorpkg.OperatorSnapshot, requestedModel, prompt string) (explainAIResult, error) {
		capturedPrompt, capturedModel = prompt, requestedModel
		return explainAIResult{model: requestedModel, analysis: "已驗證：任務未完全完成。"}, nil
	}
	command := newExplainCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "--run", runID, "--ai", "為什麼任務失敗", "--model", "remote/analyzer"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if capturedModel != "remote/analyzer" || !strings.Contains(capturedPrompt, "為什麼任務失敗") {
		t.Fatalf("AI request model=%q prompt=%q", capturedModel, capturedPrompt)
	}
	for _, expected := range []string{"Hufu progress", "AI analysis (remote/analyzer; inference):", "已驗證"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("AI output missing %q:\n%s", expected, stdout.String())
		}
	}
}

func TestExplainCommandAIJSONIsSingleStableDocument(t *testing.T) {
	workspace, _, _ := buildInspectCommandFixture(t)
	previous := explainAIAnalyze
	t.Cleanup(func() { explainAIAnalyze = previous })
	explainAIAnalyze = func(_ context.Context, _ *operatorpkg.OperatorSnapshot, requestedModel, _ string) (explainAIResult, error) {
		return explainAIResult{model: requestedModel, analysis: "analysis"}, nil
	}
	command := newExplainCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "--ai", "--model", "local/test", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope explainAIEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode AI JSON: %v\n%s", err, stdout.String())
	}
	if envelope.Kind != "ai_explanation" || envelope.Data.Operation.Status != "succeeded" || envelope.Data.Model != "local/test" || !envelope.Data.AnalysisIsInference || envelope.Data.Overview == nil {
		t.Fatalf("AI envelope = %#v", envelope)
	}
}

func TestExplainCommandRejectsAIOnlyArgumentsBeforeWorkspaceLookup(t *testing.T) {
	for name, args := range map[string][]string{
		"prompt": {"why"},
		"model":  {"--model", "local/test"},
	} {
		t.Run(name, func(t *testing.T) {
			stateRoot := filepath.Join(t.TempDir(), "state")
			t.Setenv("HUFU_STATE_HOME", stateRoot)
			command := newExplainCommand()
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs(args)
			if err := command.Execute(); err == nil {
				t.Fatal("invalid AI-only argument unexpectedly succeeded")
			}
			if _, err := os.Stat(stateRoot); !os.IsNotExist(err) {
				t.Fatalf("invalid arguments triggered workspace lookup: %v", err)
			}
		})
	}
}

func TestResolveExplainAIModelPrefersCommandLineThenPersistedTarget(t *testing.T) {
	snapshot := &operatorpkg.OperatorSnapshot{RoleTargets: []operatorpkg.RoleTargetView{{Role: "worker", Effective: "local/persisted", Availability: "verified"}}}
	session := &team.TeamSession{Config: agent.TeamConfig{SidecarModel: "local/team-sidecar", Generation: agent.GenerationParams{Model: "local/team"}}}
	cfg := &config.Config{SidecarModel: "local/global-sidecar", Model: "local/global"}
	if got := resolveExplainAIModel("local/cli", snapshot, session, cfg); got != "local/cli" {
		t.Fatalf("CLI model precedence = %q", got)
	}
	session.Config.SidecarModel = ""
	if got := resolveExplainAIModel("", snapshot, session, cfg); got != "local/persisted" {
		t.Fatalf("persisted model precedence = %q", got)
	}
}

func TestSafeAIAnalysisPreservesLinesAndStripsTerminalControls(t *testing.T) {
	got := safeAIAnalysis("first\n\x1b]8;;https://evil.example\x07second\x1b]8;;\x07")
	if got != "first\nsecond" {
		t.Fatalf("safe AI analysis = %q", got)
	}
}

func TestCompactExplainAITaskDoesNotPromotePriorDiagnosticToCancellationCause(t *testing.T) {
	task := inspectpkg.TaskData{
		TaskID: "18", Status: string(team.TaskError), FailureClass: string(team.FailureCancelled),
		Failure: &team.FailureEventPayload{
			TaskID: "18", FailureClass: team.FailureCancelled, RetryDisposition: team.RetryNone,
			Summary: "source=sigint | error=context canceled", Command: "grep outside-allowed-path",
			ExitCode: new(1), Stderr: "path consent could not be granted",
		},
	}

	compact := compactExplainAITask(task)
	if compact.Failure == nil || compact.Failure.Terminal.Summary != "source=sigint | error=context canceled" {
		t.Fatalf("terminal cancellation evidence = %#v", compact.Failure)
	}
	if compact.Failure.SupportingDiagnostic == nil || compact.Failure.SupportingDiagnostic.Command != "grep outside-allowed-path" {
		t.Fatalf("supporting diagnostic = %#v", compact.Failure)
	}
	if compact.Failure.DiagnosticRelationship != "unlinked_prior_diagnostic_not_terminal_cause" {
		t.Fatalf("diagnostic relationship = %q", compact.Failure.DiagnosticRelationship)
	}
}

func TestExplainAICausalityRequiresPersistedCancellationTrigger(t *testing.T) {
	timeline := []explainAITimelineEntry{
		{EventOrdinal: 10, Kind: string(team.EventWrapUpPhase)},
		{EventOrdinal: 11, Kind: string(team.EventTaskCancelled), TaskID: "18"},
		{EventOrdinal: 12, Kind: string(team.EventRunFinished), Status: "cancelled"},
	}
	unknown := explainAICausalityFrom(inspectpkg.RunData{Outcome: string(team.RunOutcomeCancelled)}, timeline)
	if unknown.State != explainAICauseUnknown || unknown.RootCause != "" || !strings.Contains(unknown.Summary, "temporal order alone") {
		t.Fatalf("unrecorded cancellation causality = %#v", unknown)
	}

	withCause := append([]explainAITimelineEntry{}, timeline[:1]...)
	withCause = append(withCause, explainAITimelineEntry{
		EventOrdinal: 11, Kind: string(team.EventRunCancellationRequested),
		ReasonCode: string(team.RunCancellationGracefulTimeout),
	})
	withCause = append(withCause, timeline[1:]...)
	verified := explainAICausalityFrom(inspectpkg.RunData{Outcome: string(team.RunOutcomeCancelled)}, withCause)
	if verified.State != explainAICauseVerified || verified.RootCause != string(team.RunCancellationGracefulTimeout) || len(verified.EvidenceEventOrdinals) != 1 || verified.EvidenceEventOrdinals[0] != 11 {
		t.Fatalf("persisted cancellation causality = %#v", verified)
	}
	multipleCauses := append([]explainAITimelineEntry{}, withCause[:2]...)
	multipleCauses = append(multipleCauses, explainAITimelineEntry{
		EventOrdinal: 12, Kind: string(team.EventRunCancellationRequested),
		ReasonCode: string(team.RunCancellationOperatorForce),
	})
	multipleCauses = append(multipleCauses, explainAITimelineEntry{EventOrdinal: 13, Kind: string(team.EventRunFinished), Status: "cancelled"})
	firstCause := explainAICausalityFrom(inspectpkg.RunData{Outcome: string(team.RunOutcomeCancelled)}, multipleCauses)
	if firstCause.RootCause != string(team.RunCancellationGracefulTimeout) || firstCause.EvidenceEventOrdinals[0] != 11 {
		t.Fatalf("later cancellation request replaced the trigger = %#v", firstCause)
	}

	lateCause := append([]explainAITimelineEntry{}, timeline...)
	lateCause = append(lateCause, explainAITimelineEntry{
		EventOrdinal: 13, Kind: string(team.EventRunCancellationRequested),
		ReasonCode: string(team.RunCancellationGracefulTimeout),
	})
	late := explainAICausalityFrom(inspectpkg.RunData{Outcome: string(team.RunOutcomeCancelled)}, lateCause)
	if late.State != explainAICauseUnknown || late.RootCause != "" {
		t.Fatalf("post-terminal cancellation event became a root cause = %#v", late)
	}
	nonCancelled := explainAICausalityFrom(inspectpkg.RunData{Outcome: string(team.RunOutcomeCompleted)}, withCause)
	if nonCancelled.State != "not_applicable" || nonCancelled.RootCause != "" {
		t.Fatalf("non-cancelled run acquired cancellation root cause = %#v", nonCancelled)
	}
}

func TestCompactExplainAITimelineUsesOnlyBoundedDurableCausalEvents(t *testing.T) {
	entries := []inspectpkg.TraceEntry{
		{Ref: inspectpkg.TraceRef{Source: "event_store", EventOrdinal: 1}, Kind: string(team.EventWrapUpPhase), Timestamp: "2026-09-18T12:41:42Z"},
		{Ref: inspectpkg.TraceRef{Source: "execution_receipt", EventOrdinal: 2}, Kind: "execution_receipt", Status: "failed"},
		{Ref: inspectpkg.TraceRef{Source: "event_store", EventOrdinal: 3, TaskID: "18"}, Kind: string(team.EventTaskCancelled), Status: "error"},
	}
	timeline := compactExplainAITimeline(inspectpkg.TraceData{Entries: entries})
	if len(timeline) != 2 || timeline[0].Kind != string(team.EventWrapUpPhase) || timeline[1].TaskID != "18" {
		t.Fatalf("causal timeline = %#v", timeline)
	}
}

func TestExplainAIPromptForbidsUnlinkedDiagnosticRootCause(t *testing.T) {
	prompt, err := buildExplainAIPrompt("為什麼失敗", explainAIPromptEvidence{
		Causality: explainAICausality{State: explainAICauseUnknown, Summary: "trigger unavailable"},
		Tasks:     []explainAITask{}, Timeline: []explainAITimelineEntry{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range []string{
		"causality.state=verified as the only authoritative runtime cancellation trigger",
		"Temporal proximity or ordering does not prove causation",
		"unlinked_prior_diagnostic_not_terminal_cause",
	} {
		if !strings.Contains(prompt, contract) {
			t.Fatalf("AI prompt omitted causal contract %q:\n%s", contract, prompt)
		}
	}
}

func TestExplainAIPromptCompactsStructurallyWithoutTruncatingJSON(t *testing.T) {
	blockers := make([]operatorpkg.DiagnosticView, 80)
	for index := range blockers {
		blockers[index] = operatorpkg.DiagnosticView{
			Code: "diagnostic", Severity: "warning", Message: strings.Repeat("evidence-", 200), Ref: fmt.Sprintf("ref-%d", index),
		}
	}
	evidence := explainAIPromptEvidence{
		Blockers: blockers,
		Causality: explainAICausality{
			State: explainAICauseVerified, RootCause: "budget_exceeded",
			EvidenceEventOrdinals: []int64{42}, Summary: "verified cause",
		},
		Evidence: &inspectpkg.EvidenceData{Verification: inspectpkg.EvidenceVerificationData{Verdict: "fail", Provenance: "pass"}},
		Tasks:    []explainAITask{}, Timeline: []explainAITimelineEntry{},
	}
	prompt, err := buildExplainAIPrompt("為什麼失敗", evidence)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(prompt)) > sidecar.CompactorProfile.MaxInputRunes {
		t.Fatalf("prompt runes = %d, limit = %d", len([]rune(prompt)), sidecar.CompactorProfile.MaxInputRunes)
	}
	const marker = "EVIDENCE_JSON:\n"
	_, encoded, ok := strings.Cut(prompt, marker)
	if !ok {
		t.Fatalf("prompt omitted evidence marker:\n%s", prompt)
	}
	var decoded explainAIPromptEvidence
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("bounded evidence is not valid JSON: %v\n%s", err, encoded)
	}
	if decoded.Causality.RootCause != "budget_exceeded" || decoded.Evidence == nil || decoded.Evidence.Verification.Verdict != "fail" {
		t.Fatalf("compaction lost causality or verification: %#v", decoded)
	}
	if decoded.Omitted["blockers"] != 68 || len(decoded.Blockers) != 12 {
		t.Fatalf("omission metadata/blockers = %#v/%d", decoded.Omitted, len(decoded.Blockers))
	}
}

func TestExplainCommandTextSummarizesCanonicalOverview(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	command := newExplainCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "--run", runID})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Hufu progress",
		"Workspace: " + workspace,
		"Run: " + runID,
		"Invocation: inv-cli-inspect",
		"State: FINISHED",
		"Tasks: 1 done · 0 active · 0 waiting · 0 need attention · 0 skipped (1 total)",
		"What: run outcome is partial; activity is finished",
		"Next:",
		"Evidence: durable through event",
		"Recent changes:",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("explain output missing %q:\n%s", expected, stdout.String())
		}
	}
}

func TestExplainCommandJSONPreservesOverviewEnvelope(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	command := newExplainCommand()
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--workspace", workspace, "--run", runID, "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Kind inspectpkg.Kind         `json:"kind"`
		Data inspectpkg.OverviewData `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode explain JSON: %v\n%s", err, stdout.String())
	}
	if envelope.Kind != inspectpkg.KindOverview || envelope.Data.Operation.Status != "succeeded" || envelope.Data.Snapshot == nil {
		t.Fatalf("explain envelope = %#v", envelope)
	}
	if envelope.Data.Snapshot.Scope.RunID != runID {
		t.Fatalf("explain run = %q, want %q", envelope.Data.Snapshot.Scope.RunID, runID)
	}
	if stderr.Len() != 0 {
		t.Fatalf("explain JSON wrote stderr: %q", stderr.String())
	}
}

func TestExplainAliasIsRegisteredAtRoot(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"explan"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Name() != "explain" {
		t.Fatalf("explan resolved to %q", command.CommandPath())
	}
}

func TestExplainQueryAutoSelectsActiveManagedWorkspace(t *testing.T) {
	previousOpts := opts
	t.Cleanup(func() { opts = previousOpts })
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "state")
	t.Setenv("HUFU_STATE_HOME", stateRoot)
	t.Chdir(project)
	manager, err := workspacepkg.NewManager(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	want, err := manager.Resolve(t.Context(), workspacepkg.ResolveRequest{
		StartDir: project, TeamName: "hufu-code-review", Mode: workspacepkg.ResolveEnsure,
	})
	if err != nil {
		t.Fatal(err)
	}
	opts = runOptions{}
	command := newExplainCommand()
	command.SetContext(t.Context())
	query, err := explainQuery(command, new(explainCLIOptions))
	if err != nil {
		t.Fatal(err)
	}
	if query.Workspace != want.ControlRoot {
		t.Fatalf("explain workspace = %q, want %q", query.Workspace, want.ControlRoot)
	}
}

func TestExplainMissingWorkspaceDoesNotCreateState(t *testing.T) {
	previousOpts := opts
	t.Cleanup(func() { opts = previousOpts })
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "missing-state")
	t.Setenv("HUFU_STATE_HOME", stateRoot)
	t.Chdir(project)
	opts = runOptions{}
	command := newExplainCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); err == nil {
		t.Fatal("explain unexpectedly succeeded without a managed workspace")
	}
	if _, err := os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("explain created missing state root: %v", err)
	}
}

func TestRenderExplainTextStripsPersistedTerminalControls(t *testing.T) {
	snapshot := &operatorpkg.OperatorSnapshot{
		Scope:         operatorpkg.ResolvedScope{WorkspaceExact: "/work/\x1b]8;;https://evil.example\x07link\x1b]8;;\x07", RunID: "run\nnext", BranchID: "main"},
		Activity:      operatorpkg.ActivityView{State: operatorpkg.ActivityExecuting, RawTaskStates: []string{"in_progress"}},
		Integrity:     operatorpkg.IntegrityView{Status: "valid"},
		Freshness:     operatorpkg.FreshnessView{EventID: "event", LiveState: "not_applicable"},
		LatestChanges: []operatorpkg.ChangeView{{EventOrdinal: 1, Kind: "task_started\x1b[2J", Refs: []string{"ref\x07"}}},
	}
	var output bytes.Buffer
	if err := renderExplainText(&output, snapshot); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(output.String(), "\x1b\x07") || strings.Contains(output.String(), "https://evil.example") {
		t.Fatalf("explain emitted terminal controls: %q", output.String())
	}
}
