package executioncompat

import "testing"

func TestClassifyTaskUsesOnlyDurableEvidence(t *testing.T) {
	tests := []struct {
		name   string
		input  TaskInput
		class  Classification
		reason string
	}{
		{
			name:  "canonical target",
			input: TaskInput{Target: Target{Backend: "ollama", Model: "qwen3:8b"}},
			class: ClassificationCanonical,
		},
		{
			name:   "typed local alias",
			input:  TaskInput{Target: Target{Backend: "local", Model: "qwen3:8b"}},
			class:  ClassificationMigratable,
			reason: "canonical_identity_derivable",
		},
		{
			name:   "bare model local provider",
			input:  TaskInput{Model: "qwen3:8b", SubagentProvider: "hufu-local"},
			class:  ClassificationMigratable,
			reason: "canonical_identity_derivable",
		},
		{
			name:   "bare model named provider",
			input:  TaskInput{Model: "gpt-5", SubagentProvider: "codex"},
			class:  ClassificationMigratable,
			reason: "canonical_identity_derivable",
		},
		{
			name:   "conflicting binding and receipt",
			input:  TaskInput{Model: "qwen3:8b", ProviderBinding: "openai", Receipts: []Receipt{{SubagentProvider: "hufu-local"}}},
			class:  ClassificationAmbiguous,
			reason: "conflicting_durable_backend",
		},
		{
			name:   "qualified model no evidence",
			input:  TaskInput{Model: "openai/gpt-4o"},
			class:  ClassificationAmbiguous,
			reason: "qualified_model_without_durable_backend",
		},
		{
			name:   "empty typed target",
			input:  TaskInput{Target: Target{Backend: "ollama"}},
			class:  ClassificationUnmigratable,
			reason: "invalid_typed_target",
		},
		{name: "unrelated task", class: ClassificationNotApplicable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ClassifyTask(test.input)
			if got.Classification != test.class || got.ReasonCode != test.reason {
				t.Fatalf("ClassifyTask() = %#v, want class=%q reason=%q", got, test.class, test.reason)
			}
		})
	}
}

func TestClassifyPolicySnapshot(t *testing.T) {
	if got := ClassifyPolicySnapshot(PolicyInput{Version: 4, Routes: []PolicyRoute{{Model: "qwen3:8b", Backend: "ollama"}}}); got.Classification != ClassificationCanonical {
		t.Fatalf("v4 = %#v, want canonical", got)
	}
	if got := ClassifyPolicySnapshot(PolicyInput{Version: 3, Routes: []PolicyRoute{{Model: "qwen3:8b", Backend: "local", LegacyProvider: "hufu-local"}}}); got.Classification != ClassificationAmbiguous {
		t.Fatalf("v3 conflicting legacy provider = %#v, want ambiguous", got)
	}
	if got := ClassifyPolicySnapshot(PolicyInput{Version: 3, Routes: []PolicyRoute{{Model: "qwen3:8b", Backend: "local", LegacyProvider: "local"}}}); got.Classification != ClassificationMigratable {
		t.Fatalf("v3 local route = %#v, want migratable", got)
	}
}

func TestDeriveTaskDoesNotNeedLiveConfiguration(t *testing.T) {
	derived, err := DeriveTask(TaskInput{Model: "local/qwen3:8b", SubagentProvider: "hufu-local", Receipts: []Receipt{{SubagentProvider: "hufu-local"}}})
	if err != nil {
		t.Fatalf("DeriveTask: %v", err)
	}
	if derived.Target != (Target{Backend: "ollama", Model: "qwen3:8b"}) || len(derived.Topology) != 1 || derived.ReceiptBackend[0] != "ollama" {
		t.Fatalf("derived task = %#v", derived)
	}
	if _, err := DeriveTask(TaskInput{Model: "openai/gpt-4o"}); err == nil {
		t.Fatal("qualified model without durable evidence was derived")
	}
}

func TestDeriveTaskKeepsIndependentTypedTopologyLeaves(t *testing.T) {
	input := TaskInput{
		Target: Target{Backend: "local", Model: "qwen3:8b"},
		Topology: []Target{
			{Backend: "local", Model: "qwen3:8b"},
			{Backend: "codex", Model: "gpt-5.6-luna"},
		},
	}
	derived, err := DeriveTask(input)
	if err != nil {
		t.Fatalf("DeriveTask: %v", err)
	}
	if derived.Target != (Target{Backend: "ollama", Model: "qwen3:8b"}) || len(derived.Topology) != 2 || derived.Topology[1] != (Target{Backend: "codex", Model: "gpt-5.6-luna"}) {
		t.Fatalf("derived topology = %#v", derived)
	}
}
