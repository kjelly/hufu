package team

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/cost"
)

func TestCodexNativeImagesUseOpaqueCostReservation(t *testing.T) {
	for _, scenario := range []string{"bounded", "budget denied", "unbounded"} {
		t.Run(scenario, func(t *testing.T) {
			data := mediaStorePNG(t)
			digest := sha256.Sum256(data)
			hash := hex.EncodeToString(digest[:])
			c, logPath := newCodexImageCoordinator(t, codexImageProtocolVersion, func(root string) []fakeCodexStep {
				return codexImageSteps(t, root, validProposalJSON(""), []string{hash})
			}, func(session *TeamSession) {
				bound, budget := "0.75", "2"
				if scenario == "budget denied" {
					budget = "0.01"
				}
				if scenario == "unbounded" {
					bound = ""
				}
				var err error
				session.CostCatalog, err = cost.NewCatalog(map[string]cost.PriceConfig{
					"fixture/vision":    {BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "1"},
					"codex/gpt-5-codex": {BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "1", OpaqueMaxUSDPerInvocation: bound},
				})
				if err != nil {
					t.Fatal(err)
				}
				session.Config.CostPolicy, err = cost.ResolveRunPolicy(&cost.PolicyConfig{MaxRunUSD: budget, UnknownPricePolicy: cost.UnknownPriceDeny})
				if err != nil {
					t.Fatal(err)
				}
			})
			path := filepath.Join(c.projectDir, "input.png")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, runErr := c.RunTaskWithMedia(t.Context(), TaskDef{Agent: "worker", Goal: "Inspect image.", Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{path}})
			items := c.taskTracker.TodoList().Items()
			for _, item := range items {
				if item.Status != TaskDone {
					t.Logf("task status=%s detail=%s failure=%+v", item.Status, item.Detail, item.FailureEvent)
				}
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			costs, err := costEventsFromRunEvents(events)
			if err != nil {
				t.Fatal(err)
			}
			var invocation string
			settled, denied := false, false
			for _, event := range costs {
				if event.Reservation != nil {
					invocation = event.Reservation.ProviderInvocationID
				}
				if event.Settlement != nil {
					settled = true
					if event.Settlement.FinalMicros == nil || *event.Settlement.FinalMicros != 750_000 || event.Settlement.Usage != nil || event.Settlement.EstimateSource != cost.EstimateAdmissionBound {
						t.Fatalf("unknown usage was treated as measured/free: %+v", event.Settlement)
					}
				}
				if event.Kind == cost.EventBudgetDenied {
					denied = true
				}
			}
			if scenario != "bounded" {
				if !denied || settled || len(codexImageWireCalls(t, logPath)) != 0 {
					t.Fatalf("economic denial reached provider: denied=%t settled=%t", denied, settled)
				}
				return
			}
			if len(items) != 1 || items[0].Status != TaskDone || !settled || invocation == "" {
				t.Fatalf("bounded Codex failed: items=%+v run=%v", items, runErr)
			}
			matched := false
			for _, event := range events {
				if event.Type == string(EventMediaProviderAdmitted) {
					payload, err := decodeMediaProviderEvent(event)
					if err != nil {
						t.Fatal(err)
					}
					if payload.ProviderInvocationID != invocation {
						t.Fatal("media admission detached from opaque cost reservation")
					}
					matched = true
				}
			}
			if !matched {
				t.Fatal("missing native Codex image admission")
			}
		})
	}
}
