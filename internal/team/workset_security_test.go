package team

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	runtimeTools "github.com/kjelly/hufu/internal/tools"
)

func TestWorksetArtifactAuthorizationRequiresExactCommittedReceipt(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*TodoItem, *WorksetExpansionReceipt)
		wantAccess bool
	}{
		{name: "forged binding without receipt", mutate: func(_ *TodoItem, receipt *WorksetExpansionReceipt) { *receipt = WorksetExpansionReceipt{} }},
		{name: "missing receipt", mutate: func(_ *TodoItem, _ *WorksetExpansionReceipt) {}},
		{name: "mismatched receipt", mutate: func(_ *TodoItem, receipt *WorksetExpansionReceipt) { receipt.SourceSHA256 = strings.Repeat("b", 64) }},
		{name: "wrong child receipt", mutate: func(child *TodoItem, receipt *WorksetExpansionReceipt) {
			receipt.Children[receiptKey(receipt)] = child.ID + "-other"
		}},
		{name: "exact runtime receipt", wantAccess: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, producer, child, ref, receipt := newWorksetAuthorizationFixture(t)
			if tt.mutate != nil {
				tt.mutate(child, receipt)
			}
			if tt.name != "missing receipt" {
				child.WorksetReceipt = cloneWorksetReceipt(receipt)
			}
			ctx := context.WithValue(context.Background(), todoIDKey{}, child.ID)
			got, producerID, ok := c.authorizedArtifactRef(ctx, ref.ID)
			if ok != tt.wantAccess {
				t.Fatalf("authorizedArtifactRef ok=%v, want %v (ref=%#v producer=%q)", ok, tt.wantAccess, got, producerID)
			}
			if tt.wantAccess && (got.ID != ref.ID || producerID != producer.ID) {
				t.Fatalf("authorized ref=%#v producer=%q, want ref=%q producer=%q", got, producerID, ref.ID, producer.ID)
			}
		})
	}
}

func TestLegitimateWorksetChildCanViewAssignedInputWithCommittedReceipt(t *testing.T) {
	c, _, child, ref, receipt := newWorksetAuthorizationFixture(t)
	child.WorksetReceipt = cloneWorksetReceipt(receipt)
	ctx := context.WithValue(context.Background(), todoIDKey{}, child.ID)
	ctx = runtimeTools.SetToolsAllowed(ctx, []string{"view"})
	view := runtimeTools.NewViewTool(runtimeTools.WithArtifactOpener(c.openArtifactRef))
	response, err := view.Run(ctx, fantasy.ToolCall{Input: `{"artifact_ref":"` + ref.ID + `"}`})
	if err != nil || response.IsError || !strings.Contains(response.Content, "assigned input") {
		t.Fatalf("legitimate child view response=%#v err=%v", response, err)
	}
}

func TestWorksetWorkerContextProjectsOnlyAssignedArtifactRefs(t *testing.T) {
	_, _, child, assigned, _ := newWorksetAuthorizationFixture(t)
	child.WorksetBinding.SourceArtifactID = "sha256-lineage-only-source"
	child.WorksetBinding.SourceSHA256 = strings.Repeat("s", 64)
	context := appendWorksetWorkerRuntimeContext("", child.WorksetBinding)
	if !strings.Contains(context, assigned.ID) {
		t.Fatalf("worker context omitted assigned artifact %q: %s", assigned.ID, context)
	}
	if strings.Contains(context, child.WorksetBinding.SourceArtifactID) || strings.Contains(context, child.WorksetBinding.SourceSHA256) {
		t.Fatalf("worker context exposed lineage-only source manifest: %s", context)
	}
}

func TestBoundWorksetArtifactScopeBlocksPathAliasAndProjectsOpaqueRefs(t *testing.T) {
	c, producer, child, assigned, receipt := newWorksetAuthorizationFixture(t)
	child.WorksetReceipt = cloneWorksetReceipt(receipt)
	child.DependsOn = []string{producer.ID}
	c.projectDir = c.session.Workspace
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	unassigned, err := store.Put(context.Background(), PutArtifactRequest{
		Content: []byte("unassigned artifact"), Path: "unassigned.txt", Kind: "output",
		RunID: c.executionRunID, TaskID: producer.ID, Attempt: producer.TypedResult.Attempt, Agent: producer.TypedResult.Agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	producer.TypedResult.Artifacts = append(producer.TypedResult.Artifacts, unassigned.ArtifactRef)
	independentPath := filepath.Join(c.session.Workspace, "independent.txt")
	if err := os.WriteFile(independentPath, []byte("ordinary project source"), 0o644); err != nil {
		t.Fatal(err)
	}

	scope, err := c.buildArtifactAccessScope(child.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	projected := projectDependencyResultForWorker(producer.TypedResult, child.WorksetBinding)
	prompt := FormatDependencyResults([]TaskResult{projected})
	if !strings.Contains(prompt, assigned.ID) || strings.Contains(prompt, assigned.Path) || strings.Contains(prompt, unassigned.Path) {
		t.Fatalf("dependency prompt leaked refs or paths: %s", prompt)
	}

	view := runtimeTools.NewViewTool(
		runtimeTools.WithWorkDir(c.session.Workspace),
		runtimeTools.WithAllowedPaths([]string{c.session.Workspace}),
		runtimeTools.WithArtifactOpener(c.openArtifactRef),
	)
	baseContext := context.WithValue(context.Background(), todoIDKey{}, child.ID)
	baseContext = runtimeTools.SetToolsAllowed(baseContext, []string{"view", "bash"})
	baseContext = context.WithValue(baseContext, executionAttemptKey{}, 1)
	baseContext = context.WithValue(baseContext, artifactAccessScopeKey, cloneArtifactAccessScope(scope))
	baseContext = context.WithValue(baseContext, runtimeTools.ArtifactPathPolicyKey, runtimeTools.ArtifactPathPolicy{BlockedPaths: c.artifactScopePathCandidates(scope)})
	artifactCall := func(input string) fantasy.ToolResponse {
		response, runErr := view.Run(baseContext, fantasy.ToolCall{Input: input})
		if runErr != nil {
			t.Fatalf("view %s: %v", input, runErr)
		}
		return response
	}
	if response := artifactCall(`{"artifact_ref":"` + assigned.ID + `"}`); response.IsError || !strings.Contains(response.Content, "assigned input") {
		t.Fatalf("assigned artifact_ref response=%#v", response)
	}
	for _, path := range []string{assigned.Path, unassigned.Path} {
		response := artifactCall(`{"file_path":"` + path + `"}`)
		if !response.IsError || !strings.Contains(response.Content, "opaque artifact_ref") {
			t.Fatalf("file_path %q response=%#v, want opaque-reference denial", path, response)
		}
	}
	if response := artifactCall(`{"file_path":"independent.txt"}`); response.IsError || !strings.Contains(response.Content, "ordinary project source") {
		t.Fatalf("independent project path response=%#v", response)
	}

	backingPath := filepath.Join(c.session.Workspace, logsDir, "artifacts", "data", assigned.ID)
	dataRoot := filepath.Dir(backingPath)
	metaRoot := filepath.Join(c.session.Workspace, logsDir, "artifacts", "meta")
	dataAlias := filepath.Join(c.session.Workspace, "artifact-data-alias")
	if err := os.Symlink(dataRoot, dataAlias); err != nil {
		t.Fatal(err)
	}
	metaAlias := filepath.Join(c.session.Workspace, "artifact-meta-alias")
	if err := os.Symlink(metaRoot, metaAlias); err != nil {
		t.Fatal(err)
	}
	relativeBase := filepath.Dir(c.session.Workspace)
	relativeWorkspace := filepath.Base(c.session.Workspace)
	bash := runtimeTools.NewBashTool(
		runtimeTools.WithWorkDir(relativeBase),
		runtimeTools.WithAllowedPaths([]string{relativeBase}),
	)
	for _, testCase := range []struct {
		path    string
		command string
	}{
		{path: filepath.Join(relativeWorkspace, logsDir, "artifacts", "data"), command: "ls "},
		{path: filepath.Join(relativeWorkspace, logsDir, "artifacts", "data", assigned.ID), command: "cat "},
		{path: filepath.Join(relativeWorkspace, "artifact-data-alias"), command: "ls "},
		{path: filepath.Join(relativeWorkspace, "artifact-meta-alias"), command: "ls "},
	} {
		path := testCase.path
		response, runErr := bash.Run(baseContext, fantasy.ToolCall{Input: `{"command":"` + testCase.command + path + `"}`})
		if runErr != nil || !response.IsError || !strings.Contains(response.Content, "opaque artifact_ref") {
			t.Fatalf("relative shell path %q response=%#v err=%v, want opaque-reference denial", path, response, runErr)
		}
	}

	for _, path := range []string{
		filepath.Join(relativeWorkspace, logsDir, "artifacts", "meta", assigned.ID+".json"),
	} {
		response, runErr := bash.Run(baseContext, fantasy.ToolCall{Input: `{"command":"cat ` + path + `"}`})
		if runErr != nil || !response.IsError || !strings.Contains(response.Content, "opaque artifact_ref") {
			t.Fatalf("relative shell path %q response=%#v err=%v, want opaque-reference denial", path, response, runErr)
		}
	}

	unbound := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "unbound", Agent: "reviewer", DependsOn: []string{producer.ID}}})[0]
	unboundScope, err := c.buildArtifactAccessScope(unbound.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	unboundContext := context.WithValue(context.Background(), todoIDKey{}, unbound.ID)
	unboundContext = runtimeTools.SetToolsAllowed(unboundContext, []string{"view"})
	unboundContext = context.WithValue(unboundContext, executionAttemptKey{}, 1)
	unboundContext = context.WithValue(unboundContext, artifactAccessScopeKey, cloneArtifactAccessScope(unboundScope))
	for _, ref := range []ArtifactRef{assigned, unassigned.ArtifactRef} {
		response, runErr := view.Run(unboundContext, fantasy.ToolCall{Input: `{"artifact_ref":"` + ref.ID + `"}`})
		if runErr != nil || response.IsError {
			t.Fatalf("unbound dependency artifact %q response=%#v err=%v", ref.ID, response, runErr)
		}
	}
}

func TestBoundWorksetGatedRecursiveToolsHideArtifactDescendants(t *testing.T) {
	c, _, child, assigned, receipt := newWorksetAuthorizationFixture(t)
	child.WorksetReceipt = cloneWorksetReceipt(receipt)
	scope, err := c.buildArtifactAccessScope(child.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	workspace := c.session.Workspace
	ordinaryPath := filepath.Join(workspace, "ordinary-source.go")
	ordinaryContent := "ordinary-source-unique-content"
	if err := os.WriteFile(ordinaryPath, []byte(ordinaryContent), 0o644); err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(workspace, logsDir, "artifacts", "data", assigned.ID)
	metaPath := filepath.Join(workspace, logsDir, "artifacts", "meta", assigned.ID+".json")
	dataAlias := filepath.Join(workspace, "blocked-data-file-alias")
	if err := os.Symlink(dataPath, dataAlias); err != nil {
		t.Fatal(err)
	}
	dirAlias := filepath.Join(workspace, "blocked-data-dir-alias")
	if err := os.Symlink(filepath.Dir(dataPath), dirAlias); err != nil {
		t.Fatal(err)
	}
	metaAlias := filepath.Join(workspace, "blocked-meta-dir-alias")
	if err := os.Symlink(filepath.Dir(metaPath), metaAlias); err != nil {
		t.Fatal(err)
	}

	policy := runtimeTools.ArtifactPathPolicy{BlockedPaths: c.artifactScopePathCandidates(scope), FailClosedForUnsupported: true}
	ctx := context.WithValue(context.Background(), todoIDKey{}, child.ID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, 1)
	ctx = context.WithValue(ctx, artifactAccessScopeKey, cloneArtifactAccessScope(scope))
	ctx = context.WithValue(ctx, runtimeTools.ArtifactPathPolicyKey, policy)
	ctx = runtimeTools.SetToolsAllowed(ctx, []string{"view", "ls", "glob", "grep"})

	ls := runtimeTools.NewLsTool(runtimeTools.WithWorkDir(workspace), runtimeTools.WithAllowedPaths([]string{workspace}))
	glob := runtimeTools.NewGlobTool(runtimeTools.WithWorkDir(workspace), runtimeTools.WithAllowedPaths([]string{workspace}))
	grep := runtimeTools.NewGrepTool(runtimeTools.WithWorkDir(workspace), runtimeTools.WithAllowedPaths([]string{workspace}))
	view := runtimeTools.NewViewTool(runtimeTools.WithWorkDir(workspace), runtimeTools.WithAllowedPaths([]string{workspace}), runtimeTools.WithArtifactOpener(c.openArtifactRef))
	gated := map[string]fantasy.AgentTool{}
	for _, tool := range c.gatePolicyTools([]fantasy.AgentTool{ls, glob, grep, view}) {
		gated[tool.Info().Name] = tool
	}

	for _, tc := range []struct {
		name  string
		input string
	}{
		{"ls-omitted-root", `{"depth":5}`},
		{"ls-explicit-root", `{"path":"` + workspace + `","depth":5}`},
		{"glob-omitted-root", `{"pattern":"**/*"}`},
		{"glob-explicit-root", `{"pattern":"**/*","path":"` + workspace + `"}`},
	} {
		toolName := strings.Split(tc.name, "-")[0]
		response, runErr := gated[toolName].Run(ctx, fantasy.ToolCall{Name: toolName, Input: tc.input})
		if runErr != nil || response.IsError {
			t.Fatalf("%s response=%#v err=%v", tc.name, response, runErr)
		}
		if !strings.Contains(response.Content, "ordinary-source.go") || strings.Contains(response.Content, assigned.ID) || strings.Contains(response.Content, "blocked-data-file-alias") || strings.Contains(response.Content, "blocked-data-dir-alias") || strings.Contains(response.Content, "blocked-meta-dir-alias") {
			t.Fatalf("%s leaked or missed entries: %q", tc.name, response.Content)
		}
	}

	for _, tc := range []struct {
		name  string
		input string
	}{
		{"grep-omitted-root", `{"pattern":"` + assigned.ID + `"}`},
		{"grep-explicit-root", `{"pattern":"` + assigned.ID + `","path":"` + workspace + `"}`},
		{"grep-content-omitted-root", `{"pattern":"assigned input"}`},
		{"grep-content-explicit-root", `{"pattern":"assigned input","path":"` + workspace + `"}`},
	} {
		response, runErr := gated["grep"].Run(ctx, fantasy.ToolCall{Name: "grep", Input: tc.input})
		if runErr != nil || response.IsError || strings.Contains(response.Content, assigned.ID) || strings.Contains(response.Content, "assigned input") {
			t.Fatalf("%s leaked blocked grep result: response=%#v err=%v", tc.name, response, runErr)
		}
	}
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"ls-direct-blocked", `{"path":"` + filepath.Dir(dataPath) + `"}`},
		{"glob-direct-blocked", `{"pattern":"*","path":"` + filepath.Dir(dataPath) + `"}`},
		{"grep-direct-blocked", `{"pattern":"assigned input","path":"` + filepath.Dir(dataPath) + `"}`},
	} {
		toolName := strings.Split(tc.name, "-")[0]
		response, runErr := gated[toolName].Run(ctx, fantasy.ToolCall{Name: toolName, Input: tc.input})
		if runErr != nil || !response.IsError || !strings.Contains(response.Content, "opaque artifact_ref") {
			t.Fatalf("%s response=%#v err=%v, want direct blocked denial", tc.name, response, runErr)
		}
	}

	response, runErr := gated["view"].Run(ctx, fantasy.ToolCall{Name: "view", Input: `{"artifact_ref":"` + assigned.ID + `"}`})
	if runErr != nil || response.IsError || !strings.Contains(response.Content, "assigned input") {
		t.Fatalf("assigned opaque ref response=%#v err=%v", response, runErr)
	}
}

func TestBoundWorksetChildCannotViewUnboundDependencyArtifact(t *testing.T) {
	c, producer, child, _, receipt := newWorksetAuthorizationFixture(t)
	child.WorksetReceipt = cloneWorksetReceipt(receipt)
	child.DependsOn = []string{producer.ID}

	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	unbound, err := store.Put(context.Background(), PutArtifactRequest{
		Content: []byte("unbound artifact"), Path: "unbound.txt", Kind: "output",
		RunID: c.executionRunID, TaskID: producer.ID, Attempt: producer.TypedResult.Attempt, Agent: producer.TypedResult.Agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	producer.TypedResult.Artifacts = append(producer.TypedResult.Artifacts, unbound.ArtifactRef)

	ctx := context.WithValue(context.Background(), todoIDKey{}, child.ID)
	ctx = runtimeTools.SetToolsAllowed(ctx, []string{"view"})
	view := runtimeTools.NewViewTool(runtimeTools.WithArtifactOpener(c.openArtifactRef))
	response, err := view.Run(ctx, fantasy.ToolCall{Input: `{"artifact_ref":"` + unbound.ID + `"}`})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "not authorized") {
		t.Fatalf("unbound dependency view response=%#v err=%v, want denial", response, err)
	}
}

func TestBoundWorksetStructuredRunnerDeniesShellToolsBeforeExecution(t *testing.T) {
	for _, name := range []string{"bash", "sudo", "wait_for", "view", "grep", "ls"} {
		t.Run(name, func(t *testing.T) {
			c, _, child, _, receipt := newWorksetAuthorizationFixture(t)
			child.WorksetReceipt = cloneWorksetReceipt(receipt)
			ran := false
			tool := &structuredTestTool{name: name, run: func(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				ran = true
				return fantasy.NewTextResponse("unexpected execution"), nil
			}}
			c.session.Agents = map[string]*agent.AgentDef{
				"reviewer": {Name: "reviewer", Tools: name},
			}
			c.coreTools = []fantasy.AgentTool{tool}
			result, err := (&coordinatorDeclaredToolRunner{c: c}).RunStructuredStep(context.Background(), StructuredStepRequest{
				TaskID: child.ID, Attempt: 1,
				Step:          ExecutionStep{ID: "inspect", Tool: name},
				ResolvedInput: map[string]any{"command": `artifact=logs/artifacts/data/sibling; cat "$artifact"`},
			})
			if err != nil || result.ExitCode == 0 || ran {
				t.Fatalf("structured shell step result=%#v err=%v ran=%v, want pre-execution denial", result, err, ran)
			}
		})
	}
}

func TestWorksetArtifactAuthorizationRejectsCommittedPriorRun(t *testing.T) {
	c, producer, child, ref, receipt := newWorksetAuthorizationFixture(t)
	child.WorksetReceipt = cloneWorksetReceipt(receipt)
	child.DependsOn = []string{producer.ID}
	c.executionRunID = "run-2"
	ctx := context.WithValue(context.Background(), todoIDKey{}, child.ID)
	ctx = runtimeTools.SetToolsAllowed(ctx, []string{"view"})
	view := runtimeTools.NewViewTool(runtimeTools.WithArtifactOpener(c.openArtifactRef))
	response, err := view.Run(ctx, fantasy.ToolCall{Input: `{"artifact_ref":"` + ref.ID + `"}`})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "not authorized") {
		t.Fatalf("prior-run child view response=%#v err=%v, want denial", response, err)
	}
}

func newWorksetAuthorizationFixture(t *testing.T) (*Coordinator, *TodoItem, *TodoItem, ArtifactRef, *WorksetExpansionReceipt) {
	t.Helper()
	workspace := t.TempDir()
	tracker := NewTaskTracker()
	producer := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "producer", Agent: "producer"}})[0]
	child := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "child", Agent: "reviewer"}})[0]
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), PutArtifactRequest{
		Content: []byte("assigned input"), Path: "input.txt", Kind: "input",
		RunID: "run-1", TaskID: producer.ID, Attempt: 1, Agent: "producer",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := &TaskResult{TaskID: producer.ID, Attempt: 1, Agent: "producer", Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{ref.ArtifactRef}}
	producer.TypedResult = result
	producer.Status = TaskDone
	binding := &WorksetBinding{
		WorksetID: "workset-1", ParentTaskID: "fanout", ItemKey: "one",
		Inputs: []ArtifactRef{ref.ArtifactRef}, SourceArtifactID: "manifest-1", SourceSHA256: strings.Repeat("a", 64),
		SourceArtifact: ArtifactRef{ID: "manifest-1", SHA256: strings.Repeat("a", 64), RunID: "run-1", TaskID: producer.ID, Attempt: 1, Agent: producer.Agent},
	}
	child.WorksetBinding = binding
	receipt := &WorksetExpansionReceipt{
		WorksetID: "workset-1", RunID: "run-1", ParentTaskID: "fanout",
		SourceArtifactID: "manifest-1", SourceSHA256: strings.Repeat("a", 64),
		SourceArtifact: binding.SourceArtifact,
		ItemCount:      1, Children: map[string]string{"one": child.ID},
	}
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace}, executionRunID: "run-1",
		taskTracker: tracker, taskResults: map[string]*TaskResult{producer.ID: result},
	}
	return c, producer, child, ref.ArtifactRef, receipt
}

func receiptKey(receipt *WorksetExpansionReceipt) string {
	for key := range receipt.Children {
		return key
	}
	return ""
}

func TestWorksetAuthorizationUsesStoredBytesAfterExactReceipt(t *testing.T) {
	c, _, child, ref, receipt := newWorksetAuthorizationFixture(t)
	child.WorksetReceipt = cloneWorksetReceipt(receipt)
	ctx := context.WithValue(context.Background(), todoIDKey{}, child.ID)
	reader, err := c.openArtifactRef(ctx, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(data) != "assigned input" {
		t.Fatalf("opened data=%q err=%v", data, err)
	}
}
