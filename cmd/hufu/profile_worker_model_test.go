package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newWorkerModelProfileTestCmd binds the two flags whose precedence interacts
// in applyNamedProfile to the package-level options, like the root command.
func newWorkerModelProfileTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test", Run: func(*cobra.Command, []string) {}}
	cmd.Flags().StringVarP(&opts.modelOverride, "model", "m", "", "")
	cmd.Flags().StringSliceVar(&opts.workerModelOverrides, "worker-model", nil, "")
	cmd.SetErr(new(bytes.Buffer))
	return cmd
}

func TestApplyProfileWorkerModelPrecedence(t *testing.T) {
	tests := []struct {
		name        string
		profile     string
		args        []string
		wantModel   string
		wantWorkers []string
		wantErr     string
	}{
		{
			name:        "profile global and profile worker",
			profile:     "model: A\n    worker-model: \"coder=B\"",
			wantModel:   "A",
			wantWorkers: []string{"coder=B"},
		},
		{
			name:        "unquoted comma-separated profile value",
			profile:     "worker-model: coder=A,reviewer=B",
			wantWorkers: []string{"coder=A", "reviewer=B"},
		},
		{
			name:        "explicit CLI model suppresses profile worker entries",
			profile:     "worker-model: \"coder=B\"",
			args:        []string{"--model", "A"},
			wantModel:   "A",
			wantWorkers: nil,
		},
		{
			name:        "explicit CLI worker beats profile global",
			profile:     "model: A",
			args:        []string{"--worker-model", "coder=B"},
			wantModel:   "A",
			wantWorkers: []string{"coder=B"},
		},
		{
			name:        "CLI worker entries merge over profile entries by agent",
			profile:     "worker-model: \"sa=A,coder=A,reviewer=C\"",
			args:        []string{"--worker-model", "coder=B"},
			wantWorkers: []string{"sa=A", "coder=B", "reviewer=C"},
		},
		{
			name:        "CLI worker merge matches agent case-insensitively",
			profile:     "worker-model: \"coder=A\"",
			args:        []string{"--worker-model", "Coder=B"},
			wantWorkers: []string{"Coder=B"},
		},
		{
			name:        "explicit CLI model and worker keep only CLI worker entries",
			profile:     "model: P\n    worker-model: \"sa=A,coder=A\"",
			args:        []string{"--model", "G", "--worker-model", "coder=B"},
			wantModel:   "G",
			wantWorkers: []string{"coder=B"},
		},
		{
			name:    "malformed effective profile worker-model fails",
			profile: "worker-model: \"coder\"",
			wantErr: `profile "p": invalid value for --worker-model`,
		},
		{
			name:    "malformed profile worker-model fails when merged with CLI entries",
			profile: "worker-model: \"coder=\"",
			args:    []string{"--worker-model", "reviewer=B"},
			wantErr: `profile "p": invalid value for --worker-model`,
		},
		{
			name:      "malformed profile worker-model is ignored under explicit CLI model",
			profile:   "worker-model: \"coder\"",
			args:      []string{"--model", "A"},
			wantModel: "A",
		},
		{
			name:    "malformed CLI worker entry is not attributed to the profile",
			profile: "worker-model: \"sa=A\"",
			args:    []string{"--worker-model", "coder"},
			wantErr: `invalid --worker-model entry "coder"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeHufuYAML(t, dir, "profiles:\n  p:\n    "+tt.profile+"\n")
			defer chdir(t, dir)()
			previous := opts
			t.Cleanup(func() { opts = previous })
			opts = runOptions{profileName: "p"}

			cmd := newWorkerModelProfileTestCmd()
			if err := cmd.ParseFlags(tt.args); err != nil {
				t.Fatal(err)
			}
			err := applyProfile(cmd)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("applyProfile() error = %v, want containing %q", err, tt.wantErr)
				}
				if strings.HasPrefix(tt.wantErr, "invalid --worker-model") && strings.Contains(err.Error(), "profile") {
					t.Fatalf("CLI parse error attributed to profile: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyProfile() error = %v", err)
			}
			if opts.modelOverride != tt.wantModel {
				t.Errorf("model = %q, want %q", opts.modelOverride, tt.wantModel)
			}
			if !slices.Equal(opts.workerModelOverrides, tt.wantWorkers) {
				t.Errorf("worker-model = %q, want %q", opts.workerModelOverrides, tt.wantWorkers)
			}
		})
	}
}

func TestCanonicalRunMergesProfileWorkerModels(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, `
profiles:
  balanced:
    model: codex/gpt-6-luna
    worker-model: "sa=codex/gpt-6-luna,coder=codex/gpt-6-luna,reviewer=codex/gpt-6-sol"
`)
	defer chdir(t, dir)()
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts = runOptions{profileName: "balanced"}

	command, options := newRunCommandWithOptions()
	command.SetErr(new(bytes.Buffer))
	if err := command.ParseFlags([]string{"--worker-model", "coder=codex/gpt-6-sol", "task"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveCanonicalRunOptions(command, options)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.modelOverride != "codex/gpt-6-luna" {
		t.Fatalf("model = %q, want profile model", resolved.modelOverride)
	}
	want := []string{"sa=codex/gpt-6-luna", "coder=codex/gpt-6-sol", "reviewer=codex/gpt-6-sol"}
	if !slices.Equal(resolved.workerModelOverrides, want) {
		t.Fatalf("worker-model = %q, want %q", resolved.workerModelOverrides, want)
	}
}
