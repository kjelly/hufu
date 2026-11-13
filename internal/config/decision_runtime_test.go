package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDecisionRuntimeNormalizesBeforeFileMerge(t *testing.T) {
	for _, tc := range []struct {
		name, base, override string
		conflict             bool
	}{
		{name: "legacy then canonical", base: "control-decisions:\n  model: global\n  timeout: 60s\n", override: "decision-runtime:\n  systemone:\n    model: project\n"},
		{name: "canonical then legacy", base: "decision-runtime:\n  backend: systemone\n  systemone:\n    model: global\n    timeout: 60s\n", override: "control-decisions:\n  model: project\n"},
		{name: "same layer conflict", base: "decision-runtime:\n  systemone:\n    model: one\ncontrol-decisions:\n  model: two\n", override: "decision-runtime:\n  systemone:\n    model: project\n", conflict: true},
		{name: "typed parse error remains fatal", base: "decision-runtime:\n  backend: [systemone]\n", override: "decision-runtime:\n  backend: sidecar\n", conflict: true},
		{name: "syntax error remains fatal", base: "decision-runtime:\n  backend: [\n", override: "decision-runtime:\n  backend: sidecar\n", conflict: true},
		{name: "duplicate block remains fatal", base: "decision-runtime: {backend: systemone}\ndecision-runtime: {backend: sidecar}\n", override: "decision-runtime:\n  backend: sidecar\n", conflict: true},
		{name: "invalid timeout remains fatal", base: "decision-runtime:\n  sidecar:\n    timeout: -1s\n", override: "decision-runtime:\n  backend: sidecar\n", conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			base, override := filepath.Join(dir, "global.yaml"), filepath.Join(dir, "project.yaml")
			if err := os.WriteFile(base, []byte(tc.base), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(override, []byte(tc.override), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := &Config{}
			cfg.mergeFromFile(base)
			cfg.mergeFromFile(override)
			shared, err := cfg.ResolveDecisionRuntime()
			if (err != nil) != tc.conflict {
				t.Fatalf("shared=%+v err=%v", shared, err)
			}
			if err != nil {
				return
			}
			if shared.Backend != "systemone" || shared.SystemOne.Model != "project" || shared.SystemOne.Timeout != time.Minute || cfg.ControlDecisions.HasTransport() {
				t.Fatalf("shared=%+v policy=%+v", shared, cfg.ControlDecisions)
			}
		})
	}
}

func TestDecisionRuntimeConfigurationReadFailures(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "directory"
		if missing {
			name = "missing_optional_file"
		}
		t.Run(name, func(t *testing.T) {
			path := t.TempDir()
			if missing {
				path = filepath.Join(path, "absent.yaml")
			}
			cfg := &Config{}
			cfg.mergeFromFile(path)
			_, err := cfg.ResolveDecisionRuntime()
			if (err != nil) == missing {
				t.Fatalf("missing=%v err=%v", missing, err)
			}
		})
	}
}
