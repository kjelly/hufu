package main

import (
	"bytes"
	json "encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestRunDryRunOutput(t *testing.T) {
	binary := buildTestBinary(t)
	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	if err := os.MkdirAll(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	writeHufuYAML(t, project, "unknown-preview-setting: true\nprofiles:\n  events:\n    event-format: jsonl\n  invalid-events:\n    bad-flag: invalid\n    event-format: jsonl\n")
	tests := []struct {
		name          string
		flags         []string
		task          bool
		jsonOutput    bool
		jsonl         bool
		configWarning bool
		errorContains string
	}{
		{name: "text preview", task: true},
		{name: "JSON preview", flags: []string{"--output", "json"}, task: true, jsonOutput: true},
		{name: "JSON and events", flags: []string{"--output", "json", "--event-format", "jsonl", "--report", "--skill", "missing-preview-skill"}, task: true, jsonOutput: true, jsonl: true, configWarning: true},
		{name: "text and events", flags: []string{"--event-format=jsonl"}, task: true, jsonl: true, configWarning: true},
		{name: "missing task", flags: []string{"--output", "json", "--event-format", "jsonl"}, jsonl: true, errorContains: "<task>"},
		{name: "empty task", flags: []string{"--event-format", "jsonl", "--", " "}, jsonl: true, errorContains: "<task>"},
		{name: "missing task with profile", flags: []string{"--profile", "events"}, jsonl: true, configWarning: true, errorContains: "<task>"},
		{name: "invalid JSONL profile", flags: []string{"--profile", "invalid-events"}, task: true, jsonl: true, configWarning: true, errorContains: "bad-flag"},
		{name: "unknown flag before event format", flags: []string{"--unknown-flag", "--event-format", "jsonl"}, task: true, jsonl: true, errorContains: "unknown flag"},
		{name: "invalid flag value before event format", flags: []string{"--timeout=oops", "--event-format", "jsonl"}, task: true, jsonl: true, errorContains: "--timeout"},
		{name: "invalid output", flags: []string{"--output", "xml", "--event-format", "jsonl"}, task: true, jsonl: true, errorContains: "invalid --output"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"run", "--dry-run", "--default", "--model", "ollama/preview-model", "--coordinator-model", "ollama/preview-model"}, test.flags...)
			if test.task {
				args = append(args, "--", "check files")
			}
			command := exec.CommandContext(t.Context(), binary, args...)
			command.Dir = project
			command.Env = append(command.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+dir, "HUFU_STATE_HOME="+state, "NO_COLOR=1")
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			if (err != nil) != (test.errorContains != "") {
				t.Fatalf("exit: %v, stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			if test.jsonl {
				var messages []string
				var configWarning bool
				for line := range bytes.SplitSeq(bytes.TrimSuffix(stderr.Bytes(), []byte("\n")), []byte("\n")) {
					var event jsonStatusEvent
					if err := json.Unmarshal(line, &event); err != nil || event.Type == "" || event.Time == "" {
						t.Fatalf("invalid JSONL event %q: %v", line, err)
					}
					if event.Type == "error" {
						messages = append(messages, event.Message)
					}
					if event.Type == "warning" && strings.Contains(event.Message, "unknown-preview-setting") {
						configWarning = true
					}
				}
				if test.configWarning && !configWarning {
					t.Fatal("configuration warning was not emitted as an event")
				}
				if test.errorContains != "" && (len(messages) != 1 || !strings.Contains(messages[0], test.errorContains)) {
					t.Fatalf("command error events = %q, want one containing %q", messages, test.errorContains)
				}
				if test.errorContains == "" && len(messages) > 0 {
					t.Fatalf("unexpected error events: %q", messages)
				}
			}
			switch {
			case test.errorContains != "":
				if stdout.Len() != 0 {
					t.Fatalf("failure wrote stdout: %q", stdout.String())
				}
			case test.jsonOutput:
				var preview struct {
					SchemaVersion int              `json:"schema_version"`
					DryRun        bool             `json:"dry_run"`
					Team          string           `json:"team"`
					Prompt        string           `json:"prompt"`
					Agents        []map[string]any `json:"agents"`
					RoleTargets   map[string]any   `json:"role_targets"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &preview); err != nil || preview.SchemaVersion != 1 || !preview.DryRun ||
					preview.Team != "default" || preview.Prompt != "check files" || len(preview.Agents) == 0 || len(preview.RoleTargets) == 0 {
					t.Fatalf("invalid preview document: %v, stdout=%q", err, stdout.String())
				}
			default:
				preview := stderr.String()
				if test.jsonl {
					preview = stdout.String()
				} else if stdout.Len() != 0 {
					t.Fatalf("plain dry run wrote stdout: %q", stdout.String())
				}
				if !strings.Contains(preview, "DRY RUN COMPLETE") {
					t.Fatalf("missing text preview: %q", preview)
				}
			}
			if _, err := os.Stat(state); !os.IsNotExist(err) {
				t.Fatalf("preview created managed state: %v", err)
			}
		})
	}
}

func TestCanonicalRunWorkspaceSemantics(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })

	exact := filepath.Join(t.TempDir(), "already-team")
	opts = runOptions{workspace: exact, workspaceMode: "exact"}
	session := &team.TeamSession{}
	if err := resolveTeamWorkspacePath("review", session); err != nil {
		t.Fatal(err)
	}
	if session.Workspace != exact {
		t.Fatalf("exact workspace = %q, want %q", session.Workspace, exact)
	}

	root := t.TempDir()
	opts = runOptions{workspace: root, workspaceMode: "root"}
	session = &team.TeamSession{}
	if err := resolveTeamWorkspacePath("review", session); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "review")
	if session.Workspace != want {
		t.Fatalf("root workspace = %q, want %q", session.Workspace, want)
	}

	project := t.TempDir()
	stateRoot := filepath.Join(t.TempDir(), "state")
	t.Setenv("HUFU_STATE_HOME", stateRoot)
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	opts = runOptions{}
	session = &team.TeamSession{}
	if err := resolveTeamWorkspacePath("review", session); err != nil {
		t.Fatal(err)
	}
	if !session.Scope.Managed || session.Scope.SubjectRoot != project || !strings.HasPrefix(session.Workspace, filepath.Join(stateRoot, "projects")) {
		t.Fatalf("managed default workspace scope = %+v", session.Scope)
	}
	_ = closeSessionWorkspaceLease(session)
}

func TestCanonicalRunResolvesProfileBeforeRuntimeBridge(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, `
profiles:
  safe:
    no-net: "true"
    unattended: "true"
    max-duration: "60"
    max-total-tokens: "1200"
    graceful-wrap-up-timeout: "45m"
    model: "profile-model"
    output: "json"
    workspace: "/profile/workspace"
    event-format: "jsonl"
`)
	defer chdir(t, dir)()
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts.profileName = "safe"

	command, options := newRunCommandWithOptions()
	var diagnostics bytes.Buffer
	command.SetErr(&diagnostics)
	if err := command.ParseFlags([]string{"task"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveCanonicalRunOptions(command, options)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.noNet || !resolved.unattended || resolved.maxDuration != 60 || resolved.maxTotalTokens != 1200 || resolved.gracefulWrapUpTimeout != 45*time.Minute {
		t.Fatalf("profile safety/budget options not bridged: %#v", resolved)
	}
	if resolved.modelOverride != "profile-model" || resolved.outputFormat != "json" || resolved.workspace != "/profile/workspace" || resolved.workspaceMode != "exact" {
		t.Fatalf("profile runtime options not bridged: %#v", resolved)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("JSONL profile emitted non-JSONL diagnostics: %q", diagnostics.String())
	}
}

func TestCanonicalRunProjectDefaultProfilePrecedence(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, `
default-profile: coding
profiles:
  coding:
    team: dev-team
    model: profile-model
    plan: "true"
  review:
    team: review-team
    model: review-model
`)
	defer chdir(t, dir)()
	previous := opts
	t.Cleanup(func() { opts = previous })

	tests := []struct {
		name, team, model string
		args              []string
		wantPlan          bool
	}{
		{name: "project default", team: "dev-team", model: "profile-model", wantPlan: true},
		{name: "explicit flags", args: []string{"--team", "cli-team", "--model", "cli-model"}, team: "cli-team", model: "cli-model", wantPlan: true},
		{name: "legacy selector overrides profile team", args: []string{"--agent-team", "legacy-team"}, team: "legacy-team", model: "profile-model", wantPlan: true},
		{name: "built-in selector overrides profile team", args: []string{"--default"}, model: "profile-model", wantPlan: true},
		{name: "automatic selector overrides profile team", args: []string{"--auto-team"}, model: "profile-model", wantPlan: true},
		{name: "explicit profile", args: []string{"--profile", "review"}, team: "review-team", model: "review-model"},
		{name: "explicit empty profile", args: []string{"--profile="}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts = runOptions{}
			root := &cobra.Command{Use: "hufu"}
			root.PersistentFlags().StringVar(&opts.profileName, "profile", "", "")
			command, options := newRunCommandWithOptions()
			root.AddCommand(command)
			command.SetErr(new(bytes.Buffer))
			if err := command.ParseFlags(append(tt.args, "task")); err != nil {
				t.Fatal(err)
			}
			resolved, err := resolveCanonicalRunOptions(command, options)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.agentTeamName != tt.team || resolved.modelOverride != tt.model || resolved.planMode != tt.wantPlan {
				t.Fatalf("resolved = %#v, want team %q, model %q, plan %v", resolved, tt.team, tt.model, tt.wantPlan)
			}
		})
	}
}

func TestCanonicalRunInvalidProjectDefaultProfileFails(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, "default-profile: missing\nprofiles:\n  coding:\n    team: dev-team\n")
	defer chdir(t, dir)()
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts = runOptions{}
	command, options := newRunCommandWithOptions()
	if err := command.ParseFlags([]string{"task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCanonicalRunOptions(command, options); err == nil || !strings.Contains(err.Error(), "profile \"missing\" not found") {
		t.Fatalf("invalid project default error = %v", err)
	}
}

func TestCanonicalRunExplicitZeroAndFalseOverrideProfile(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, `
profiles:
  safe:
    no-net: "true"
    max-duration: "60"
    model: "profile-model"
    output: "json"
`)
	defer chdir(t, dir)()
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts.profileName = "safe"

	command, options := newRunCommandWithOptions()
	command.SetErr(new(bytes.Buffer))
	if err := command.ParseFlags([]string{"--no-net=false", "--max-duration=0", "--model", "cli-model", "--output", "text", "task"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveCanonicalRunOptions(command, options)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.noNet || resolved.maxDuration != 0 || resolved.modelOverride != "cli-model" || resolved.outputFormat != "text" {
		t.Fatalf("explicit CLI values did not override profile: %#v", resolved)
	}
}

func TestCanonicalRunProfileAliasConflictFailsBeforeWorkspaceMutation(t *testing.T) {
	dir := t.TempDir()
	exact := filepath.Join(dir, "must-not-exist")
	writeHufuYAML(t, dir, "profiles:\n  scoped:\n    workspace: \""+exact+"\"\n")
	defer chdir(t, dir)()
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts.profileName = "scoped"

	command, options := newRunCommandWithOptions()
	command.SetErr(new(bytes.Buffer))
	if err := command.ParseFlags([]string{"--workspace-root", filepath.Join(dir, "root"), "task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCanonicalRunOptions(command, options); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("profile alias conflict error = %v", err)
	}
	if _, err := os.Stat(exact); !os.IsNotExist(err) {
		t.Fatalf("profile conflict mutated workspace %q: %v", exact, err)
	}
}

func TestCanonicalRunFactoryFlagsAreInstanceScoped(t *testing.T) {
	first := newRunCommand()
	second := newRunCommand()
	if err := first.ParseFlags([]string{"--team", "alpha", "--workspace", "/tmp/alpha"}); err != nil {
		t.Fatal(err)
	}
	if second.Flags().Lookup("team").Value.String() != "" || second.Flags().Lookup("workspace").Value.String() != "" {
		t.Fatal("new run command inherited values from another factory instance")
	}
}

func TestCanonicalRunExposesPresentationFlags(t *testing.T) {
	command := newRunCommand()
	for _, name := range []string{"theme", "display-preset", "display-mode", "no-spinner", "no-summary", "tui-compact", "route"} {
		if command.Flags().Lookup(name) == nil {
			t.Errorf("canonical run missing --%s", name)
		}
	}
}

func TestCanonicalRunAcceptsHyphenLeadingPromptAfterSeparator(t *testing.T) {
	command := newRunCommand()
	if err := command.ParseFlags([]string{"--team", "alpha", "--", "-audit the result"}); err != nil {
		t.Fatal(err)
	}
	args := command.Flags().Args()
	if len(args) != 1 || args[0] != "-audit the result" {
		t.Fatalf("prompt args = %#v", args)
	}
}

func TestCanonicalRunRejectsFlagConflictsBeforeExecution(t *testing.T) {
	command := newRunCommand()
	parent := t.TempDir()
	exact := filepath.Join(parent, "exact-must-not-exist")
	root := filepath.Join(parent, "root-must-not-exist")
	command.SetArgs([]string{"--workspace", exact, "--workspace-root", root, "task"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, path := range []string{exact, root} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("conflicting workspace flags mutated %q: %v", path, statErr)
		}
	}

	command = newRunCommand()
	command.SetArgs([]string{"--team", "alpha", "--agent-team", "beta", "task"})
	err = command.Execute()
	if err == nil || !strings.Contains(err.Error(), "different teams") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestValidateCanonicalRunSegments(t *testing.T) {
	segments := []team.PromptSegment{
		{Type: team.SegmentSwitchTeam, Name: "alpha"},
		{Type: team.SegmentSwitchTeam, Name: "beta"},
	}
	if err := validateCanonicalRunSegments(segments, "alpha", "root", "execute"); err == nil || !strings.Contains(err.Error(), "prompt switches") {
		t.Fatalf("explicit team conflict error = %v", err)
	}
	if err := validateCanonicalRunSegments(segments, "", "exact", "execute"); err == nil || !strings.Contains(err.Error(), "multiple teams") {
		t.Fatalf("exact multi-team error = %v", err)
	}
	if err := validateCanonicalRunSegments(segments, "", "root", "execute"); err != nil {
		t.Fatalf("root multi-team error = %v", err)
	}
	if err := validateCanonicalRunSegments(segments, "", "root", "decision"); err == nil || !strings.Contains(err.Error(), "one owner team") {
		t.Fatalf("decision multi-team error = %v", err)
	}
}

func TestCanonicalDecisionFlagMatrix(t *testing.T) {
	tests := []struct {
		name          string
		commandIntent string
		args          []string
		wantProfile   string
		wantError     string
	}{
		{name: "run decision defers team default", commandIntent: "execute", args: []string{"--intent", "decision", "question"}, wantProfile: ""},
		{name: "decide defers team default", commandIntent: "decision", args: []string{"question"}, wantProfile: ""},
		{name: "rigor light", commandIntent: "decision", args: []string{"--rigor", "light", "question"}, wantProfile: "builtin/light@v2"},
		{name: "rigor high", commandIntent: "decision", args: []string{"--rigor", "high", "question"}, wantProfile: "builtin/high-stakes@v2"},
		{name: "decide rejects execute", commandIntent: "decision", args: []string{"--intent", "execute", "question"}, wantError: "requires --intent decision"},
		{name: "execute rejects primary", commandIntent: "execute", args: []string{"--primary-decision-profile", "builtin/light@v2", "question"}, wantError: "require --intent decision"},
		{name: "primary and rigor conflict", commandIntent: "decision", args: []string{"--primary-decision-profile", "builtin/light@v2", "--rigor", "high", "question"}, wantError: "mutually exclusive"},
		{name: "decision requires journal", commandIntent: "decision", args: []string{"--no-journal", "question"}, wantError: "not supported"},
		{name: "off cannot disable primary", commandIntent: "decision", args: []string{"--primary-decision-profile", "off", "question"}, wantError: "cannot be off"},
		{name: "unknown exact builtin", commandIntent: "decision", args: []string{"--primary-decision-profile", "builtin/unknown@v2", "question"}, wantError: "unknown primary decision profile"},
		{name: "supporting profile stays separate", commandIntent: "decision", args: []string{"--primary-decision-profile", "builtin/light@v2", "question"}, wantProfile: "builtin/light@v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previous := opts
			t.Cleanup(func() { opts = previous })
			command, options := newCanonicalRunCommandWithOptions("test", tt.commandIntent)
			if err := command.ParseFlags(tt.args); err != nil {
				t.Fatal(err)
			}
			resolved, err := resolveCanonicalRunOptionsForIntent(command, options, tt.commandIntent)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resolved.primaryDecisionProfile != tt.wantProfile || resolved.intent != "decision" {
				t.Fatalf("resolved = %#v", resolved)
			}
		})
	}
}

func TestCanonicalDecisionResumeRejectsSemanticChanges(t *testing.T) {
	logical := "ldr_0123456789abcdef0123456789abcdef"
	for _, changed := range [][]string{
		{"--model", "other"}, {"--new"}, {"--temp"}, {"--input", "x=1"}, {"--rigor", "high"},
		{"--auto-team"}, {"--dry-run"}, {"--route", "fast"}, {"--skill", "audit"}, {"--allow-path", "/tmp"},
	} {
		command, options := newCanonicalRunCommandWithOptions("decide", "decision")
		args := append([]string{"--resume-decision", logical}, changed...)
		if err := command.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveCanonicalRunOptionsForIntent(command, options, "decision"); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
			t.Fatalf("args %v error = %v", args, err)
		}
	}
}

func TestResolveLoadedPrimaryDecisionProfilePrecedence(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	configured := &teamContext{session: &team.TeamSession{Config: agent.TeamConfig{Decision: agent.DecisionConfig{
		PrimaryProfile: "team-primary",
		ProfileSpecs: map[string]agent.DecisionProfileSpec{
			"team-primary": {Preset: &agent.DecisionProfileRef{Name: agent.DecisionProfileBuiltinLightV2}},
		},
	}}}}
	loaded := map[string]*teamContext{"review": configured}

	opts = runOptions{intent: "decision"}
	if err := resolveLoadedPrimaryDecisionProfile(loaded, false); err != nil {
		t.Fatal(err)
	}
	if opts.primaryDecisionProfile != agent.DecisionProfileBuiltinLightV2 || opts.primaryDecisionProfileRequested != "team-primary" || opts.primaryDecisionProfileOrigin != agent.DecisionProfileOriginTeamInline {
		t.Fatalf("team primary = %#v", opts)
	}

	opts = runOptions{intent: "decision", primaryDecisionProfile: agent.DecisionProfileBuiltinHighStakesV2}
	if err := resolveLoadedPrimaryDecisionProfile(loaded, false); err != nil {
		t.Fatal(err)
	}
	if opts.primaryDecisionProfile != agent.DecisionProfileBuiltinHighStakesV2 || opts.primaryDecisionProfileRequested != agent.DecisionProfileBuiltinHighStakesV2 {
		t.Fatalf("explicit primary did not win: %#v", opts)
	}
}
