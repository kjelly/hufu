package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestReviewerPromptDefersToRuntimeResultProtocol(t *testing.T) {
	prompt, err := os.ReadFile(filepath.Join("..", "reviewer.md"))
	if err != nil {
		t.Fatalf("read reviewer prompt: %v", err)
	}
	text := strings.Join(strings.Fields(string(prompt)), " ")
	for _, required := range []string{
		"runtime-provided result protocol",
		"active runtime schema",
		"`files_read` using exactly the representation required by the active schema",
		"cite the supplied opaque artifact identifier exactly",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("reviewer prompt omitted %q", required)
		}
	}
	for _, forbidden := range []string{
		"`submit_result`",
		"WorkerResultProposal",
		"local tool",
		"external structured result provider",
		"`files_read` object",
		"array of non-empty strings",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("reviewer prompt retained transport-specific instruction %q", forbidden)
		}
	}
}

func TestReviewWorkerPromptsRequireArtifactOnlySourceEvidence(t *testing.T) {
	for filename, allowed := range map[string][]string{
		"reviewer.md":               {"view"},
		"critic.md":                 {"view", "decision_primitive"},
		"documentation-reviewer.md": {"view"},
	} {
		t.Run(filename, func(t *testing.T) {
			prompt, err := os.ReadFile(filepath.Join("..", filename))
			if err != nil {
				t.Fatal(err)
			}
			text := string(prompt)
			for _, required := range []string{
				"reviewed-source snapshot",
				"never use `file_path`",
				"live checkout",
			} {
				if !strings.Contains(text, required) {
					t.Fatalf("%s omitted artifact-only instruction %q", filename, required)
				}
			}
			if err := reviewWorkerToolPolicy(prompt, filename, allowed); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Use the runtime parser so scalar, list, nested grants, presets, and implied
// tools are checked according to the same rules used when loading a real agent.
func reviewWorkerToolPolicy(raw []byte, filename string, allowed []string) error {
	def, err := team.ValidateAgentContent(raw, filename)
	if err != nil {
		return err
	}
	tools := slices.Sorted(strings.SplitSeq(def.Tools, ","))
	want := slices.Sorted(slices.Values(allowed))
	if !slices.Equal(tools, want) {
		return fmt.Errorf("%s grants %v, want exactly %v", filename, tools, allowed)
	}
	return nil
}

func TestReviewWorkerToolPolicyRejectsAdditionalGrants(t *testing.T) {
	for _, test := range []struct {
		name, frontmatter string
		allowed           []string
		wantErr           bool
	}{
		{name: "scalar", frontmatter: "tools: view", allowed: []string{"view"}},
		{name: "list", frontmatter: "tools: [view]", allowed: []string{"view"}},
		{name: "nested", frontmatter: "tools: {allowed: [view]}", allowed: []string{"view"}},
		{name: "critic", frontmatter: "tools: view,decision_primitive", allowed: []string{"decision_primitive", "view"}},
		{name: "extra execution", frontmatter: "tools: view,bash", allowed: []string{"view"}, wantErr: true},
		{name: "extra search", frontmatter: "tools: view,grep", allowed: []string{"view"}, wantErr: true},
		{name: "name prefix", frontmatter: "tools: viewbash", allowed: []string{"view"}, wantErr: true},
		{name: "list execution", frontmatter: "tools: [view, bash]", allowed: []string{"view"}, wantErr: true},
		{name: "nested execution", frontmatter: "tools: {allowed: [view, bash]}", allowed: []string{"view"}, wantErr: true},
		{name: "preset grants", frontmatter: "tools: view\npreset: coding", allowed: []string{"view"}, wantErr: true},
		{name: "wildcard", frontmatter: "tools: all", allowed: []string{"view"}, wantErr: true},
		{name: "missing tools", frontmatter: "", allowed: []string{"view"}, wantErr: true},
		{name: "critic execution", frontmatter: "tools: view,decision_primitive,bash", allowed: []string{"decision_primitive", "view"}, wantErr: true},
		{name: "critic missing primitive", frontmatter: "tools: view", allowed: []string{"decision_primitive", "view"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte("---\nname: reviewer\nrole: worker\n" + test.frontmatter + "\n---\nReview immutable artifacts.\n")
			err := reviewWorkerToolPolicy(raw, "reviewer.md", test.allowed)
			if (err != nil) != test.wantErr {
				t.Fatalf("tool policy error = %v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}
