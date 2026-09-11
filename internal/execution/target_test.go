package execution

import "testing"

func TestParseExecutionSelector(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		backend     string
		model       string
		shouldError bool
	}{
		{name: "codex", raw: "codex/gpt-5.6-luna", backend: "codex", model: "gpt-5.6-luna"},
		{name: "nested model", raw: "openrouter/meta-llama/llama-3.3", backend: "openrouter", model: "meta-llama/llama-3.3"},
		{name: "ollama", raw: "ollama/qwen3:8b", backend: "ollama", model: "qwen3:8b"},
		{name: "legacy local", raw: "local/qwen3:8b", backend: "ollama", model: "qwen3:8b"},
		{name: "bare", raw: "qwen3:8b", model: "qwen3:8b"},
		{name: "mixed backend", raw: "CoDeX/GPT-5", backend: "codex", model: "GPT-5"},
		{name: "empty", raw: "", shouldError: true},
		{name: "missing backend", raw: "/model", shouldError: true},
		{name: "missing model", raw: "codex/", shouldError: true},
		{name: "space", raw: "codex/model name", shouldError: true},
		{name: "surrounding space", raw: " codex/model", shouldError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseExecutionSelector(tt.raw)
			if tt.shouldError {
				if err == nil {
					t.Fatalf("ParseExecutionSelector(%q) succeeded", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseExecutionSelector(%q): %v", tt.raw, err)
			}
			if got.Raw != tt.raw || got.Backend != tt.backend || got.Model != tt.model {
				t.Fatalf("selector = %#v, want raw=%q backend=%q model=%q", got, tt.raw, tt.backend, tt.model)
			}
		})
	}
}

func TestExecutionTargetValidateAndString(t *testing.T) {
	valid := ExecutionTarget{Backend: "codex", Model: "gpt-5.6-luna"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate valid target: %v", err)
	}
	if got := valid.String(); got != "codex/gpt-5.6-luna" {
		t.Fatalf("String() = %q", got)
	}
	for _, target := range []ExecutionTarget{
		{},
		{Backend: "", Model: "model"},
		{Backend: "Ollama", Model: "model"},
		{Backend: "codex", Model: "model name"},
	} {
		if err := target.Validate(); err == nil {
			t.Fatalf("Validate(%#v) succeeded", target)
		}
	}
}
