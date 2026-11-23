package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTeamYMLDeprecatedVars(t *testing.T) {
	t.Parallel()
	for _, manifest := range []string{
		"name: independent\ndeprecated-vars:\n  legacy.option: use --input scope=<json>\n",
		"apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: independent\nspec:\n  deprecated-vars:\n    legacy.option: use --input scope=<json>\n",
	} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "team.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := parseTeamYML(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DeprecatedVars["legacy.option"] != "use --input scope=<json>" {
			t.Fatalf("migration guidance lost: %#v", cfg.DeprecatedVars)
		}
	}
}
