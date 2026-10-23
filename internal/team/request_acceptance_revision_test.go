package team

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
)

func documentAnalysisExtraction(request, extent, source string) []byte {
	raw, _ := json.Marshal(map[string]any{"requirements": []any{
		map[string]any{"statement": "Read the actual relevant contents of the two documents", "source_quote": request, "required": true, "verification": "observed",
			"delivery": RequestDelivery{Target: "The two source documents", Extent: extent, Source: source, AllowedSubstitutes: []RequestDeliverySubstitute{}}},
		map[string]any{"statement": "Explain whether their reasoning is consistent", "source_quote": request, "required": true, "verification": "response",
			"delivery": RequestDelivery{Target: "Comparison of the documents' reasoning", Extent: "not_applicable", Source: "any", AllowedSubstitutes: []RequestDeliverySubstitute{}}},
	}})
	return raw
}

func beginRequestRevisionFixture(t *testing.T, c *Coordinator) context.Context {
	t.Helper()
	ctx, end := c.beginInvocationExecutionRun(t.Context())
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestRequestAcceptanceRevisesInventedRestrictionsBeforeFreezing(t *testing.T) {
	c := requestAcceptanceFixture(t)
	const request = "Compare the reasoning in the two source documents"
	extractions, audits := 0, 0
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
		if !strings.Contains(prompt, request) || c.RunInputSnapshot() != nil || len(c.taskTracker.TodoList().Items()) != 0 {
			t.Fatal("candidate lost the original request or was frozen/dispatched before approval")
		}
		if operation == "extract" {
			extractions++
			if extractions == 1 {
				return documentAnalysisExtraction(request, "full", "primary"), nil
			}
			if extractions != 2 || !strings.Contains(prompt, "invented full/primary restrictions") || !strings.Contains(prompt, `"extent":"full"`) || !strings.Contains(prompt, "untrusted data") {
				t.Fatal("revision omitted the rejected candidate or independent feedback")
			}
			return documentAnalysisExtraction(request, "relevant_parts", "any"), nil
		}
		if operation != "contract" {
			t.Fatalf("unexpected operation %q", operation)
		}
		audits++
		if audits == 1 {
			return []byte(`{"faithful":false,"reason":"The candidate invented full/primary restrictions; retain actual document contents and the requested analysis."}`), nil
		}
		if audits != 2 || strings.Contains(prompt, `"extent":"full"`) || !strings.Contains(prompt, `"verification":"observed"`) || !strings.Contains(prompt, `"verification":"response"`) {
			t.Fatal("revised contract omitted material access or retained the invented restriction")
		}
		return faithfulRequestContract(), nil
	})))
	ctx := beginRequestRevisionFixture(t, c)
	if err := c.resolveRunInputsForInvocation(ctx, request); err != nil {
		t.Fatal(err)
	}
	snapshot := c.RunInputSnapshot()
	contract, err := RequestAcceptanceContractFromSnapshot(snapshot)
	if err != nil || extractions != 2 || audits != 2 || len(contract.Requirements) != 2 || contract.Requirements[0].Verification != "observed" || contract.Requirements[0].Delivery.Extent != "relevant_parts" || contract.Requirements[0].Delivery.Source != "any" {
		t.Fatalf("revised contract=%#v extracts=%d audits=%d error=%v", contract, extractions, audits, err)
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resolved, reviewed := 0, 0
	var rejectedHash string
	for _, event := range events {
		if event.Type == string(EventRunInputsResolved) {
			resolved++
		}
		if event.Type != "request_acceptance_contract_reviewed" {
			continue
		}
		var review struct {
			ContractHash string `json:"contract_hash"`
			Faithful     bool   `json:"faithful"`
		}
		if err := json.Unmarshal(event.Payload, &review); err != nil {
			t.Fatal(err)
		}
		reviewed++
		if reviewed == 1 {
			if review.Faithful {
				t.Fatal("original rejected candidate was approved")
			}
			rejectedHash = review.ContractHash
		} else if !review.Faithful || review.ContractHash == rejectedHash || review.ContractHash != runInputHash(snapshot.Inputs[0].CanonicalValue) {
			t.Fatal("audit approval did not bind the revised candidate")
		}
	}
	replayed := ReduceToSessionData(events)
	if resolved != 1 || reviewed != 2 || len(replayed.RunInputSnapshots) != 1 || replayed.RunInputSnapshots[0].SnapshotHash != snapshot.SnapshotHash {
		t.Fatalf("rejected candidate leaked into durable invocation inputs: resolved=%d reviewed=%d", resolved, reviewed)
	}
}

func TestRequestAcceptanceCandidateRevisionIsBounded(t *testing.T) {
	for _, scenario := range []string{"unchanged", "still rejected", "drops material", "shared validation budget"} {
		t.Run(scenario, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			const request = "Compare the two source documents"
			extractions, audits := 0, 0
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
				if operation == "extract" {
					extractions++
					if scenario == "shared validation budget" && extractions == 1 {
						return []byte(`{"requirements":[]}`), nil
					}
					if extractions == 2 && scenario == "drops material" {
						return requestExtraction(request, request, "response"), nil
					}
					extent := "full"
					if extractions == 2 && scenario == "still rejected" {
						extent = "relevant_parts"
					}
					return documentAnalysisExtraction(request, extent, "primary"), nil
				}
				audits++
				// If the same candidate were re-audited, this model would
				// opportunistically approve it. The runtime must prevent that.
				if scenario == "unchanged" && audits > 1 {
					return faithfulRequestContract(), nil
				}
				return []byte(`{"faithful":false,"reason":"The candidate still invents restrictions or omits actual source access."}`), nil
			})))
			ctx := beginRequestRevisionFixture(t, c)
			err := c.resolveRunInputsForInvocation(ctx, request)
			wantAudits := 2
			if scenario == "unchanged" || scenario == "shared validation budget" {
				wantAudits = 1
			}
			if err == nil || extractions != 2 || audits != wantAudits || c.RunInputSnapshot() != nil || len(c.taskTracker.TodoList().Items()) != 0 {
				t.Fatalf("revision escaped its budget or bypassed audit: extracts=%d audits=%d error=%v", extractions, audits, err)
			}
		})
	}
}
