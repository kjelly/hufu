package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestParseContextArtifactsPolicy(t *testing.T) {
	defaults := agent.DefaultContextArtifactsPolicy()
	enabledDefaults := defaults
	enabledDefaults.Enabled = true
	cases := []struct {
		name    string
		block   string
		want    agent.ContextArtifactsPolicy
		wantErr string
	}{
		{name: "missing block is disabled zero value", want: agent.ContextArtifactsPolicy{}},
		{name: "enabled with defaults", block: "context-artifacts:\n  enabled: true\n", want: enabledDefaults},
		{name: "disabled block keeps defaults", block: "context-artifacts:\n  enabled: false\n", want: defaults},
		{name: "explicit values", block: "context-artifacts:\n  enabled: true\n  min-bytes: 65536\n  preview-bytes: 4096\n  max-artifact-bytes: 2097152\n  max-read-bytes: 65536\n  max-artifacts-per-attempt: 8\n",
			want: agent.ContextArtifactsPolicy{Enabled: true, MinBytes: 65536, PreviewBytes: 4096, MaxArtifactBytes: 2097152, MaxReadBytes: 65536, MaxArtifactsPerAttempt: 8}},
		{name: "explicit zero is rejected", block: "context-artifacts:\n  enabled: true\n  min-bytes: 0\n", wantErr: "min-bytes must be positive"},
		{name: "negative is rejected even when disabled", block: "context-artifacts:\n  max-read-bytes: -1\n", wantErr: "max-read-bytes must be positive"},
		{name: "preview below floor", block: "context-artifacts:\n  enabled: true\n  preview-bytes: 512\n", wantErr: "preview-bytes must be at least 1024"},
		{name: "preview not below min", block: "context-artifacts:\n  enabled: true\n  preview-bytes: 51200\n", wantErr: "must be less than min-bytes"},
		{name: "min above max artifact", block: "context-artifacts:\n  enabled: true\n  max-artifact-bytes: 40000\n", wantErr: "must not exceed max-artifact-bytes"},
		{name: "artifact cap", block: "context-artifacts:\n  enabled: true\n  max-artifact-bytes: 4194305\n", wantErr: "max-artifact-bytes must not exceed 4194304"},
		{name: "read cap", block: "context-artifacts:\n  enabled: true\n  max-artifact-bytes: 4194304\n  max-read-bytes: 524289\n", wantErr: "max-read-bytes must not exceed 524288"},
		{name: "read above artifact size", block: "context-artifacts:\n  enabled: true\n  min-bytes: 8192\n  preview-bytes: 4096\n  max-artifact-bytes: 16384\n  max-read-bytes: 32768\n", wantErr: "max-read-bytes must not exceed 16384"},
		{name: "unknown key is rejected", block: "context-artifacts:\n  enabled: true\n  preview: 1\n", wantErr: "preview"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			content := "name: context-artifacts-parse\n" + tc.block
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := parseTeamYML(dir, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), "context-artifacts") && tc.name != "unknown key is rejected" {
					t.Fatalf("error %q is not wrapped with the block name", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ContextArtifacts != tc.want {
				t.Fatalf("policy = %+v, want %+v", cfg.ContextArtifacts, tc.want)
			}
		})
	}
}

// newContextArtifactsPolicyCoordinator sets the policy before construction:
// the execution-policy snapshot is computed at coordinator startup.
func newContextArtifactsPolicyCoordinator(t *testing.T, workspace string, policy agent.ContextArtifactsPolicy) *Coordinator {
	t.Helper()
	session := &TeamSession{
		Dir:       workspace,
		Workspace: workspace,
		Config:    agent.TeamConfig{Name: "context-artifacts-resume", DefaultLLMBackend: "ollama", ContextArtifacts: policy},
		Agents: map[string]*agent.AgentDef{
			"coder": {Name: "coder", Role: "worker", Generation: agent.GenerationParams{Model: "ollama/coder"}},
		},
	}
	subjectRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.SetCompatibilityWorkspaceScope(subjectRoot); err != nil {
		t.Fatal(err)
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.eventStore != nil {
			_ = c.eventStore.Close()
		}
		c.CloseContextPreflight()
	})
	return c
}

func TestContextArtifactsSnapshotOmittedWhenDisabled(t *testing.T) {
	disabled := newContextArtifactsPolicyCoordinator(t, t.TempDir(), agent.DefaultContextArtifactsPolicy())
	baseline := newContextArtifactsPolicyCoordinator(t, t.TempDir(), agent.ContextArtifactsPolicy{})
	disabledState, err := newExecutionPolicyState(disabled)
	if err != nil {
		t.Fatal(err)
	}
	baselineState, err := newExecutionPolicyState(baseline)
	if err != nil {
		t.Fatal(err)
	}
	disabledJSON, _ := json.Marshal(disabledState.snapshot)
	baselineJSON, _ := json.Marshal(baselineState.snapshot)
	if string(disabledJSON) != string(baselineJSON) || strings.Contains(string(disabledJSON), "context_artifacts") {
		t.Fatalf("disabled snapshot differs from baseline:\n%s\n%s", disabledJSON, baselineJSON)
	}
	if _, ok := disabled.admittedContextArtifactPolicy(); ok {
		t.Fatal("disabled policy reported as admitted before admission")
	}

	enabledPolicy := agent.DefaultContextArtifactsPolicy()
	enabledPolicy.Enabled = true
	enabled := newContextArtifactsPolicyCoordinator(t, t.TempDir(), enabledPolicy)
	enabledState, err := newExecutionPolicyState(enabled)
	if err != nil {
		t.Fatal(err)
	}
	if enabledState.snapshot.ContextArtifacts == nil || enabledState.snapshot.ConfigurationHash == baselineState.snapshot.ConfigurationHash {
		t.Fatalf("enabled snapshot did not pin context artifacts: %+v", enabledState.snapshot)
	}
	clone := cloneExecutionPolicySnapshot(enabledState.snapshot)
	clone.ContextArtifacts.PreviewBytes++
	if enabledState.snapshot.ContextArtifacts.PreviewBytes == clone.ContextArtifacts.PreviewBytes {
		t.Fatal("clone shares the context artifact policy pointer")
	}
	if err := validateExecutionPolicySnapshot(enabledState.snapshot); err != nil {
		t.Fatalf("enabled snapshot invalid: %v", err)
	}
}

func TestContextArtifactsChangeOnResumeFailsClosedOnPolicyDrift(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	enabled := agent.DefaultContextArtifactsPolicy()
	enabled.Enabled = true

	first := newContextArtifactsPolicyCoordinator(t, workspace, enabled)
	first.initEventStore()
	if err := first.checkRunAdmission(); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	if got, ok := first.admittedContextArtifactPolicy(); !ok || got.PreviewBytes != enabled.PreviewBytes {
		t.Fatalf("admitted policy = %+v ok=%v", got, ok)
	}
	closeWorkerModelResumeStore(t, first)

	for _, tc := range []struct {
		name   string
		policy agent.ContextArtifactsPolicy
	}{
		{name: "value changed", policy: func() agent.ContextArtifactsPolicy { p := enabled; p.PreviewBytes = 4096; return p }()},
		{name: "disabled", policy: agent.DefaultContextArtifactsPolicy()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := newContextArtifactsPolicyCoordinator(t, workspace, tc.policy)
			changed.SetSessionData(LoadSession(workspace))
			changed.initEventStore()
			if err := changed.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "snapshot drift detected") {
				t.Fatalf("resume error = %v, want policy snapshot drift", err)
			}
			closeWorkerModelResumeStore(t, changed)
		})
	}

	unchanged := newContextArtifactsPolicyCoordinator(t, workspace, enabled)
	unchanged.SetSessionData(LoadSession(workspace))
	unchanged.initEventStore()
	if err := unchanged.checkRunAdmission(); err != nil {
		t.Fatalf("resume with unchanged policy: %v", err)
	}
}
