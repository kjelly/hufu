package inspect

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

const maxTraceDiagnostics = 100

type TraceData struct {
	RunID   string       `json:"run_id"`
	Entries []TraceEntry `json:"entries"`
}

type traceCandidate struct {
	entry      TraceEntry
	anchored   bool
	entryClass int
	stableKey  string
}

func InspectTrace(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindTrace); err != nil {
		return nil, err
	}
	lineage, err := LoadLineage(ctx, query)
	if err != nil {
		return nil, err
	}
	selected, err := selectRun(lineage, query)
	if err != nil {
		return nil, err
	}
	indexedEvents := runIndexedEvents(lineage, query)
	query.BranchID = lineage.BranchID
	diagnostics := make([]Diagnostic, 0)
	candidates := make([]traceCandidate, 0, len(indexedEvents))
	for _, indexed := range indexedEvents {
		entry := eventTraceEntry(indexed)
		candidates = append(candidates, traceCandidate{entry: entry, anchored: true, stableKey: indexed.Event.ID})
	}
	tasks, err := team.ReplayTodoList(selected.runEvents)
	if err != nil {
		return nil, fmt.Errorf("%w: replay tasks for run %q: %v", ErrIntegrity, query.RunID, err)
	}
	for _, item := range tasks {
		if item == nil {
			continue
		}
		candidates = append(candidates, receiptTraceCandidates(indexedEvents, item, query)...)
		candidates = append(candidates, contextTraceCandidates(indexedEvents, item, query)...)
		candidates = append(candidates, memoryTraceCandidates(indexedEvents, item, query)...)
	}
	if result, resultErr := selectedRunResult(selected, query.RunID); resultErr != nil {
		return nil, resultErr
	} else if result != nil && result.EvidenceManifest != nil {
		entry := TraceEntry{
			Ref:  TraceRef{RunID: query.RunID, SessionID: query.SessionID, BranchID: lineage.BranchID, Source: "evidence_manifest"},
			Kind: "evidence_manifest", Status: result.EvidenceManifest.Status,
			Refs: evidenceRefs(result.EvidenceManifest),
		}
		anchorSupplement(&entry, terminalIndexedEvent(indexedEvents))
		candidates = append(candidates, supplementalCandidate(entry, result.EvidenceManifest.ManifestHash))
	}

	if decisions, loadErr := LoadDecisionAddresses(query); loadErr == nil {
		for _, decision := range decisions {
			entry := TraceEntry{
				Ref:  TraceRef{RunID: query.RunID, BranchID: lineage.BranchID, TaskID: decision.TaskID, Source: "decision_index"},
				Kind: "decision_address", Status: decisionStatus(decision),
				Refs: normalizeOpaqueRefs([]string{decision.DecisionID, decision.RecordRef.ID, decision.RecordRef.Digest}),
			}
			anchorSupplement(&entry, findPayloadAnchor(indexedEvents, "decision_id", decision.DecisionID, ""))
			candidates = append(candidates, supplementalCandidate(entry, decision.DecisionID))
		}
	} else {
		diagnostics = append(diagnostics, Diagnostic{Code: ReasonProjectionUnreadable, Severity: "warning", Message: "Decision addressing projection could not be read."})
	}
	if terminals, loadErr := LoadTerminalFacts(query); loadErr == nil {
		for _, terminal := range terminals {
			refs := []string{terminal.SessionID}
			for _, output := range terminal.OutputRefs {
				refs = append(refs, output.ID, output.Digest)
			}
			entry := TraceEntry{
				Ref:  TraceRef{RunID: query.RunID, BranchID: lineage.BranchID, TaskID: terminal.OwnerTaskID, AgentID: terminal.Agent, Source: "terminal_projection"},
				Kind: "terminal_session", Status: string(terminal.State), Refs: normalizeOpaqueRefs(refs),
			}
			anchorSupplement(&entry, findPayloadAnchor(indexedEvents, "session_id", terminal.SessionID, "terminal_"))
			candidates = append(candidates, supplementalCandidate(entry, terminal.SessionID))
		}
	} else {
		diagnostics = append(diagnostics, Diagnostic{Code: ReasonProjectionUnreadable, Severity: "warning", Message: "Terminal lifecycle projection could not be read."})
	}

	slices.SortStableFunc(candidates, compareTraceCandidates)
	data := TraceData{RunID: query.RunID, Entries: make([]TraceEntry, 0, len(candidates))}
	missingCount := 0
	for _, candidate := range candidates {
		data.Entries = append(data.Entries, candidate.entry)
		if candidate.anchored || candidate.entry.Ref.Source == "event_store" {
			continue
		}
		missingCount++
		if len(diagnostics) < maxTraceDiagnostics {
			diagnostics = append(diagnostics, Diagnostic{Code: ReasonMissingAnchor, Severity: "warning", Message: "Supplemental projection has no persisted event anchor.", Ref: safeOpaqueRef(candidate.stableKey)})
		}
	}
	envelope := envelope(KindTrace, query, lineage.BranchID, data)
	envelope.Diagnostics = diagnostics
	if missingCount > maxTraceDiagnostics {
		envelope.Diagnostics = append(envelope.Diagnostics, Diagnostic{Code: ReasonMissingAnchor, Severity: "warning", Message: "Additional unanchored supplemental projections were omitted from diagnostics."})
	}
	if len(envelope.Diagnostics) > 0 {
		envelope.Integrity.Projection = "unavailable"
	}
	return envelope, nil
}

func runIndexedEvents(lineage Lineage, query InspectQuery) []IndexedEvent {
	out := make([]IndexedEvent, 0)
	for _, indexed := range lineage.Events {
		if indexed.Event.RunID == query.RunID && (query.SessionID == "" || indexed.Event.SessionID == query.SessionID) {
			out = append(out, indexed)
		}
	}
	return out
}

func eventTraceEntry(indexed IndexedEvent) TraceEntry {
	event := indexed.Event
	status, reasonCode := eventStatusAndReason(event.Payload)
	return TraceEntry{
		Ref: TraceRef{
			RunID: event.RunID, SessionID: event.SessionID, BranchID: event.BranchID,
			TaskID: event.TaskID, Attempt: event.Attempt, AgentID: event.Actor,
			EventID: event.ID, EventHash: event.Hash, EventOrdinal: indexed.Ordinal, Source: "event_store",
		},
		Kind: event.Type, AnchorEventID: event.ID, AnchorEventOrdinal: indexed.Ordinal,
		Timestamp: event.Timestamp, Status: status, ReasonCode: reasonCode, Refs: []string{},
	}
}

func eventStatusAndReason(payload json.RawMessage) (string, string) {
	var metadata struct {
		Status      string `json:"status"`
		Outcome     string `json:"outcome"`
		ReasonCode  string `json:"reason_code"`
		FailureType string `json:"failure_type"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &metadata) != nil {
		return "", ""
	}
	status := boundedCode(metadata.Status)
	if status == "" {
		status = boundedCode(metadata.Outcome)
	}
	reason := boundedCode(metadata.ReasonCode)
	if reason == "" {
		reason = boundedCode(metadata.FailureType)
	}
	return status, reason
}

func boundedCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("_.:-", char) {
			continue
		}
		return ""
	}
	return value
}

func receiptTraceCandidates(events []IndexedEvent, item *team.TodoItem, query InspectQuery) []traceCandidate {
	out := make([]traceCandidate, 0, len(item.ExecutionReceipts))
	for _, receipt := range item.ExecutionReceipts {
		if receipt.RunID != query.RunID || receipt.TaskID != item.ID {
			continue
		}
		refs := []string{receipt.ModelExecutionID, receipt.ProducerID, receipt.TranscriptRef, receipt.ProviderTranscriptRef}
		if receipt.VerifyResult != nil {
			refs = append(refs, receipt.VerifyResult.Fingerprint)
		}
		entry := TraceEntry{
			Ref:  TraceRef{RunID: query.RunID, BranchID: query.BranchID, TaskID: item.ID, Attempt: receipt.Attempt, AgentID: receipt.ProducerID, ExecutionTarget: receipt.Backend, Source: "execution_receipt"},
			Kind: "execution_receipt", Timestamp: receiptTimestamp(receipt), Status: receiptStatus(receipt), Refs: normalizeOpaqueRefs(refs),
		}
		anchorSupplement(&entry, findReceiptAnchor(events, receipt))
		stableKey := fmt.Sprintf("%s:%d:%s", item.ID, receipt.Attempt, receipt.ModelExecutionID)
		out = append(out, supplementalCandidate(entry, stableKey))
	}
	return out
}

func receiptTimestamp(receipt team.ExecutionReceipt) string {
	if !receipt.FinishedAt.IsZero() {
		return receipt.FinishedAt.UTC().Format(time.RFC3339Nano)
	}
	if !receipt.StartedAt.IsZero() {
		return receipt.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	return ""
}

func receiptStatus(receipt team.ExecutionReceipt) string {
	if receipt.ExitCode == nil {
		return "unknown"
	}
	if *receipt.ExitCode == 0 {
		return "succeeded"
	}
	return "failed"
}

func findReceiptAnchor(events []IndexedEvent, receipt team.ExecutionReceipt) *IndexedEvent {
	for index := range events {
		event := &events[index]
		if event.Event.TaskID != receipt.TaskID {
			continue
		}
		var payload struct {
			ExecutionReceipt  *team.ExecutionReceipt  `json:"execution_receipt"`
			ExecutionReceipts []team.ExecutionReceipt `json:"execution_receipts"`
		}
		if json.Unmarshal(event.Event.Payload, &payload) != nil {
			continue
		}
		if payload.ExecutionReceipt != nil && sameReceiptIdentity(*payload.ExecutionReceipt, receipt) {
			return event
		}
		for _, candidate := range payload.ExecutionReceipts {
			if sameReceiptIdentity(candidate, receipt) {
				return event
			}
		}
	}
	return nil
}

func sameReceiptIdentity(left, right team.ExecutionReceipt) bool {
	return left.RunID == right.RunID && left.TaskID == right.TaskID && left.Attempt == right.Attempt && left.ModelExecutionID == right.ModelExecutionID && left.ProducerID == right.ProducerID
}

func contextTraceCandidates(events []IndexedEvent, item *team.TodoItem, query InspectQuery) []traceCandidate {
	var out []traceCandidate
	for _, manifest := range item.ContextManifests {
		if manifest.RunID != query.RunID {
			continue
		}
		refs := []string{manifest.Fingerprint, manifest.RequestID, manifest.ModelExecutionID}
		for _, contextItem := range manifest.Items {
			refs = append(refs, strings.TrimPrefix(contextItem.ID, "context:"))
		}
		entry := TraceEntry{
			Ref:  TraceRef{RunID: query.RunID, BranchID: query.BranchID, TaskID: item.ID, Attempt: manifest.Attempt, AgentID: manifest.Agent, ExecutionTarget: manifest.ModelExecutionID, Source: "context_manifest"},
			Kind: "context_injection", Timestamp: formatPersistedTime(manifest.CreatedAt), Status: boundedCode(manifest.Outcome), Refs: normalizeOpaqueRefs(refs),
		}
		anchorSupplement(&entry, findContextManifestAnchor(events, manifest.Fingerprint))
		out = append(out, supplementalCandidate(entry, manifest.Fingerprint+":"+manifest.RequestID))
	}
	return out
}

func findContextManifestAnchor(events []IndexedEvent, fingerprint string) *IndexedEvent {
	for index := range events {
		var payload struct {
			Fingerprint      string                          `json:"fingerprint"`
			ContextManifests []team.ContextInjectionManifest `json:"context_manifests"`
		}
		if json.Unmarshal(events[index].Event.Payload, &payload) != nil {
			continue
		}
		if payload.Fingerprint == fingerprint {
			return &events[index]
		}
		for _, manifest := range payload.ContextManifests {
			if manifest.Fingerprint == fingerprint {
				return &events[index]
			}
		}
	}
	return nil
}

func memoryTraceCandidates(events []IndexedEvent, item *team.TodoItem, query InspectQuery) []traceCandidate {
	var out []traceCandidate
	for _, manifest := range item.MemoryManifests {
		if manifest.RunID != query.RunID {
			continue
		}
		for _, memoryItem := range manifest.Items {
			entry := TraceEntry{
				Ref:  TraceRef{RunID: query.RunID, BranchID: query.BranchID, TaskID: item.ID, Attempt: manifest.Attempt, AgentID: manifest.Agent, Source: "memory_manifest"},
				Kind: "memory_retrieval", Timestamp: formatPersistedTime(manifest.CreatedAt), Status: "injected",
				Refs: normalizeOpaqueRefs([]string{manifest.RetrievalID, manifest.Fingerprint, memoryItem.ContextItemID}),
			}
			anchorSupplement(&entry, findMemoryAnchor(events, manifest.RetrievalID, memoryItem.ContextItemID))
			out = append(out, supplementalCandidate(entry, manifest.RetrievalID+":"+memoryItem.ContextItemID))
		}
	}
	return out
}

func formatPersistedTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func findMemoryAnchor(events []IndexedEvent, retrievalID, contextItemID string) *IndexedEvent {
	for index := range events {
		if events[index].Event.Type != string(team.EventMemoryRetrieved) {
			continue
		}
		var payload struct {
			RetrievalID   string `json:"retrieval_id"`
			ContextItemID string `json:"context_item_id"`
		}
		if json.Unmarshal(events[index].Event.Payload, &payload) == nil && payload.RetrievalID == retrievalID && payload.ContextItemID == contextItemID {
			return &events[index]
		}
	}
	return nil
}

func terminalIndexedEvent(events []IndexedEvent) *IndexedEvent {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Event.Type == string(team.EventRunFinished) {
			return &events[index]
		}
	}
	return nil
}

func findPayloadAnchor(events []IndexedEvent, field, value, typePrefix string) *IndexedEvent {
	if value == "" {
		return nil
	}
	for index := range events {
		if typePrefix != "" && !strings.HasPrefix(events[index].Event.Type, typePrefix) {
			continue
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal(events[index].Event.Payload, &payload) != nil {
			continue
		}
		var candidate string
		if json.Unmarshal(payload[field], &candidate) == nil && candidate == value {
			return &events[index]
		}
	}
	return nil
}

func anchorSupplement(entry *TraceEntry, anchor *IndexedEvent) {
	if entry == nil {
		return
	}
	if anchor == nil {
		entry.ReasonCode = ReasonMissingAnchor
		return
	}
	entry.AnchorEventID = anchor.Event.ID
	entry.AnchorEventOrdinal = anchor.Ordinal
	if entry.Ref.SessionID == "" {
		entry.Ref.SessionID = anchor.Event.SessionID
	}
	if entry.Ref.BranchID == "" {
		entry.Ref.BranchID = anchor.Event.BranchID
	}
	if entry.Timestamp == "" {
		entry.Timestamp = anchor.Event.Timestamp
	}
}

func supplementalCandidate(entry TraceEntry, stableKey string) traceCandidate {
	return traceCandidate{entry: entry, anchored: entry.AnchorEventOrdinal > 0, entryClass: 1, stableKey: stableKey}
}

func compareTraceCandidates(left, right traceCandidate) int {
	if left.anchored != right.anchored {
		if left.anchored {
			return -1
		}
		return 1
	}
	if left.anchored {
		if order := cmp.Compare(left.entry.AnchorEventOrdinal, right.entry.AnchorEventOrdinal); order != 0 {
			return order
		}
		if order := cmp.Compare(left.entryClass, right.entryClass); order != 0 {
			return order
		}
		if order := cmp.Compare(left.entry.Timestamp, right.entry.Timestamp); order != 0 {
			return order
		}
		if order := cmp.Compare(left.entry.Ref.Source, right.entry.Ref.Source); order != 0 {
			return order
		}
	} else {
		if order := cmp.Compare(left.entry.Ref.Source, right.entry.Ref.Source); order != 0 {
			return order
		}
		if order := cmp.Compare(left.entry.Timestamp, right.entry.Timestamp); order != 0 {
			return order
		}
	}
	return cmp.Compare(left.stableKey, right.stableKey)
}

func decisionStatus(decision DecisionAddress) string {
	if decision.Resolved {
		return "resolved"
	}
	return "pending"
}
