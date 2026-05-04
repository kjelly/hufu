package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseVarFlags(t *testing.T) {
	tests := []struct {
		name  string
		flags []string
		want  map[string]string
	}{
		{
			name:  "empty",
			flags: nil,
			want:  map[string]string{},
		},
		{
			name:  "simple key=value",
			flags: []string{"model=qwen3:8b"},
			want:  map[string]string{"model": "qwen3:8b"},
		},
		{
			name:  "value with equals",
			flags: []string{"prompt=do x=y"},
			want:  map[string]string{"prompt": "do x=y"},
		},
		{
			name:  "no value",
			flags: []string{"debug"},
			want:  map[string]string{"debug": ""},
		},
		{
			name:  "multiple flags",
			flags: []string{"model=qwen3:8b", "temperature=0.7", "debug=true"},
			want:  map[string]string{"model": "qwen3:8b", "temperature": "0.7", "debug": "true"},
		},
		{
			name:  "later overrides earlier",
			flags: []string{"model=qwen3:8b", "model=deepseek:14b"},
			want:  map[string]string{"model": "deepseek:14b"},
		},
		{
			name:  "empty value",
			flags: []string{"key="},
			want:  map[string]string{"key": ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseVarFlags(tt.flags)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseVarFlags(%v) = %v (len %d), want %v (len %d)", tt.flags, got, len(got), tt.want, len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("ParseVarFlags(%v)[%q] = %q, want %q", tt.flags, k, got[k], v)
				}
			}
		})
	}
}

func TestLoadVarsFileEnvFormat(t *testing.T) {
	content := `# Comment
DB_HOST=localhost
DB_PORT=5432
EMPTY_VAR=
QUOTED_VAR="hello world"
SINGLE_QUOTED='value with spaces'
`
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "vars.env")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadVarsFile(path)
	if err != nil {
		t.Fatalf("LoadVarsFile() error: %v", err)
	}

	if m["DB_HOST"] != "localhost" {
		t.Errorf("DB_HOST = %q, want %q", m["DB_HOST"], "localhost")
	}
	if m["DB_PORT"] != "5432" {
		t.Errorf("DB_PORT = %q, want %q", m["DB_PORT"], "5432")
	}
	if m["EMPTY_VAR"] != "" {
		t.Errorf("EMPTY_VAR = %q, want empty", m["EMPTY_VAR"])
	}
	if m["QUOTED_VAR"] != "hello world" {
		t.Errorf("QUOTED_VAR = %q, want %q", m["QUOTED_VAR"], "hello world")
	}
	if m["SINGLE_QUOTED"] != "value with spaces" {
		t.Errorf("SINGLE_QUOTED = %q, want %q", m["SINGLE_QUOTED"], "value with spaces")
	}
	if _, ok := m["Comment"]; ok {
		t.Error("comment line should not be parsed")
	}
}

func TestLoadVarsFileYAMLFormat(t *testing.T) {
	content := `model: qwen3:8b
temperature: "0.7"
debug: true
max_rounds: 10
`
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "vars.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadVarsFile(path)
	if err != nil {
		t.Fatalf("LoadVarsFile() error: %v", err)
	}

	if m["model"] != "qwen3:8b" {
		t.Errorf("model = %q, want %q", m["model"], "qwen3:8b")
	}
	if m["temperature"] != "0.7" {
		t.Errorf("temperature = %q, want %q", m["temperature"], "0.7")
	}
	if m["debug"] != "true" {
		t.Errorf("debug = %q, want %q", m["debug"], "true")
	}
	if m["max_rounds"] != "10" {
		t.Errorf("max_rounds = %q, want %q", m["max_rounds"], "10")
	}
}

func TestLoadVarsFileYAMLNested(t *testing.T) {
	content := `model:
  id: qwen3:8b
  temperature: 0.7
`
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "vars.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadVarsFile(path)
	if err != nil {
		t.Fatalf("LoadVarsFile() error: %v", err)
	}

	if m["model.id"] != "qwen3:8b" {
		t.Errorf("model.id = %q, want %q", m["model.id"], "qwen3:8b")
	}
	if m["model.temperature"] != "0.7" {
		t.Errorf("model.temperature = %q, want %q", m["model.temperature"], "0.7")
	}
}

func TestLoadVarsFileNotFound(t *testing.T) {
	_, err := LoadVarsFile("/nonexistent/vars.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestMergeVars(t *testing.T) {
	tests := []struct {
		name string
		maps []map[string]string
		want map[string]string
	}{
		{
			name: "nil maps",
			maps: nil,
			want: map[string]string{},
		},
		{
			name: "single map",
			maps: []map[string]string{{"a": "1", "b": "2"}},
			want: map[string]string{"a": "1", "b": "2"},
		},
		{
			name: "later overrides earlier",
			maps: []map[string]string{{"a": "1", "b": "2"}, {"a": "3", "c": "4"}},
			want: map[string]string{"a": "3", "b": "2", "c": "4"},
		},
		{
			name: "three maps",
			maps: []map[string]string{{"a": "1"}, {"b": "2"}, {"a": "3"}},
			want: map[string]string{"a": "3", "b": "2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeVars(tt.maps...)
			if len(got) != len(tt.want) {
				t.Fatalf("MergeVars() = %v (len %d), want %v (len %d)", got, len(got), tt.want, len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("MergeVars()[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestResolveVars(t *testing.T) {
	t.Run("no vars", func(t *testing.T) {
		m, err := ResolveVars(nil, nil)
		if err != nil {
			t.Fatalf("ResolveVars() error: %v", err)
		}
		if m != nil {
			t.Errorf("ResolveVars() = %v, want nil", m)
		}
	})

	t.Run("flags only", func(t *testing.T) {
		m, err := ResolveVars(nil, []string{"model=qwen3:8b"})
		if err != nil {
			t.Fatalf("ResolveVars() error: %v", err)
		}
		if m["model"] != "qwen3:8b" {
			t.Errorf("model = %q, want %q", m["model"], "qwen3:8b")
		}
	})

	t.Run("file and flags merge", func(t *testing.T) {
		tmpDir := t.TempDir()
		content := "model=qwen3:8b\ntemperature=0.7\n"
		path := filepath.Join(tmpDir, "vars.env")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}

		m, err := ResolveVars([]string{path}, []string{"model=deepseek:14b"})
		if err != nil {
			t.Fatalf("ResolveVars() error: %v", err)
		}
		if m["model"] != "deepseek:14b" {
			t.Errorf("model = %q, want %q (flag overrides file)", m["model"], "deepseek:14b")
		}
		if m["temperature"] != "0.7" {
			t.Errorf("temperature = %q, want %q", m["temperature"], "0.7")
		}
	})

	t.Run("multiple files, later overrides earlier", func(t *testing.T) {
		tmpDir := t.TempDir()

		content1 := "model=qwen3:8b\ntemperature=0.5\n"
		path1 := filepath.Join(tmpDir, "base.env")
		if err := os.WriteFile(path1, []byte(content1), 0644); err != nil {
			t.Fatal(err)
		}

		content2 := "model=deepseek:14b\n"
		path2 := filepath.Join(tmpDir, "override.env")
		if err := os.WriteFile(path2, []byte(content2), 0644); err != nil {
			t.Fatal(err)
		}

		m, err := ResolveVars([]string{path1, path2}, nil)
		if err != nil {
			t.Fatalf("ResolveVars() error: %v", err)
		}
		if m["model"] != "deepseek:14b" {
			t.Errorf("model = %q, want %q", m["model"], "deepseek:14b")
		}
		if m["temperature"] != "0.5" {
			t.Errorf("temperature = %q, want %q", m["temperature"], "0.5")
		}
	})
}
