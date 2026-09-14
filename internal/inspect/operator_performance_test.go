package inspect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

// TestOperatorOverviewPerformance is an opt-in measurement harness, not a
// machine-independent pass/fail assertion. Release evidence records cold and
// warm p95 separately; CI correctness tests enforce bounded data surfaces.
func TestOperatorOverviewPerformance(t *testing.T) {
	if os.Getenv("HUFU_OPERATOR_PERF") != "1" {
		t.Skip("set HUFU_OPERATOR_PERF=1 to measure operator overview latency")
	}
	for _, taskCount := range []int{25, 1000} {
		t.Run(fmt.Sprintf("tasks-%d", taskCount), func(t *testing.T) {
			workspace := buildOperatorPerformanceFixture(t, taskCount)
			query := InspectQuery{Workspace: workspace, RunID: "run-performance"}
			coldStarted := time.Now()
			if _, err := InspectOverview(t.Context(), query); err != nil {
				t.Fatal(err)
			}
			cold := time.Since(coldStarted)
			warm := make([]time.Duration, 30)
			for index := range warm {
				started := time.Now()
				if _, err := InspectOverview(t.Context(), query); err != nil {
					t.Fatal(err)
				}
				warm[index] = time.Since(started)
			}
			slices.Sort(warm)
			p95 := warm[(len(warm)*95+99)/100-1]
			t.Logf("events=%d context_items=%d cold=%s warm_p95=%s samples=%d", taskCount+1, taskCount, cold, p95, len(warm))
		})
	}
}

func buildOperatorPerformanceFixture(t *testing.T, taskCount int) string {
	t.Helper()
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-performance", "session-performance")
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event team.RunEvent) {
		t.Helper()
		if _, appendErr := store.AppendPersisted(event); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	appendEvent(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: operatorPerformanceJSON(t, map[string]any{"goal": "measure overview"})})
	for index := range taskCount {
		id := fmt.Sprintf("task-%04d", index)
		appendEvent(team.RunEvent{Type: "task_created", Actor: "coordinator", TaskID: id, Payload: operatorPerformanceJSON(t, map[string]any{"id": id, "status": team.TaskPending, "agent": "worker"})})
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	buildContextPerformanceFixture(t, workspace, taskCount)
	return workspace
}

func buildContextPerformanceFixture(t *testing.T, workspace string, itemCount int) {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	items := make([]contextstore.ContextItem, itemCount)
	observations := make([]contextstore.ExperienceObservation, itemCount)
	for index := range itemCount {
		id := fmt.Sprintf("context-%04d", index)
		items[index] = contextstore.ContextItem{
			ID: id, Kind: contextstore.ContextPattern, Content: "performance item " + id,
			Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Lifecycle: contextstore.LifecycleConfirmed,
		}
		observations[index] = contextstore.ExperienceObservation{
			IdempotencyKey: "observation-" + id, ContextItemID: id, PolicyVersion: "policy-performance",
			ProjectID: "project", TaskID: fmt.Sprintf("task-%04d", index), ObservedAt: now,
			ExposureDelta: 1, ConsultedDelta: 1,
		}
	}
	if err = repo.Append(t.Context(), items...); err != nil {
		t.Fatal(err)
	}
	if err = repo.RebuildExperienceAggregates(t.Context(), observations); err != nil {
		t.Fatal(err)
	}
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	policy.PolicyVersion = "policy-performance"
	snapshot := operatorPerformanceJSON(t, map[string]any{"learning": policy})
	if err = repo.SaveMemoryPolicyVersion(t.Context(), policy.PolicyVersion, snapshot, "revision-performance", "active", now); err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
}

func operatorPerformanceJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
