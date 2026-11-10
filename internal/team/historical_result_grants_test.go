package team

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func historicalGrantEventFixture(t *testing.T) RunEvent {
	t.Helper()
	source := RunEvent{ID: "accepted-event", SessionID: "source-session", RunID: "source-run", TaskID: "1", Type: string(EventTaskCompleted)}
	payload := HistoricalResultGrantsPayload{
		SchemaVersion: historicalResultGrantsVersion, Workspace: filepath.Join(t.TempDir(), "control"), Team: "review",
		SessionID: "consumer-session", BranchID: "main", CycleID: "consumer-cycle", PolicyFingerprint: strings.Repeat("a", 64),
		Grants: []HistoricalResultGrant{{Recipient: "reviewer", ResultID: HistoricalResultID(source),
			SourceSessionID: source.SessionID, SourceBranchID: "main", SourceRunID: source.RunID,
			SourceTaskID: source.TaskID, SourceAgent: "Researcher", SourceAttempt: 2, SourceDispatchAttempt: 1,
			SourceEventID: source.ID, SourceEventSHA256: strings.Repeat("b", 64), PayloadSHA256: strings.Repeat("c", 64),
			PresentationSHA256: strings.Repeat("d", 64), PresentationBytes: 100}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return RunEvent{SchemaVersion: eventStoreSchemaVersion, ID: "grant-event", RunID: payload.CycleID, SessionID: payload.SessionID,
		BranchID: payload.BranchID, Actor: "operator", Type: string(EventHistoricalResultsGranted), Timestamp: "2026-10-29T00:00:00Z",
		IdempotencyKey: "grant:consumer-cycle", Payload: data}
}

func changeHistoricalGrantPayload(t *testing.T, event RunEvent, change func(*HistoricalResultGrantsPayload)) RunEvent {
	t.Helper()
	var payload HistoricalResultGrantsPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	change(&payload)
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event.Payload = data
	return event
}

func TestHistoricalResultGrantEnvelopeValidation(t *testing.T) {
	fixture := historicalGrantEventFixture(t)
	tests := []struct {
		name   string
		change func(RunEvent) RunEvent
	}{
		{"legacy_event", func(e RunEvent) RunEvent { e.SchemaVersion = 1; return e }},
		{"unknown_version", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.SchemaVersion++ })
		}},
		{"consumer_mismatch", func(e RunEvent) RunEvent { e.RunID = "another-cycle"; return e }},
		{"worker_actor", func(e RunEvent) RunEvent { e.Actor = "reviewer"; return e }},
		{"task_binding", func(e RunEvent) RunEvent { e.TaskID = "2"; return e }},
		{"missing_key", func(e RunEvent) RunEvent { e.IdempotencyKey = ""; return e }},
		{"unknown_field", func(e RunEvent) RunEvent {
			e.Payload = append(append([]byte(nil), e.Payload[:len(e.Payload)-1]...), []byte(`,"presentation":"forged"}`)...)
			return e
		}},
		{"duplicate_field", func(e RunEvent) RunEvent {
			e.Payload = bytes.Replace(e.Payload, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
			return e
		}},
		{"nested_duplicate", func(e RunEvent) RunEvent {
			e.Payload = bytes.Replace(e.Payload, []byte(`"recipient":"reviewer"`), []byte(`"recipient":"reviewer","recipient":"reviewer"`), 1)
			return e
		}},
		{"trailing_value", func(e RunEvent) RunEvent { e.Payload = append(e.Payload, []byte(` {}`)...); return e }},
		{"relative_workspace", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.Workspace = "workspace" })
		}},
		{"noncanonical_team", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.Team = "Review" })
		}},
		{"wildcard_recipient", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.Grants[0].Recipient = "review*" })
		}},
		{"empty_grants", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.Grants = nil })
		}},
		{"duplicate_grant", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.Grants = append(p.Grants, p.Grants[0]) })
		}},
		{"unsorted_grants", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) {
				g := p.Grants[0]
				g.Recipient = "author"
				p.Grants = append(p.Grants, g)
			})
		}},
		{"too_many", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) {
				for range maxHistoricalResultGrants {
					p.Grants = append(p.Grants, p.Grants[0])
				}
			})
		}},
		{"wrong_source_id", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.Grants[0].SourceEventID = "other-event" })
		}},
		{"missing_dispatch_attempt", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.Grants[0].SourceDispatchAttempt = 0 })
		}},
		{"invalid_hash", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.Grants[0].PayloadSHA256 = strings.Repeat("G", 64) })
		}},
		{"zero_bytes", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.Grants[0].PresentationBytes = 0 })
		}},
		{"aggregate_byte_limit", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) {
				g := p.Grants[0]
				g.Recipient = "writer"
				g.PresentationBytes = maxHistoricalResultBytes
				p.Grants = append(p.Grants, g)
			})
		}},
		{"integer_overflow", func(e RunEvent) RunEvent {
			return changeHistoricalGrantPayload(t, e, func(p *HistoricalResultGrantsPayload) { p.Grants[0].PresentationBytes = int(^uint(0) >> 1) })
		}},
	}
	if err := ValidateEventPayload(fixture); err != nil {
		t.Fatalf("valid grant: %v", err)
	}
	if !IsKnownEventType(fixture.Type) {
		t.Fatal("grant event is absent from the event catalog")
	}
	decoded, err := decodeHistoricalResultGrants(fixture)
	if err != nil || decoded.Grants[0].SourceAttempt != 2 || decoded.Grants[0].SourceDispatchAttempt != 1 {
		t.Fatalf("distinct occurrence/dispatch attempts were lost: %+v, %v", decoded, err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateEventPayload(test.change(cloneRunEvent(fixture))); err == nil {
				t.Fatal("accepted an invalid grant envelope")
			}
		})
	}
}

func openHistoricalGrantStore(t *testing.T) *EventStore {
	t.Helper()
	store, err := NewEventStore(t.TempDir(), "consumer-cycle", "consumer-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.SetBranchID("main")
	return store
}

func TestHistoricalResultGrantAppendRejectsAmbiguousJSON(t *testing.T) {
	for _, field := range []string{"schema_version", "recipient"} {
		t.Run(field, func(t *testing.T) {
			store := openHistoricalGrantStore(t)
			request := historicalGrantEventFixture(t)
			if field == "schema_version" {
				request.Payload = bytes.Replace(request.Payload, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
			} else {
				request.Payload = bytes.Replace(request.Payload, []byte(`"recipient":"reviewer"`), []byte(`"recipient":"reviewer","recipient":"reviewer"`), 1)
			}
			if _, err := store.AppendPersisted(request); err == nil {
				t.Fatal("redaction erased duplicate members before grant validation")
			}
			events, err := store.ReadEvents()
			if err != nil || len(events) != 0 {
				t.Fatalf("invalid raw grant changed the journal: %d, %v", len(events), err)
			}
		})
	}
}

func assertHistoricalGrantIndex(t *testing.T, store *EventStore, event RunEvent) {
	t.Helper()
	ref, found, err := runtimestore.HistoricalGrantCycle(t.Context(), store.store.DB(), event.BranchID, event.RunID)
	if err != nil || !found || ref.EventID != event.ID || ref.EventHash != event.Hash {
		t.Fatalf("grant index = %+v, found %v, error %v", ref, found, err)
	}
}

func TestHistoricalResultGrantAdmissionIsImmutable(t *testing.T) {
	store := openHistoricalGrantStore(t)
	request := historicalGrantEventFixture(t)
	durable, err := store.AppendPersisted(request)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoricalGrantIndex(t, store, durable)
	retry := cloneRunEvent(request)
	retry.ID, retry.RunID, retry.SessionID, retry.BranchID, retry.Timestamp = "", "", "", "", ""
	again, err := store.AppendPersisted(retry)
	if err != nil || again.ID != durable.ID || again.Hash != durable.Hash {
		t.Fatalf("exact admission retry = %+v, %v", again, err)
	}
	changed := changeHistoricalGrantPayload(t, request, func(p *HistoricalResultGrantsPayload) { p.Grants[0].Recipient = "other-worker" })
	if _, err := store.AppendPersisted(changed); !errors.Is(err, ErrHistoricalResultGrantConflict) {
		t.Fatalf("replaced an admission using its idempotency key: %v", err)
	}
	changed.ID, changed.IdempotencyKey = "replacement-event", "replacement-key"
	if _, err := store.AppendPersisted(changed); !errors.Is(err, runtimestore.ErrHistoricalGrantConflict) {
		t.Fatalf("replaced an admission using another key: %v", err)
	}
	otherType := RunEvent{Type: "other-event", Actor: "operator", IdempotencyKey: request.IdempotencyKey}
	if _, err := store.AppendPersisted(otherType); !errors.Is(err, ErrHistoricalResultGrantConflict) {
		t.Fatalf("reused a grant key for a different event: %v", err)
	}
	events, err := store.ReadEvents()
	if err != nil || len(events) != 1 {
		t.Fatalf("conflicting admission left extra events: %d, %v", len(events), err)
	}
	assertHistoricalGrantIndex(t, store, durable)
	for _, branch := range []string{"sibling", "independent-root"} {
		other := changeHistoricalGrantPayload(t, request, func(p *HistoricalResultGrantsPayload) { p.BranchID = branch })
		other.BranchID, other.ID = branch, "grant-"+branch
		event, err := store.AppendPersisted(other)
		if err != nil {
			t.Fatal(err)
		}
		assertHistoricalGrantIndex(t, store, event)
	}
	next := changeHistoricalGrantPayload(t, request, func(p *HistoricalResultGrantsPayload) { p.CycleID = "next-cycle" })
	next.RunID, next.ID, next.IdempotencyKey = "next-cycle", "next-grant", "grant:next-cycle"
	event, err := store.AppendPersisted(next)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoricalGrantIndex(t, store, event)
}

func TestHistoricalResultGrantEventAndIndexCommitTogether(t *testing.T) {
	store := openHistoricalGrantStore(t)
	request := historicalGrantEventFixture(t)
	if _, err := store.store.DB().ExecContext(t.Context(), `CREATE TRIGGER refuse_grant BEFORE INSERT ON historical_grant_cycles BEGIN SELECT RAISE(ABORT,'injected index failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(request); err == nil {
		t.Fatal("append ignored an index failure")
	}
	var events, refs int
	if err := store.store.DB().QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM events),(SELECT COUNT(*) FROM historical_grant_cycles)`).Scan(&events, &refs); err != nil {
		t.Fatal(err)
	}
	if events != 0 || refs != 0 {
		t.Fatalf("partial grant transaction: events %d, references %d", events, refs)
	}
	if _, err := store.store.DB().ExecContext(t.Context(), `DROP TRIGGER refuse_grant`); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyHashChain(); err != nil {
		t.Fatal(err)
	}
	store.commitHook = func() error { return errors.New("uncertain acknowledgement") }
	if _, err := store.AppendPersisted(request); err == nil {
		t.Fatal("lost uncertain commit error")
	}
	// Reload after the uncertain acknowledgement must recover both canonical
	// event and reference, then return the same admission without another event.
	durable, err := store.AppendPersisted(request)
	if err != nil {
		t.Fatal(err)
	}
	assertHistoricalGrantIndex(t, store, durable)
	loaded, err := store.ReadEvents()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("uncertain retry duplicated the grant: %d, %v", len(loaded), err)
	}
}

func TestHistoricalResultGrantRetryRejectsMissingIndex(t *testing.T) {
	store := openHistoricalGrantStore(t)
	request := historicalGrantEventFixture(t)
	if _, err := store.AppendPersisted(request); err != nil {
		t.Fatal(err)
	}
	if _, err := store.store.DB().ExecContext(t.Context(), `DELETE FROM historical_grant_cycles`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(request); !errors.Is(err, runtimestore.ErrHistoricalGrantSource) {
		t.Fatalf("retry silently reconstructed a missing authorization reference: %v", err)
	}
	replacement := changeHistoricalGrantPayload(t, request, func(p *HistoricalResultGrantsPayload) { p.Grants[0].Recipient = "another-worker" })
	replacement.ID, replacement.IdempotencyKey = "replacement", "another-key"
	if _, err := store.AppendPersisted(replacement); !errors.Is(err, runtimestore.ErrHistoricalGrantConflict) {
		t.Fatalf("new key replaced the original journal admission after index loss: %v", err)
	}
}

func TestHistoricalResultGrantExportImportPreservesAuthority(t *testing.T) {
	store := openHistoricalGrantStore(t)
	durable, err := store.AppendPersisted(historicalGrantEventFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if err := ExportEventLog(t.Context(), filepath.Dir(store.path), &exported); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if count, err := ImportEventLog(t.Context(), workspace, bytes.NewReader(exported.Bytes())); err != nil || count != 1 {
		t.Fatalf("import = %d, %v", count, err)
	}
	imported, err := OpenEventStoreReadOnly(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = imported.Close() })
	assertHistoricalGrantIndex(t, imported, durable)
	loaded, err := imported.ReadEvents()
	if err != nil || len(loaded) != 1 || loaded[0].Hash != durable.Hash || !bytes.Equal(loaded[0].Payload, durable.Payload) {
		t.Fatalf("import changed canonical grant bytes: %+v, %v", loaded, err)
	}
	// A valid hash chain with a second admission for the same branch/cycle
	// cannot partially import either its events or its references.
	conflict := cloneRunEvent(durable)
	conflict.ID, conflict.IdempotencyKey = "second-grant", "second-key"
	conflict.PreviousID, conflict.PreviousHash = durable.ID, durable.Hash
	conflict.Hash = ComputeEventHash(conflict.PreviousHash, conflict.ID, conflict.Type, conflict.Timestamp, conflict.Payload)
	line, err := json.Marshal(conflict)
	if err != nil {
		t.Fatal(err)
	}
	log := append(append([]byte(nil), exported.Bytes()...), append(line, '\n')...)
	rejected := t.TempDir()
	if _, err := ImportEventLog(t.Context(), rejected, bytes.NewReader(log)); !errors.Is(err, runtimestore.ErrHistoricalGrantConflict) {
		t.Fatalf("imported a conflicting cycle admission: %v", err)
	}
	rejectedStore, err := OpenEventStoreReadOnly(rejected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rejectedStore.Close() })
	loaded, err = rejectedStore.ReadEvents()
	if err != nil || len(loaded) != 0 {
		t.Fatalf("failed import left partial events: %d, %v", len(loaded), err)
	}
	if _, found, err := runtimestore.HistoricalGrantCycle(t.Context(), rejectedStore.store.DB(), "main", "consumer-cycle"); err != nil || found {
		t.Fatalf("failed import left a grant reference: %v, %v", found, err)
	}
}
