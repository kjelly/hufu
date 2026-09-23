package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParseWorkerModelOverrides(t *testing.T) {
	tests := []struct {
		name    string
		values  []string
		want    []WorkerModelOverride
		wantErr string
	}{
		{
			name:   "single entry",
			values: []string{"coder=codex/gpt-6-sol"},
			want:   []WorkerModelOverride{{Agent: "coder", Target: "codex/gpt-6-sol"}},
		},
		{
			name:   "surrounding whitespace is trimmed",
			values: []string{" coder = codex/gpt-6-sol "},
			want:   []WorkerModelOverride{{Agent: "coder", Target: "codex/gpt-6-sol"}},
		},
		{
			name:   "target keeps later equals signs",
			values: []string{"coder=backend/model=variant"},
			want:   []WorkerModelOverride{{Agent: "coder", Target: "backend/model=variant"}},
		},
		{
			name:   "ollama tag target",
			values: []string{"reviewer=ollama/qwen3.5:27b"},
			want:   []WorkerModelOverride{{Agent: "reviewer", Target: "ollama/qwen3.5:27b"}},
		},
		{
			name:   "repeated values",
			values: []string{"coder=A", "reviewer=B"},
			want:   []WorkerModelOverride{{Agent: "coder", Target: "A"}, {Agent: "reviewer", Target: "B"}},
		},
		{
			name:   "comma-separated profile value",
			values: []string{"sa=A,coder=B,reviewer=C"},
			want:   []WorkerModelOverride{{Agent: "sa", Target: "A"}, {Agent: "coder", Target: "B"}, {Agent: "reviewer", Target: "C"}},
		},
		{
			name:   "duplicate agent last occurrence wins",
			values: []string{"coder=A", "reviewer=X", "coder=B"},
			want:   []WorkerModelOverride{{Agent: "coder", Target: "B"}, {Agent: "reviewer", Target: "X"}},
		},
		{
			name:   "duplicate agent matches case-insensitively",
			values: []string{"Coder=A,coder=B"},
			want:   []WorkerModelOverride{{Agent: "coder", Target: "B"}},
		},
		{
			name:   "no values",
			values: nil,
			want:   nil,
		},
		{name: "missing separator", values: []string{"coder"}, wantErr: "expected agent=target"},
		{name: "missing agent", values: []string{"=codex/gpt-6-sol"}, wantErr: "missing agent name"},
		{name: "missing target", values: []string{"coder="}, wantErr: "missing execution target"},
		{name: "whitespace-only target", values: []string{" coder = "}, wantErr: "missing execution target"},
		{name: "empty comma entry", values: []string{"coder=A,,reviewer=B"}, wantErr: "expected agent=target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseWorkerModelOverrides(tt.values)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseWorkerModelOverrides(%q) error = %v, want containing %q", tt.values, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWorkerModelOverrides(%q) unexpected error: %v", tt.values, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseWorkerModelOverrides(%q) = %#v, want %#v", tt.values, got, tt.want)
			}
		})
	}
}

func TestMergeWorkerModelOverrides(t *testing.T) {
	tests := []struct {
		name    string
		base    []WorkerModelOverride
		overlay []WorkerModelOverride
		want    []string
	}{
		{
			name:    "overlay replaces same agent and keeps other base entries",
			base:    []WorkerModelOverride{{Agent: "sa", Target: "A"}, {Agent: "coder", Target: "A"}, {Agent: "reviewer", Target: "C"}},
			overlay: []WorkerModelOverride{{Agent: "coder", Target: "B"}},
			want:    []string{"sa=A", "coder=B", "reviewer=C"},
		},
		{
			name:    "overlay key matches case-insensitively",
			base:    []WorkerModelOverride{{Agent: "coder", Target: "A"}},
			overlay: []WorkerModelOverride{{Agent: "Coder", Target: "B"}},
			want:    []string{"Coder=B"},
		},
		{
			name:    "new overlay agents are appended",
			base:    []WorkerModelOverride{{Agent: "sa", Target: "A"}},
			overlay: []WorkerModelOverride{{Agent: "verifier", Target: "V"}},
			want:    []string{"sa=A", "verifier=V"},
		},
		{
			name: "empty layers",
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatWorkerModelOverrides(mergeWorkerModelOverrides(tt.base, tt.overlay))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("merge = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkerModelFlagParsesOnRootAndCanonicalRun(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "single", args: []string{"--worker-model", "coder=A"}, want: []string{"coder=A"}},
		{name: "repeated", args: []string{"--worker-model", "coder=A", "--worker-model", "reviewer=B"}, want: []string{"coder=A", "reviewer=B"}},
		{name: "comma-separated", args: []string{"--worker-model", "coder=A,reviewer=B"}, want: []string{"coder=A", "reviewer=B"}},
		{name: "absent", args: nil, want: nil},
	}
	for _, tt := range tests {
		t.Run("root/"+tt.name, func(t *testing.T) {
			previous := opts
			t.Cleanup(func() { opts = previous })
			opts = runOptions{}
			root := newRootCommand()
			if err := root.ParseFlags(append(append([]string{}, tt.args...), "task")); err != nil {
				t.Fatal(err)
			}
			if got := opts.workerModelOverrides; !slices.Equal(got, tt.want) {
				t.Fatalf("root --worker-model = %q, want %q", got, tt.want)
			}
		})
		t.Run("run/"+tt.name, func(t *testing.T) {
			previous := opts
			t.Cleanup(func() { opts = previous })
			opts = runOptions{}
			command, options := newRunCommandWithOptions()
			if err := command.ParseFlags(append(append([]string{}, tt.args...), "task")); err != nil {
				t.Fatal(err)
			}
			resolved, err := resolveCanonicalRunOptions(command, options)
			if err != nil {
				t.Fatal(err)
			}
			if got := resolved.workerModelOverrides; !slices.Equal(got, tt.want) {
				t.Fatalf("run --worker-model = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResumeDecisionRejectsWorkerModel(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts = runOptions{}
	command, options := newCanonicalRunCommandWithOptions("decide", "decision")
	if err := command.ParseFlags([]string{"--resume-decision", "ldr_" + strings.Repeat("a", 32), "--worker-model", "coder=A"}); err != nil {
		t.Fatal(err)
	}
	_, err := resolveCanonicalRunOptionsForIntent(command, options, "decision")
	if err == nil || !strings.Contains(err.Error(), "--worker-model") {
		t.Fatalf("resume-decision with --worker-model error = %v", err)
	}
}
