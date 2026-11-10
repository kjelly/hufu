package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func deliveryExtraction(request, verification string, delivery RequestDelivery) []byte {
	raw, _ := json.Marshal(map[string]any{"requirements": []any{map[string]any{
		"statement": request, "source_quote": request, "required": true, "verification": verification, "delivery": delivery,
	}}})
	return raw
}

func freezeDeliveryFixture(t *testing.T, c *Coordinator, request string, delivery RequestDelivery) *RunInputSnapshot {
	t.Helper()
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
		if operation == "contract" {
			return faithfulRequestContract(), nil
		}
		return deliveryExtraction(request, "observed", delivery), nil
	})))
	ctx, end := c.beginInvocationExecutionRun(t.Context())
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := c.resolveRunInputsForInvocation(ctx, request); err != nil {
		t.Fatal(err)
	}
	return c.RunInputSnapshot()
}

func assessedRequestReview(sourceID string, assessment RequestDeliveryAssessment) []byte {
	raw, _ := json.Marshal(map[string]any{"request_covered": true, "checks": []any{map[string]any{
		"id": "requirement-1", "status": "satisfied", "reason": "Related coverage was useful for the analysis.",
		"delivery": assessment, "evidence": []any{map[string]any{"source_id": sourceID, "quote_index": 0}},
	}}})
	return raw
}

func TestRequestAcceptanceDeliveryRejectsKnownGapsWithoutProofRepair(t *testing.T) {
	for _, scenario := range []string{"partial", "secondary", "wrong target", "unknown", "declared gap", "unauthorized substitute"} {
		t.Run(scenario, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			snapshot := freezeDeliveryFixture(t, c, "Retrieve the complete original document from its publisher", RequestDelivery{
				Target: "The requested original document", Extent: "full", Source: "primary", AllowedSubstitutes: []RequestDeliverySubstitute{},
			})
			item := completedRequestEvidence(t, c, "Related commentary and excerpts were retrieved; the complete original document was not obtained.", false)
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil || len(sources) == 0 {
				t.Fatalf("sources=%#v error=%v", sources, err)
			}
			assessment := RequestDeliveryAssessment{Match: "requested", Extent: "full", Source: "primary", Gaps: []string{}}
			switch scenario {
			case "partial":
				assessment.Extent = "partial"
			case "secondary":
				assessment.Source = "secondary"
			case "wrong target":
				assessment.Match = "none"
			case "unknown":
				assessment.Source = "unknown"
			case "declared gap":
				assessment.Gaps = []string{"The full original is still missing."}
			case "unauthorized substitute":
				assessment.Match, assessment.SubstituteIndex = "allowed_substitute", new(0)
			}
			calls := 0
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
				calls++
				return assessedRequestReview(sources[0].ID, assessment), nil
			})))
			acceptance, err := c.runAcceptance(t.Context(), "The original is unavailable. Here is a provisional analysis of related material.")
			if err == nil || acceptance.IsPassed() || calls != 1 || acceptance.RequestAcceptance.Checks[0].Status != "missing" {
				t.Fatalf("known shortfall escaped or was protocol-repaired: calls=%d review=%#v error=%v", calls, acceptance.RequestAcceptance, err)
			}
			// Even an edited saved success claim must fail offline verification;
			// no semantic model or action is called during this audit.
			acceptance.RequestAcceptance.Error = ""
			acceptance.RequestAcceptance.Checks[0].Status = "satisfied"
			result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true, RunInputs: snapshot, Acceptance: acceptance}
			if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, nil); err == nil {
				t.Fatal("offline audit accepted contradictory delivery dimensions")
			}
		})
	}
}

func TestRequestAcceptanceExplicitSubstituteAndFullOriginal(t *testing.T) {
	for _, substitute := range []bool{false, true} {
		t.Run(map[bool]string{false: "full original", true: "authorized summary"}[substitute], func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			request := "Retrieve the complete original; a third-party summary is acceptable if unavailable."
			delivery := RequestDelivery{Target: "original", Extent: "full", Source: "primary", AllowedSubstitutes: []RequestDeliverySubstitute{{
				Description: "third-party summary", SourceQuote: "a third-party summary is acceptable if unavailable", Extent: "relevant_parts", Source: "any",
			}}}
			snapshot := freezeDeliveryFixture(t, c, request, delivery)
			text := "The complete original document."
			assessment := RequestDeliveryAssessment{Match: "requested", Extent: "full", Source: "primary", Gaps: []string{}}
			if substitute {
				text = "The requested third-party summary."
				assessment.Match, assessment.SubstituteIndex, assessment.Extent, assessment.Source = "allowed_substitute", new(0), "relevant_parts", "secondary"
			}
			item := completedRequestEvidence(t, c, text, false)
			sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
			if err != nil || len(sources) == 0 {
				t.Fatalf("sources=%#v error=%v", sources, err)
			}
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
				return assessedRequestReview(sources[0].ID, assessment), nil
			})))
			acceptance, err := c.runAcceptance(t.Context())
			if err != nil || !acceptance.IsPassed() {
				t.Fatalf("authorized delivery failed: %#v error=%v", acceptance, err)
			}
			store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true, RunInputs: snapshot, Acceptance: acceptance}
			if err := ValidateRequestAcceptanceBinding(t.Context(), result, []*TodoItem{item}, store); err != nil {
				t.Fatal(err)
			}
			contract, err := RequestAcceptanceContractFromSnapshot(snapshot)
			if err != nil || contract.Version != 3 || contract.Requirements[0].Delivery.AllowedSubstitutes[0].SourceQuote != delivery.AllowedSubstitutes[0].SourceQuote {
				t.Fatalf("delivery contract changed: %#v error=%v", contract, err)
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			audited := 0
			for _, event := range events {
				if event.Type != "request_acceptance_contract_reviewed" {
					continue
				}
				var audit struct {
					ContractHash string `json:"contract_hash"`
					Faithful     bool   `json:"faithful"`
				}
				if err := json.Unmarshal(event.Payload, &audit); err != nil || !audit.Faithful || audit.ContractHash != runInputHash(snapshot.Inputs[0].CanonicalValue) {
					t.Fatalf("audit was not bound to the frozen contract: %#v error=%v", audit, err)
				}
				audited++
			}
			if audited != 1 {
				t.Fatalf("expected one independent pre-freeze audit, got %d", audited)
			}
		})
	}
}

func TestRequestAcceptanceContractAuditVetoesBeforeFreeze(t *testing.T) {
	for _, test := range []struct {
		verdict string
		calls   int
	}{
		{`{"faithful":false,"reason":"Requested original material was weakened into related coverage."}`, 3},
		{`{"faithful":false,"reason":"The original task requests opinions; primary-source acquisition was invented."}`, 3},
		{`{"reason":"Missing verdict"}`, 3},
		{`{"faithful":true,"reason":"ok","command":"unexpected"}`, 3},
		{`{"faithful":true,"reason":""}`, 3},
		{`{"faithful":"true","reason":"ok"}`, 3},
		{`{"faithful":"false","reason":"Rejected"}`, 2},
		{`{"reason":42,"faithful":false}`, 2},
		{`{"command":"unexpected","faithful":false,"reason":"Rejected"}`, 2},
		{"provider unavailable", 2},
	} {
		verdict := test.verdict
		t.Run(verdict, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			const request = "Compare the two documents"
			calls := 0
			c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
				calls++
				if operation == "extract" {
					return requestExtraction(request, request, "observed"), nil
				}
				if operation != "contract" || !strings.Contains(prompt, request) || !strings.Contains(prompt, `"delivery"`) {
					t.Fatal("independent audit omitted original request or proposed delivery contract")
				}
				if verdict == "provider unavailable" {
					return nil, errors.New(verdict)
				}
				return []byte(verdict), nil
			})))
			ctx, end := c.beginInvocationExecutionRun(t.Context())
			t.Cleanup(end)
			if err := c.checkRunAdmission(); err != nil {
				t.Fatal(err)
			}
			if err := c.resolveRunInputsForInvocation(ctx, request); err == nil || calls != test.calls {
				t.Fatalf("audit failed open or retried: calls=%d error=%v", calls, err)
			}
			if c.RunInputSnapshot() != nil || len(c.taskTracker.TodoList().Items()) != 0 {
				t.Fatal("rejected contract was frozen or dispatched")
			}
		})
	}
}

func TestRequestAcceptanceContractAuditRepairsFormatBeforeFreeze(t *testing.T) {
	for _, raw := range []string{
		`{"faithful":"true","reason":"The contract preserves the request."}`,
		`{"reason":"The contract preserves the request."}`,
		`{"faithful":true,"reason":"The contract preserves the request.","extra":true}`,
		`{"faithful":true,"reason":null}`,
		`{"faithful":true,"reason":"The contract preserves the request."`,
	} {
		t.Run(raw, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			const request = "Explain the tradeoffs"
			extractions, audits := 0, 0
			var originalPrompt string
			c.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(_ context.Context, operation, prompt string) ([]byte, error) {
				if c.RunInputSnapshot() != nil || len(c.taskTracker.TodoList().Items()) != 0 {
					t.Fatal("audit response was frozen or dispatched before validation")
				}
				if operation == "extract" {
					extractions++
					return requestExtraction(request, request, "response"), nil
				}
				if operation != "contract" {
					t.Fatalf("unexpected operation %q", operation)
				}
				audits++
				if audits == 1 {
					originalPrompt = prompt
					return []byte(raw), nil
				}
				if audits != 2 || !strings.HasPrefix(prompt, originalPrompt) || !strings.Contains(prompt, raw) || !strings.Contains(prompt, "untrusted data") || !strings.Contains(prompt, "JSON boolean") {
					t.Fatal("repair changed the candidate or omitted rejected response and protocol feedback")
				}
				return faithfulRequestContract(), nil
			}))
			ctx := beginRequestRevisionFixture(t, c)
			if err := c.resolveRunInputsForInvocation(ctx, request); err != nil {
				t.Fatal(err)
			}
			snapshot := c.RunInputSnapshot()
			if extractions != 1 || audits != 2 || snapshot == nil {
				t.Fatalf("extracts=%d audits=%d snapshot=%#v", extractions, audits, snapshot)
			}
			events, err := c.EventJournal().ReadEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			reviewed, resolved := 0, 0
			for _, event := range events {
				switch event.Type {
				case "request_acceptance_contract_reviewed":
					var review requestAcceptanceContractReview
					if err := json.Unmarshal(event.Payload, &review); err != nil || review.Faithful == nil || !*review.Faithful {
						t.Fatalf("invalid approval persisted: review=%#v error=%v", review, err)
					}
					reviewed++
				case string(EventRunInputsResolved):
					resolved++
				}
			}
			replayed := ReduceToSessionData(events)
			if reviewed != 1 || resolved != 1 || len(replayed.RunInputSnapshots) != 1 || replayed.RunInputSnapshots[0].SnapshotHash != snapshot.SnapshotHash {
				t.Fatalf("repair did not persist one approved snapshot: reviewed=%d resolved=%d", reviewed, resolved)
			}
		})
	}
}

func TestRequestAcceptanceContractAuditRepairFailuresStayUnfrozen(t *testing.T) {
	for _, scenario := range []string{"provider error", "canceled", "oversized", "invalid repair", "repair provider error", "repair canceled", "repair oversized", "repair veto"} {
		t.Run(scenario, func(t *testing.T) {
			c := requestAcceptanceFixture(t)
			const request = "Explain the tradeoffs"
			audits := 0
			ctx, cancel := context.WithCancel(beginRequestRevisionFixture(t, c))
			defer cancel()
			c.SetRequestAcceptanceModel(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
				if operation == "extract" {
					return requestExtraction(request, request, "response"), nil
				}
				audits++
				phase := scenario
				if strings.HasPrefix(scenario, "repair ") {
					if audits == 1 {
						return []byte(`{"faithful":"true","reason":"ok"}`), nil
					}
					phase = strings.TrimPrefix(scenario, "repair ")
				}
				switch phase {
				case "provider error":
					return nil, errors.New("provider unavailable")
				case "canceled":
					cancel()
					return faithfulRequestContract(), nil
				case "oversized":
					return []byte(strings.Repeat(" ", maxRunInputValueBytes+1)), nil
				case "veto":
					return []byte(`{"faithful":false,"reason":"The contract omits a requested outcome."}`), nil
				default:
					return []byte(`{"faithful":"true","reason":"ok"}`), nil
				}
			}))
			err := c.resolveRunInputsForInvocation(ctx, request)
			wantAudits := 1
			if strings.HasPrefix(scenario, "repair ") || scenario == "invalid repair" {
				wantAudits = 2
			}
			if err == nil || audits != wantAudits || c.RunInputSnapshot() != nil || len(c.taskTracker.TodoList().Items()) != 0 {
				t.Fatalf("audit repair escaped: audits=%d error=%v", audits, err)
			}
		})
	}
}

func TestRequestAcceptanceDeliverySchemaFailsClosed(t *testing.T) {
	const request = "Read the document"
	for _, delivery := range []RequestDelivery{
		{Target: "document", Extent: "whatever", Source: "any", AllowedSubstitutes: []RequestDeliverySubstitute{}},
		{Target: "document", Extent: "full", Source: "trusted", AllowedSubstitutes: []RequestDeliverySubstitute{}},
		{Target: "document", Extent: "full", Source: "any", AllowedSubstitutes: []RequestDeliverySubstitute{{Description: "summary", SourceQuote: "invented permission", Extent: "full", Source: "any"}}},
	} {
		if _, err := validateRequestAcceptanceExtraction(request, deliveryExtraction(request, "observed", delivery)); err == nil {
			t.Fatalf("invalid delivery accepted: %#v", delivery)
		}
	}
	withoutDelivery := []byte(`{"requirements":[{"statement":"Read the document","source_quote":"Read the document","required":true,"verification":"observed"}]}`)
	if _, err := validateRequestAcceptanceExtraction(request, withoutDelivery); err == nil {
		t.Fatal("new contract silently accepted legacy extraction without delivery obligations")
	}
	withoutSubstitutes := []byte(`{"requirements":[{"statement":"Read the document","source_quote":"Read the document","required":true,"verification":"observed","delivery":{"target":"document","extent":"full","source":"any"}}]}`)
	if _, err := validateRequestAcceptanceExtraction(request, withoutSubstitutes); err == nil {
		t.Fatal("new contract silently defaulted omitted alternative authorization")
	}
}

func TestRequestAcceptanceOpinionTaskHasNoInventedAcquisition(t *testing.T) {
	c := requestAcceptanceFixture(t)
	const request = "整理某項公共政策的正反看法"
	freezeRequestFixture(t, c, request, "response")
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(_ context.Context, operation, _ string) ([]byte, error) {
		if operation != "review" {
			t.Fatal("answer-only task attempted another extraction or action")
		}
		return passingRequestReview(requestAcceptanceSource{ID: requestAcceptanceFinalResponseSource}, "支持與反對的理由"), nil
	})))
	acceptance, err := c.runAcceptance(t.Context(), "支持與反對的理由，以及各自的限制。")
	if err != nil || !acceptance.IsPassed() || len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatalf("answer-only task acquired an invented requirement: %#v error=%v", acceptance, err)
	}
}

func TestRequestAcceptanceLegacySnapshotRemainsAuditable(t *testing.T) {
	legacy := []byte(`{"version":1,"request":"read document","requirements":[{"id":"requirement-1","statement":"read document","source_quote":"read document","required":true,"verification":"observed"}]}`)
	snapshot, err := ResolveRunInputSnapshot([]RunInputDefinition{requestAcceptanceDefinitionVersion(1)}, []RunInputAssignment{{
		Name: requestAcceptanceInput, RawValue: legacy, Source: RunInputSourceResolver, ResolverID: "hufu-request-acceptance", ResolverVersion: "1",
	}}, "run-legacy", "invocation", "generic")
	if err != nil {
		t.Fatal(err)
	}
	contract, err := RequestAcceptanceContractFromSnapshot(snapshot)
	if err != nil || contract.Version != 1 || contract.Requirements[0].Delivery != nil {
		t.Fatalf("historical contract was reinterpreted: %#v error=%v", contract, err)
	}
	if _, err := validateAndCanonicalizeRunInput(requestAcceptanceDefinition().Schema, legacy); err == nil {
		t.Fatal("new schema admitted a legacy contract")
	}
}

func TestRequestAcceptanceDeliveryFailurePersistsPartial(t *testing.T) {
	c := requestAcceptanceFixture(t)
	const request = "Retrieve the original document in full"
	snapshot := freezeDeliveryFixture(t, c, request, RequestDelivery{Target: "original document", Extent: "full", Source: "any", AllowedSubstitutes: []RequestDeliverySubstitute{}})
	completedRequestEvidence(t, c, "Only excerpts were returned.", false)
	sources, err := c.requestAcceptanceSources(t.Context(), snapshot)
	if err != nil || len(sources) == 0 {
		t.Fatalf("sources=%#v error=%v", sources, err)
	}
	c.SetRequestAcceptanceModel(withRequestMaterialReview(requestAcceptanceModelFunc(func(context.Context, string, string) ([]byte, error) {
		return assessedRequestReview(sources[0].ID, RequestDeliveryAssessment{Match: "requested", Extent: "partial", Source: "secondary", Gaps: []string{"The full original is missing."}}), nil
	})))
	tool := &finishTool{coordinator: c}
	for attempt := range 3 {
		response, err := tool.Run(t.Context(), fantasy.ToolCall{Input: `{"response":"Only partial analysis is available."}`})
		if err != nil || response.IsError != (attempt < 2) {
			t.Fatalf("attempt=%d response=%#v error=%v", attempt, response, err)
		}
	}
	result := c.LastRunResult()
	if result == nil || result.Outcome != RunOutcomePartial || result.StopReason != StopReasonAcceptanceFailed || result.GoalSatisfied {
		t.Fatalf("known shortfall declared completion: %#v", result)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToSessionData(events)
	if replayed.RunResult == nil || replayed.RunResult.Outcome != RunOutcomePartial || replayed.RunResult.GoalSatisfied {
		t.Fatalf("replay lost partial outcome: %#v", replayed.RunResult)
	}
	check := replayed.RunResult.Acceptance.RequestAcceptance.Checks[0]
	if check.Status != "missing" || check.Delivery == nil || check.Delivery.Extent != "partial" || len(check.Delivery.Gaps) != 1 {
		t.Fatalf("replay lost delivery shortfall: %#v", check)
	}
	if !strings.Contains(result.Response, "extent=partial") || !strings.Contains(result.Response, "The full original is missing") {
		t.Fatal("final result omitted the delivery shortfall")
	}
}
