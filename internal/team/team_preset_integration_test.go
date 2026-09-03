package team

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team/preset"
)

// Phase 7 (spec.md Specification 05 / Specification 01 §8): team presets
// compile down to normal agent files (an AgentPreset name in frontmatter),
// so they must load through the ordinary CompileTeam/LoadTeam pipeline
// like any hand-authored team, with no dedicated runtime support.
func TestTeamPreset_RenderedFilesCompileThroughOrdinaryPipeline(t *testing.T) {
	for _, name := range preset.TeamNames() {
		name := name
		t.Run(name, func(t *testing.T) {
			teamPreset, ok := preset.LookupTeam(name)
			if !ok {
				t.Fatalf("preset.LookupTeam(%q) not found", name)
			}
			dir := t.TempDir()
			for filename, content := range teamPreset.Render() {
				if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
					t.Fatalf("write %s: %v", filename, err)
				}
			}

			spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
			if err != nil {
				t.Fatalf("CompileTeam(%q): %v", name, err)
			}
			// A normal team preset does not require authored tasks: the
			// team loads and validates with zero static task contracts.
			if len(spec.RuntimeSession().ContractTasks) != 0 {
				t.Errorf("team preset %q required authored tasks, want none", name)
			}
			for _, finding := range ValidateEffectiveTeam(spec) {
				if finding.Severity == FindingSeverityError {
					t.Errorf("team preset %q produced an error finding: %+v", name, finding)
				}
			}
			// Every agent's effective tools/side-effect must trace back to
			// the preset it was authored with — "explain shows the
			// compiled topology".
			for _, agentDef := range teamPreset.Agents {
				fileAlias := agentDef.FileName[:len(agentDef.FileName)-len(".md")]
				got, ok := spec.Agents[fileAlias]
				if !ok {
					t.Fatalf("team preset %q: compiled spec missing agent %q", name, fileAlias)
				}
				if got.Tools.Source != SourcePreset {
					t.Errorf("team preset %q agent %q: Tools.Source = %q, want %q", name, fileAlias, got.Tools.Source, SourcePreset)
				}
			}
		})
	}
}
