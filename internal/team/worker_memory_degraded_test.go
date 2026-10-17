package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

type recallErrorWorkerMemoryService struct {
	noopWorkerMemoryService
	err error
}

func (s recallErrorWorkerMemoryService) Recall(context.Context, WorkerMemoryRecallRequest) (WorkerMemoryBundle, error) {
	return WorkerMemoryBundle{}, s.err
}

func TestWorkerMemoryCancellationStopsDirectDispatch(t *testing.T) {
	worker := &contextManifestCountingAgent{}
	c := newDirectTerminationCoordinator(t, worker)
	def := c.agentPool.(*mockAgentPool).resolveDef
	def.Memory = agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession}
	def.MemoryID = def.Name
	c.workerMemorySvc = recallErrorWorkerMemoryService{err: context.Canceled}
	result, err := c.RunDirectAgent(t.Context(), "worker", "perform direct work")
	if err != nil || result == nil || !errors.Is(result.Error, context.Canceled) {
		t.Fatalf("cancellation was not preserved: result=%+v err=%v", result, err)
	}
	if worker.calls != 0 {
		t.Fatalf("model called %d times after recall cancellation", worker.calls)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].Status == TaskDone || !isTerminalTaskStatus(items[0].Status) {
		t.Fatalf("cancelled task was not finalized: %+v", items)
	}
}

func TestWorkerMemoryDegradationSurvivesQuerySelection(t *testing.T) {
	repo := wp3SetupRepo(t)
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker-a"}
	persistent := scope
	persistent.SessionID, persistent.BranchID = "", ""
	wp3AppendItems(t, repo, contextstore.ContextItem{ID: "persistent", Kind: contextstore.ContextPattern, Scope: persistent, Content: "calibrate beacon persistent note"})
	svc := NewWorkerMemoryService(scopeFailingSearchRepo{
		Repository: repo,
		fail:       func(scope contextstore.Scope) bool { return scope.SessionID != "" },
		err:        errors.New("disk error at /secret/path"),
	}, nil)
	ctx, err := WithContextQueryEvaluationProfile(t.Context(), "candidate")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := svc.Recall(ctx, WorkerMemoryRecallRequest{
		WorkerID: "worker-a", Scope: scope, Query: "calibrate beacon",
		Policy: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent, MaxItems: 10, MaxTokens: 4000},
	})
	if err != nil || bundle.querySnapshot == nil || len(bundle.Items) != 1 {
		t.Fatalf("degraded primary was discarded: %+v err=%v", bundle, err)
	}
	for _, source := range bundle.querySnapshot.sources {
		if source.Unavailable != (source.Kind == QueryPrivateSession) {
			t.Fatalf("failed source can re-enter supplemental retrieval: %+v", source)
		}
	}
	selected := ContextQuerySelection{Sources: []ContextQuerySourceSelection{{Source: QueryPrivatePersistent, Results: []contextstore.SearchResult{{Item: bundle.Items[0].ContextItem, Score: 1}}}}}
	merged := bundle.querySnapshot.mergeSelection(selected, &bundle)
	if strings.Join(merged.Trace.Degraded, ",") != "session:main:retrieval_error" || len(merged.Items) != 1 {
		t.Fatalf("query selection lost partial results or failure attribution: %+v", merged)
	}
}
