package team

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func configureNativeMediaCost(t *testing.T, session *TeamSession, additionalModels ...string) {
	t.Helper()
	prices := map[string]cost.PriceConfig{
		"fixture/vision": {BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "2"},
		"fixture/text":   {BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "2"},
	}
	for _, model := range additionalModels {
		prices[model] = prices["fixture/vision"]
	}
	var err error
	session.CostCatalog, err = cost.NewCatalog(prices)
	if err != nil {
		t.Fatal(err)
	}
	session.Config.CostPolicy, err = cost.ResolveRunPolicy(&cost.PolicyConfig{UnknownPricePolicy: cost.UnknownPriceDeny})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeImageBudgetDenialPreventsSDKTransport(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "budget denial must precede transport", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
		configureNativeMediaCost(t, session)
		session.Config.CostPolicy.MaxRunMicros = new(int64(1))
		session.Agents["worker"].Tools = "submit_result"
		session.Agents["worker"].ResultTransport = agent.ResultTransportTool
	})
	path := filepath.Join(c.projectDir, "image.png")
	if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.RunDirectAgentWithMedia(t.Context(), "worker", "Compare the images.", MediaTurnRequest{Images: []string{path, path}})
	denial, ok := errors.AsType[*CostAdmissionError](err)
	if !ok || denial.Reason != cost.DenialBudgetExceeded || calls.Load() != 0 {
		t.Fatalf("native budget failed to deny transport: calls=%d err=%v", calls.Load(), err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	denials := 0
	for _, event := range events {
		switch EventType(event.Type) {
		case EventCostBudgetDenied:
			denials++
		case EventCostReservationCommitted, EventCostSettled:
			t.Fatal("denied image request reserved or settled a provider call")
		}
	}
	if denials != 1 || c.LastRunResult() == nil || c.LastRunResult().GoalSatisfied {
		t.Fatal("budget denial lost its durable failure or invented acceptance")
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
}

// The fake transport reports no usage. Its reservations must include every
// repeated image, while settlements retain an estimate rather than inventing
// measured tokens. Every actual SDK call owns exactly one accounting pair.
func assertNativeMediaCost(t *testing.T, events []RunEvent, calls int, auxiliaryPurposes ...cost.Purpose) {
	t.Helper()
	auxiliary := make(map[cost.Purpose]int)
	for _, purpose := range auxiliaryPurposes {
		auxiliary[purpose]++
	}
	mediaInputs := make(map[string]MediaProviderAdmittedPayload)
	mediaTasks := make(map[string]bool)
	for _, event := range events {
		if event.Type == string(EventMediaProviderAdmitted) {
			payload, err := decodeMediaProviderEvent(event)
			if err != nil {
				t.Fatal(err)
			}
			mediaInputs[payload.ProviderInvocationID] = payload
			mediaTasks[event.TaskID] = true
		}
	}
	accounting, err := costEventsFromRunEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	reservations := make(map[string]*cost.ReservationEvent)
	settlements := make(map[string]bool)
	for _, event := range accounting {
		if reservation := event.Reservation; reservation != nil {
			id := reservation.ProviderInvocationID
			if reservations[id] != nil || reservation.ReservedOutputTokens <= 0 || reservation.ReservedMicros == nil {
				t.Fatal("invalid or repeated provider cost reservation")
			}
			if image, ok := mediaInputs[id]; ok {
				if reservation.EstimatedInputTokens != image.RequestTokens || image.ImageTokens != int64(image.ImageCount)*image.ImageTokenUpperBound || image.ImageTokens <= 0 || reservation.OccurrenceAttempt != image.Attempt {
					t.Fatal("image cost lost its ordered occurrences, full request or attempt identity")
				}
			} else if reservation.Purpose != cost.PurposeCoordinator || reservation.TaskID != CoordTodoID {
				if auxiliary[reservation.Purpose] < 1 || !mediaTasks[reservation.TaskID] {
					t.Fatalf("text role did not own its expected cost reservation: purpose=%s task=%s", reservation.Purpose, reservation.TaskID)
				}
				auxiliary[reservation.Purpose]--
			}
			reservations[id] = reservation
		}
		if settlement := event.Settlement; settlement != nil {
			id := settlement.ProviderInvocationID
			if reservations[id] == nil || settlements[id] || settlement.Usage != nil || settlement.FinalMicros == nil || settlement.EstimateSource != cost.EstimateAdmissionBound {
				t.Fatal("provider settlement lost reservation ordering or fabricated measured usage")
			}
			settlements[id] = true
		}
	}
	if len(reservations) != calls || len(settlements) != calls {
		t.Fatalf("provider accounting pairs=%d/%d, calls=%d", len(reservations), len(settlements), calls)
	}
	for _, remaining := range auxiliary {
		if remaining != 0 {
			t.Fatal("expected auxiliary cost reservation missing")
		}
	}
}
