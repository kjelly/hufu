package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/tools"
)

func TestHufuCodeReviewCriticSubmitsDirectly(t *testing.T) {
	for _, id := range []string{"critic-review", "review-documentation-escalation"} {
		t.Run(id, func(t *testing.T) {
			loaded := loadHufuCodeReviewTeam(t)
			c := newTestCoordinator(t)
			c.session.Config.ResultTransport = loaded.Config.ResultTransport
			c.session.Config.CollectorResponseChannel = loaded.Config.CollectorResponseChannel
			c.session.Config.DefaultLLMBackend = execution.OllamaBackendName
			c.session.Config.DecisionPrimitives = loaded.Config.DecisionPrimitives
			c.session.Config.Delegation.BindTaskGoalContracts = true
			critic := *loaded.Agents["critic"]
			critic.ExecutionRoute, critic.Generation.Model = "", "ollama/test"
			c.session.Agents[critic.Name] = &critic
			c.session.ResultContracts = loaded.ResultContracts
			c.acceptanceSpec = loaded.Config.AcceptanceSpec
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			c.projectDir = t.TempDir()
			if err := c.session.SetCompatibilityWorkspaceScope(c.projectDir); err != nil {
				t.Fatal(err)
			}
			// A collector must be unnecessary even when a provider route exists.
			var err error
			c.providerManager, err = agent.NewProviderManager("http://127.0.0.1:1/v1", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			index := slices.IndexFunc(loaded.ContractTasks, func(task TaskDef) bool { return task.ID == id })
			if index < 0 {
				t.Fatalf("missing contract %s", id)
			}
			task := loaded.ContractTasks[index]
			var inputID string
			payload := `{"record":{"kind":"critique","decisions":{}}}`
			request := TaskDef{Agent: critic.Name, ContractID: id, Goal: "Complete the assigned review", Model: "ollama/test"}
			if id == "critic-review" {
				request.EvidenceFrom = []string{seedEmptyCriticReview(t, c)}
			} else {
				inputID = seedEscalationWorkset(t, c, &task)
				payload = `{"record":{"kind":"review","findings":[],"gaps":[],"invariant_assessments":[],"reference_assessments":[]}}`
			}
			c.session.ContractTasks = []TaskDef{task}
			c.executionPolicy, err = newExecutionPolicyState(c)
			if err != nil {
				t.Fatal(err)
			}

			worker := scriptWorker(scriptedTurn{stream: func(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
				ctx = tools.SetToolsAllowed(ctx, []string{"view", submitResultToolName})
				todoID, _ := ctx.Value(todoIDKey{}).(string)
				if inputID != "" {
					view := c.gatePolicyTools([]fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))})[0]
					response, err := view.Run(ctx, fantasy.ToolCall{ID: "read-immutable-doc", Name: "view", Input: fmt.Sprintf(`{"artifact_ref":%q,"byte_limit":1024}`, inputID)})
					if err != nil || response.IsError {
						return nil, fmt.Errorf("read assigned artifact: %s (%v)", response.Content, err)
					}
				}
				response, err := (&submitResultTool{coordinator: c, todoID: todoID}).Run(ctx, fantasy.ToolCall{
					ID: "direct-review-result", Name: submitResultToolName,
					Input: `{"status":"success","summary":"review complete","structured_payload":` + payload + `}`,
				})
				if err != nil || response.IsError {
					return nil, fmt.Errorf("submit directly: %s (%v)", response.Content, err)
				}
				return textAgentResult("submitted"), nil
			}})
			c.workerAgentOverride = worker
			if _, err := c.ExecuteTasks(withTestProtocolRepairInvocationContext(t.Context()), []TaskDef{request}); err != nil {
				items := c.taskTracker.TodoList().Items()
				last := items[len(items)-1]
				t.Fatalf("execute %s: %v (status=%s, detail=%s, output=%s)", id, err, last.Status, last.Detail, last.Output)
			}
			items := c.taskTracker.TodoList().Items()
			item := items[len(items)-1]
			if item.Status != TaskDone || item.TypedResult == nil || item.TypedResult.Source != "submitted" ||
				item.VerifyResult == nil || item.VerifyResult.ExitCode != 0 || currentResultCollection(item) != nil || worker.Calls() != 1 {
				t.Fatalf("direct transport did not finish: status=%s calls=%d", item.Status, worker.Calls())
			}
			var stored map[string]any
			if err := json.Unmarshal(item.TypedResult.StructuredPayload.Value, &stored); err != nil {
				t.Fatal(err)
			}
			if id == "critic-review" && stored["audit_complete"] != true {
				t.Fatal("accepted critique lacks runtime completion")
			}
			if inputID != "" && !slices.ContainsFunc(item.TypedResult.FilesRead, func(ref FileRef) bool { return ref.Path == inputID }) {
				t.Fatal("documentation result lacks the observed native artifact")
			}
		})
	}
}

func seedEmptyCriticReview(t *testing.T, c *Coordinator) string {
	t.Helper()
	schema := c.session.ResultContracts["schemas/review-v1.json"]
	ref := schema.ref(true)
	payload, err := validateStructuredResultPayload(schema, ref, []byte(`{"record":{"kind":"review","review_revision":"frozen","workset_key":"unit-0000","lens":"general","findings":[],"gaps":[],"invariant_assessments":[],"reference_assessments":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	source := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", ContractID: "review-primary-workset", ResultContract: &ref, WorksetBinding: &WorksetBinding{Bindings: map[string]string{"reference_candidate_ids": "[]"}}}})[0]
	c.storeSubmittedTaskResult(source.ID, &TaskResult{TaskID: source.ID, Agent: source.Agent, Status: TaskResultStatusSuccess, Source: "submitted", StructuredPayload: payload})
	c.taskTracker.TodoList().UpdateStatus(source.ID, TaskDone, "reviewed")
	return source.ID
}

func seedEscalationWorkset(t *testing.T, c *Coordinator, task *TaskDef) string {
	t.Helper()
	producer := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", PlanTaskID: "seed"}})[0]
	store, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	request := PutArtifactRequest{Content: []byte("No documentation changes in this workset.\n"), Path: "docs.diff", Kind: "diff",
		RunID: c.executionRunID, TaskID: producer.ID, Agent: producer.Agent, Attempt: 1}
	input, err := store.Put(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{{
		Key:      "unit-0000",
		Bindings: map[string]string{"key": "unit-0000", "review_revision": "frozen", "lens": "noop", "reference_candidate_ids": "[]"},
		Inputs:   []ArtifactRef{input.ArtifactRef},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	request.Content, request.Kind, request.Path = manifest, "workset_manifest", "escalation.json"
	declared, err := store.Put(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	c.storeSubmittedTaskResult(producer.ID, &TaskResult{TaskID: producer.ID, Agent: producer.Agent, Attempt: 1, Status: TaskResultStatusSuccess, Source: "runtime", Artifacts: []ArtifactRef{input.ArtifactRef, declared.ArtifactRef}})
	c.taskTracker.TodoList().UpdateStatus(producer.ID, TaskDone, "produced")
	task.FanOut = &FanOutSpec{SourceArtifact: FactRef{TaskID: "seed", Artifact: declared.ID}, GoalTemplate: task.FanOut.GoalTemplate}
	return input.ID
}
