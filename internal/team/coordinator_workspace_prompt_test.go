package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestWorkerContextDistinguishesProjectRootFromControlWorkspace(t *testing.T) {
	c := &Coordinator{
		projectDir: "/project",
		session:    &TeamSession{Workspace: "/control"},
	}
	def := c.injectWorkerContext(context.Background(), &agent.AgentDef{Name: "worker", Role: "worker"})

	for _, want := range []string{"Project root (CWD): /project", "Control workspace: /control", "Modify deliverables under the project root only when the task authorizes it"} {
		if !strings.Contains(def.System, want) {
			t.Fatalf("worker context omitted %q:\n%s", want, def.System)
		}
	}
	if strings.Contains(def.System, "NEVER write outside workspace") {
		t.Fatalf("worker context retained contradictory workspace prohibition:\n%s", def.System)
	}
}
