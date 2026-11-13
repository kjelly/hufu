package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func supplementGateFixture(t *testing.T) session {
	t.Helper()
	current := gateFixture(t)
	replacePayload(t, &current.Tasks[0], func(p *payload) {
		var r record
		if err := json.Unmarshal(p.Record, &r); err != nil {
			t.Fatal(err)
		}
		r.Gaps = []json.RawMessage{raw(t, sourceGap{Description: "Caller missing from bundle", MissingEvidence: "Caller source", Kind: "missing_dependency", RequestedEvidence: []requestedEvidence{{Path: "caller.go", Ranges: []evidenceRange{}}}, Limitations: []sourceLimitation{}})}
		p.Record = raw(t, r)
	})
	requests, err := collectSupplementRequests(current, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	seal := func(id, contract string, outputs map[string]json.RawMessage) task {
		item := fixtureTask(t, id, contract, "success", payload{}, record{}, nil)
		item.SnapshotID = current.SnapshotID
		item.Receipt.ActionID = "action-" + id
		var values map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw(t, outputs)))
		decoder.UseNumber()
		if err := decoder.Decode(&values); err != nil {
			t.Fatal(err)
		}
		setSupplementRuntimeOutputs(t, &item, values)
		return item
	}
	inventory := seal("20", "inventory-review-supplement-requests", map[string]json.RawMessage{"review_supplement_requests": raw(t, requests)})
	outcomes := []supplementOutcome{{SourceTaskID: "3", GapIndex: 0, Kind: "missing_dependency", WorksetKey: "supplement-0000", Limitations: []sourceLimitation{}}}
	producer := seal("21", "prepare-review-supplement", map[string]json.RawMessage{"supplement_round": raw(t, 1), "supplement_prepared": raw(t, true), "supplement_outcomes": raw(t, outcomes)})
	review := record{Kind: "review", Revision: "frozen-head", Key: "supplement-0000", Lens: "general", Findings: []finding{}, Gaps: []json.RawMessage{}, References: []referenceAssessment{}}
	incremental := fixtureTask(t, "22", "review-supplement-workset", "success", payload{Record: raw(t, review)}, review, nil)
	incremental.Binding.Bindings["source_task_id"] = "3"
	incremental.Binding.Bindings["source_gap_index"] = "0"
	incremental.Binding.Bindings["source_payload_sha256"] = requests.Requests[0].SourceHash
	incremental.Binding.Bindings["supplement_round"] = "1"
	current.Tasks = append(current.Tasks, inventory, producer, incremental)
	return current
}

func TestSupplementAuditPreservesAcceptedSourceAndRejectsIncompleteRound(t *testing.T) {
	for _, kind := range []string{"valid", "missing_review", "wrong_source", "second_round", "receipt", "hidden_request"} {
		t.Run(kind, func(t *testing.T) {
			current := supplementGateFixture(t)
			original := append([]byte(nil), current.Tasks[0].Result.Payload.Value...)
			switch kind {
			case "missing_review":
				current.Tasks = current.Tasks[:len(current.Tasks)-1]
			case "wrong_source":
				current.Tasks[len(current.Tasks)-1].Binding.Bindings["source_task_id"] = "4"
			case "second_round":
				current.Tasks = append(current.Tasks, current.Tasks[len(current.Tasks)-2])
			case "receipt":
				current.Tasks[len(current.Tasks)-2].Receipt.RuntimeOutputsHash = "invalid"
			case "hidden_request":
				current.Tasks[len(current.Tasks)-3].Result.RuntimeOutputs["review_supplement_requests"] = raw(t, supplementRequests{Round: 1, SnapshotID: current.SnapshotID, Requests: []supplementRequest{}})
			}
			outcomes, err := collectSupplementOutcomes(current, "run-1")
			if (err == nil) != (kind == "valid") {
				t.Fatalf("audit %s: %v", kind, err)
			}
			if kind == "valid" && (len(outcomes) != 1 || outcomes[0].SourceTaskID != "3") {
				t.Fatal("lost original gap identity")
			}
			if !reflect.DeepEqual(original, []byte(current.Tasks[0].Result.Payload.Value)) {
				t.Fatal("supplement rewrote accepted source")
			}
		})
	}
}

func TestSourceGapRequiresStructuredEvidenceOrLimitation(t *testing.T) {
	for _, kind := range []string{"valid", "semantic", "missing_request", "path_escape", "invalid_range", "unknown_kind", "unknown_limitation"} {
		t.Run(kind, func(t *testing.T) {
			gap := sourceGap{Description: "Missing evidence", MissingEvidence: "caller", Kind: "unread", RequestedEvidence: []requestedEvidence{{Path: "caller.go", Ranges: []evidenceRange{{Start: 0, End: 5}}}}, Limitations: []sourceLimitation{}}
			switch kind {
			case "semantic":
				gap.Kind = "semantic_uncertainty"
				gap.RequestedEvidence = []requestedEvidence{}
			case "missing_request":
				gap.RequestedEvidence = []requestedEvidence{}
			case "path_escape":
				gap.RequestedEvidence[0].Path = "../caller.go"
			case "invalid_range":
				gap.RequestedEvidence[0].Ranges[0].Start = 6
			case "unknown_kind":
				gap.Kind = "guessed_from_prose"
			case "unknown_limitation":
				gap.Limitations = []sourceLimitation{{Kind: "hidden", Detail: "unknown"}}
			}
			_, err := decodeSourceGap(raw(t, gap))
			valid := kind == "valid" || kind == "semantic"
			if (err == nil) != valid {
				t.Fatalf("gap %s: %v", kind, err)
			}
		})
	}
}

func TestSupplementReportAuditsIncrementalEvidenceWithoutErasingSources(t *testing.T) {
	current := supplementGateFixture(t)
	var source record
	var sourcePayload payload
	if err := json.Unmarshal(current.Tasks[0].Result.Payload.Value, &sourcePayload); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(sourcePayload.Record, &source); err != nil {
		t.Fatal(err)
	}
	replacePayload(t, &current.Tasks[2], func(p *payload) {
		var critique record
		if err := json.Unmarshal(p.Record, &critique); err != nil {
			t.Fatal(err)
		}
		critique.InputReview = []inputReview{entry(current.Tasks[0], source, t)}
		p.Record = raw(t, critique)
	})
	var outcomes []supplementOutcome
	producer := current.Tasks[len(current.Tasks)-2]
	if err := json.Unmarshal(producer.Result.RuntimeOutputs["supplement_outcomes"], &outcomes); err != nil {
		t.Fatal(err)
	}
	replacePayload(t, &current.Tasks[4], func(p *payload) {
		p.InputReview = []inputReview{}
		for _, item := range current.Tasks {
			switch item.ContractID {
			case "review-primary-workset", "review-supplement-workset", "critic-review":
			default:
				continue
			}
			var value payload
			if err := json.Unmarshal(item.Result.Payload.Value, &value); err != nil {
				t.Fatal(err)
			}
			var r record
			if err := json.Unmarshal(value.Record, &r); err != nil {
				t.Fatal(err)
			}
			p.InputReview = append(p.InputReview, entry(item, r, t))
		}
		p.SupplementOutcomes = raw(t, outcomes)
		p.GapResolutions[0].ResolvingTaskID = "22"
	})
	current.Tasks[4].EvidenceFrom = append(current.Tasks[4].EvidenceFrom, "22")
	setFixtureInventory(t, &current)
	if _, _, err := verify(current, "run-1"); err != nil {
		t.Fatalf("complete supplemental handoff: %v", err)
	}
	original := append([]byte(nil), current.Tasks[0].Result.Payload.Value...)
	// The report must preserve all original source records and native outcomes.
	replacePayload(t, &current.Tasks[4], func(p *payload) { p.InputReview = p.InputReview[1:] })
	if _, _, err := verify(current, "run-1"); err == nil {
		t.Fatal("report erased the original review after supplementation")
	}
	if !reflect.DeepEqual(original, []byte(current.Tasks[0].Result.Payload.Value)) {
		t.Fatal("audit changed accepted source bytes")
	}
}

func setSupplementRuntimeOutputs(t *testing.T, item *task, values map[string]any) {
	t.Helper()
	outputs, digest, err := team.CanonicalizeRuntimeOutputs(values)
	if err != nil {
		t.Fatal(err)
	}
	item.Result.RuntimeOutputs = nil
	if err := json.Unmarshal(raw(t, outputs), &item.Result.RuntimeOutputs); err != nil {
		t.Fatal(err)
	}
	item.Result.RuntimeOutputsHash, item.Receipt.RuntimeOutputsHash = digest, digest
}

func TestSupplementActionUsesRuntimeReceiptFormat(t *testing.T) {
	for _, scenario := range []string{"valid", "changed_output", "changed_receipt", "compact_hash"} {
		t.Run(scenario, func(t *testing.T) {
			current := supplementGateFixture(t)
			item := current.Tasks[len(current.Tasks)-2]
			setSupplementRuntimeOutputs(t, &item, map[string]any{"scope": map[string]any{"count": json.Number("9007199254740993"), "ratio": json.Number("1e-7"), "paths": []any{"來源<&>.go", nil, true}, "empty": map[string]any{}}})
			switch scenario {
			case "changed_output":
				item.Result.RuntimeOutputs["scope"] = json.RawMessage(`{"count":2}`)
			case "changed_receipt":
				item.Receipt.RuntimeOutputsHash = "invalid"
			case "compact_hash":
				canonical, err := canonicalJSON(raw(t, item.Result.RuntimeOutputs))
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(canonical)
				digest := "sha256:" + hex.EncodeToString(sum[:])
				item.Result.RuntimeOutputsHash, item.Receipt.RuntimeOutputsHash = digest, digest
			}
			if err := acceptedSupplementAction(item, current, "run-1"); (err == nil) != (scenario == "valid") {
				t.Fatalf("runtime receipt verification: %v", err)
			}
		})
	}
}
