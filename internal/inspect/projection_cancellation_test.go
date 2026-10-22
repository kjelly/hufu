package inspect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/team"
)

func TestObservationProjectionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := projectWorkerAttemptsContext(ctx, nil, []IndexedEvent{{}}, time.Time{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("worker projection ignored cancellation: %v", err)
	}
	todo := &team.TodoItem{ExecutionReceipts: []team.ExecutionReceipt{{}}}
	if _, err := routeAttemptsContext(ctx, todo, RouteTask{}, nil, nil, "run"); !errors.Is(err, context.Canceled) {
		t.Fatalf("receipt matching ignored cancellation: %v", err)
	}
	ledger := cost.NewProjection()
	ledger.Invocations["one"] = cost.InvocationProjection{}
	if err := economicsAttributionContext(ctx, &EconomicsData{}, RoutesData{}, ledger); !errors.Is(err, context.Canceled) {
		t.Fatalf("cost attribution ignored cancellation: %v", err)
	}
}
