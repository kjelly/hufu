package team

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/runtimestore"
)

const (
	historicalResultGrantsVersion = 1
	maxHistoricalResultGrants     = 16
	maxHistoricalResultBytes      = 1024 * 1024
)

var ErrHistoricalResultGrantConflict = errors.New("historical result grant admission conflicts with its durable event")

// HistoricalResultGrantsPayload freezes one cycle's explicit recipient grants.
// Only references and digests are durable here; source results remain in their
// canonical events. Possessing this envelope does not grant artifact access.
type HistoricalResultGrantsPayload struct {
	SchemaVersion     int                     `json:"schema_version"`
	Workspace         string                  `json:"workspace"`
	Team              string                  `json:"team"`
	SessionID         string                  `json:"session_id"`
	BranchID          string                  `json:"branch_id"`
	CycleID           string                  `json:"cycle_id"`
	PolicyFingerprint string                  `json:"policy_fingerprint"`
	Grants            []HistoricalResultGrant `json:"grants"`
}

// HistoricalResultGrant identifies the exact accepted source, recipient and
// required presentation. Occurrence and dispatch attempts are distinct: result
// collection can accept a prior dispatch in a later task occurrence.
type HistoricalResultGrant struct {
	Recipient             string `json:"recipient"`
	ResultID              string `json:"result_id"`
	SourceSessionID       string `json:"source_session_id"`
	SourceBranchID        string `json:"source_branch_id"`
	SourceRunID           string `json:"source_run_id"`
	SourceTaskID          string `json:"source_task_id"`
	SourceAgent           string `json:"source_agent"`
	SourceAttempt         int    `json:"source_attempt"`
	SourceDispatchAttempt int    `json:"source_dispatch_attempt"`
	SourceEventID         string `json:"source_event_id"`
	SourceEventSHA256     string `json:"source_event_sha256"`
	PayloadSHA256         string `json:"payload_sha256"`
	PresentationSHA256    string `json:"presentation_sha256"`
	PresentationBytes     int    `json:"presentation_bytes"`
}

func decodeHistoricalResultGrantPayload(data []byte) (HistoricalResultGrantsPayload, error) {
	var payload HistoricalResultGrantsPayload
	if err := json.Unmarshal(data, &payload, json.RejectUnknownMembers(true)); err != nil {
		return payload, fmt.Errorf("decode historical result grants: %w", err)
	}
	return payload, nil
}

func decodeHistoricalResultGrants(event RunEvent) (HistoricalResultGrantsPayload, error) {
	payload, err := decodeHistoricalResultGrantPayload(event.Payload)
	if err != nil {
		return payload, err
	}
	if event.SchemaVersion != eventStoreSchemaVersion || event.Type != string(EventHistoricalResultsGranted) ||
		event.Actor != "operator" || event.TaskID != "" || event.Attempt != 0 || event.IdempotencyKey == "" ||
		payload.SchemaVersion != historicalResultGrantsVersion || payload.SessionID != event.SessionID ||
		payload.BranchID != event.BranchID || payload.CycleID != event.RunID {
		return payload, fmt.Errorf("historical result grants have invalid event or consumer binding")
	}
	if !historicalGrantIdentity(payload.SessionID) || !historicalGrantIdentity(payload.BranchID) || !historicalGrantIdentity(payload.CycleID) ||
		!filepath.IsAbs(payload.Workspace) || filepath.Clean(payload.Workspace) != payload.Workspace ||
		!historicalGrantIdentity(payload.Workspace) || !historicalGrantCanonicalName(payload.Team) || !historicalGrantSHA256(payload.PolicyFingerprint) {
		return payload, fmt.Errorf("historical result grants have invalid scope or policy")
	}
	if len(payload.Grants) == 0 || len(payload.Grants) > maxHistoricalResultGrants {
		return payload, fmt.Errorf("historical result grants require between 1 and %d recipients/results", maxHistoricalResultGrants)
	}
	var previousRecipient, previousResult string
	totalBytes := 0
	for i, grant := range payload.Grants {
		if err := validateHistoricalResultGrant(grant); err != nil {
			return payload, fmt.Errorf("historical result grant %d: %w", i, err)
		}
		if i > 0 && (grant.Recipient < previousRecipient || (grant.Recipient == previousRecipient && grant.ResultID <= previousResult)) {
			return payload, fmt.Errorf("historical result grants are not uniquely sorted by recipient and result")
		}
		previousRecipient, previousResult = grant.Recipient, grant.ResultID
		// Subtract first so hostile int values cannot overflow the aggregate.
		if grant.PresentationBytes <= 0 || grant.PresentationBytes > maxHistoricalResultBytes-totalBytes {
			return payload, fmt.Errorf("historical result grant presentations exceed the required byte limit")
		}
		totalBytes += grant.PresentationBytes
	}
	return payload, nil
}

func validateHistoricalResultGrant(grant HistoricalResultGrant) error {
	if !historicalGrantCanonicalName(grant.Recipient) ||
		!historicalGrantIdentity(grant.SourceSessionID) || !historicalGrantIdentity(grant.SourceBranchID) ||
		!historicalGrantIdentity(grant.SourceRunID) || !historicalGrantIdentity(grant.SourceTaskID) ||
		!historicalGrantIdentity(grant.SourceAgent) || !historicalGrantIdentity(grant.SourceEventID) ||
		grant.SourceAttempt < 1 || grant.SourceDispatchAttempt < 1 ||
		!historicalGrantSHA256(grant.SourceEventSHA256) || !historicalGrantSHA256(grant.PayloadSHA256) ||
		!historicalGrantSHA256(grant.PresentationSHA256) {
		return errors.New("invalid source or recipient identity")
	}
	expectedID := HistoricalResultID(RunEvent{ID: grant.SourceEventID, SessionID: grant.SourceSessionID,
		RunID: grant.SourceRunID, TaskID: grant.SourceTaskID, Type: string(EventTaskCompleted)})
	if grant.ResultID != expectedID {
		return errors.New("result does not identify its accepted source")
	}
	return nil
}

func (es *EventStore) validateHistoricalGrantReplay(ctx context.Context, q runtimestore.Queryer, durable, requested RunEvent) error {
	if requested.Type != string(EventHistoricalResultsGranted) && durable.Type != string(EventHistoricalResultsGranted) {
		return nil
	}
	prepared, err := es.prepareEvent(requested)
	if err != nil {
		return err
	}
	if err := historicalGrantIdempotencyEquivalent(durable, prepared); err != nil {
		return err
	}
	ref, found, err := runtimestore.HistoricalGrantCycle(ctx, q, durable.BranchID, durable.RunID)
	if err != nil {
		return err
	}
	if !found || ref.EventID != durable.ID || ref.EventHash != durable.Hash {
		return runtimestore.ErrHistoricalGrantSource
	}
	return nil
}

func historicalGrantIdentity(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func historicalGrantCanonicalName(value string) bool {
	return historicalGrantIdentity(value) && strings.ToLower(value) == value && !strings.ContainsAny(value, "*?")
}

func historicalGrantSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func historicalGrantIdempotencyEquivalent(durable, requested RunEvent) error {
	if durable.Type != requested.Type {
		return ErrHistoricalResultGrantConflict
	}
	oldPayload, err := decodeHistoricalResultGrants(durable)
	if err != nil {
		return err
	}
	newPayload, err := decodeHistoricalResultGrants(requested)
	if err != nil {
		return err
	}
	oldJSON, err := json.Marshal(oldPayload)
	if err != nil {
		return err
	}
	newJSON, err := json.Marshal(newPayload)
	if err != nil {
		return err
	}
	if string(oldJSON) != string(newJSON) {
		return ErrHistoricalResultGrantConflict
	}
	return nil
}
