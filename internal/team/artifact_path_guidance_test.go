package team

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDAGWorkerRuntimeContextTreatsRuntimeArtifactsAsOutputStaging(t *testing.T) {
	root := filepath.Join("workspace", "runtime")
	context := formatRuntimeWorkflowContext(PhaseExecute, ExecutionContext{
		RuntimeWorkspace: RuntimeWorkspace{Root: root},
	})
	for _, fragment := range []string{
		"runtime/artifacts",
		"output staging only",
		"not an opaque artifact-ID lookup directory",
		"Pass each issued ID unchanged as `view.artifact_ref`",
		"canonical artifact store under `workspace/logs/artifacts/{data,meta}`",
	} {
		if !strings.Contains(context, fragment) {
			t.Errorf("DAG worker context missing artifact guidance %q: %s", fragment, context)
		}
	}
}

func TestDirectAgentRuntimeContextPreservesOpaqueArtifactRefs(t *testing.T) {
	c := newDirectTypedCoordinator(t, "", nil, nil)
	c.phaseWorkflow = &runtimeWorkflow{
		enabled:   true,
		state:     PhaseExecute,
		workspace: RuntimeWorkspace{Root: filepath.Join(c.session.Workspace, "runtime")},
	}
	prompt := c.directAgentWorkflowPrompt("perform work", workerAgentDef(), "worker", "todo-1", map[string]bool{}, TaskDef{})
	for _, fragment := range []string{
		"output staging only",
		"not an opaque artifact-ID lookup directory",
		"Pass each issued ID unchanged as `view.artifact_ref`",
	} {
		if !strings.Contains(prompt, fragment) {
			t.Errorf("direct-agent prompt missing artifact guidance %q: %s", fragment, prompt)
		}
	}
	if strings.Contains(prompt, "Ensure all durable outputs are written to the artifacts directory") {
		t.Fatal("direct-agent prompt still presents runtime artifacts as the durable artifact store")
	}
}
