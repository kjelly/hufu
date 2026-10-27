package team

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestResultTransportAuthoringAcceptsOnlyDeclaredModes(t *testing.T) {
	for _, value := range []string{"runtime", "tool", "RUNTIME", "automatic", "''", "null", "true", "[]", "{}"} {
		for _, surface := range []string{"agent", "agent_quoted", "agent_indented", "flat", "manifest"} {
			t.Run(surface+"/"+value, func(t *testing.T) {
				wantValid := value == "runtime" || value == "tool"
				var mode agent.ResultTransport
				var err error
				if surface == "agent" || surface == "agent_quoted" || surface == "agent_indented" {
					var def *agent.AgentDef
					source := fmt.Sprintf("---\nname: worker\nrole: worker\nresult-transport: %s\n---\nPerform work.\n", value)
					if surface == "agent_quoted" {
						source = fmt.Sprintf("---\nname: worker\nrole: worker\n\"result-transport\": %s\n---\nPerform work.\n", value)
					}
					if surface == "agent_indented" {
						source = fmt.Sprintf("---\n  name: worker\n  role: worker\n  result-transport: %s\n---\nPerform work.\n", value)
					}
					def, err = parseAgentContent([]byte(source), "worker.md", nil)
					if def != nil {
						mode = def.ResultTransport
					}
				} else {
					data := "name: transport\nresult-transport: " + value + "\n"
					if surface == "manifest" {
						data = "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: transport\nspec:\n  result-transport: " + value + "\n"
					}
					dir := t.TempDir()
					if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(data), 0o600); err != nil {
						t.Fatal(err)
					}
					var config agent.TeamConfig
					config, err = parseTeamYML(dir, nil)
					mode = config.ResultTransport
				}
				if (err == nil) != wantValid || wantValid && string(mode) != value {
					t.Fatalf("mode=%q valid=%v err=%v", mode, wantValid, err)
				}
			})
		}
	}
}
