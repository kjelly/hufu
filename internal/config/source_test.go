package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceNamesTheLastFileThatSetEachKey(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home.yaml")
	project := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(home, []byte("model: home-model\nsidecar-model: home-sidecar\nexecution-routes:\n  review:\n    candidates: [ollama/a]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte("sidecar-model: project-sidecar\nguard-model: project-guard\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	cfg.mergeFromFile(home)
	cfg.mergeFromFile(project)

	tests := []struct {
		key  string
		want string
	}{
		{key: "model", want: home},
		{key: "sidecar-model", want: project},
		{key: "guard-model", want: project},
		{key: "execution-routes", want: home},
		{key: "judge-model", want: ""},
		{key: "worker-model", want: ""},
	}
	for _, tt := range tests {
		if got := cfg.Source(tt.key); got != tt.want {
			t.Errorf("Source(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
	if got := (*Config)(nil).Source("model"); got != "" {
		t.Errorf("nil Config Source = %q, want empty", got)
	}
}
