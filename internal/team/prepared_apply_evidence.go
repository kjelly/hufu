package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type preparedApplyIntent struct {
	record    *preparedApplyRecord
	published bool
}

// Keep the exact manifest and its work binding in one create-only record
// outside the worker's root. The publication record distinguishes a pending
// write from the loss of a completed projection.
type preparedApplyRecord struct {
	AttemptWorkspaceDelta
	SealSHA256 string `json:"seal_sha256"`
}

func readPreparedApplyIntent(root, key, worldDir, projection string) (preparedApplyIntent, error) {
	var intent preparedApplyIntent
	record, err := readPreparedApplyRecord(worldDir)
	if err != nil {
		return intent, rejectNormalizationRecoveryRead(err)
	}
	intent.record = record
	prepared, err := readRuntimeEffectRecord(root, key+"/prepared")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return intent, rejectNormalizationRecoveryRead(err)
	}
	if errors.Is(err, os.ErrNotExist) && projection != "" {
		return intent, rejectResultSourceRead(errors.New("prepared apply projection lacks its original binding"))
	}
	if err == nil && (record == nil || record.SealSHA256 != prepared) {
		return intent, rejectResultSourceRead(errors.New("original prepared apply manifest is missing or changed"))
	}
	published, err := readRuntimeEffectRecord(root, key+"/published")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return intent, rejectNormalizationRecoveryRead(err)
	}
	intent.published = err == nil
	if projection != "" && (intent.record == nil || projection != intent.record.SealSHA256) ||
		intent.published && (projection == "" || projection != published) {
		return intent, rejectResultSourceRead(errors.New("prepared apply does not match its sealed intent"))
	}
	return intent, nil
}

func preparedApplyRecordDir(worldDir string) string {
	return filepath.Join(worldDir, "prepared-apply")
}

func readPreparedApplyRecord(worldDir string) (*preparedApplyRecord, error) {
	// The world delta already has a validated path-count limit and lives in
	// the runtime control area, independent of model result size limits.
	body, found, err := readSealedRuntimeEffect(preparedApplyRecordDir(worldDir), -1)
	if err != nil || !found {
		return nil, err
	}
	var record preparedApplyRecord
	if json.Unmarshal(body, &record, json.RejectUnknownMembers(true)) != nil || !validCollectionDigest(record.SealSHA256) {
		return nil, rejectResultSourceRead(errors.New("prepared apply has an invalid seal"))
	}
	if err := validateAttemptWorkspaceDelta(&record.AttemptWorkspaceDelta); err != nil {
		return nil, rejectResultSourceRead(err)
	}
	return &record, nil
}

func (intent preparedApplyIntent) loadDelta(ctx context.Context, isolated *isolatedAttempt, verifiedManifest string) (*AttemptWorkspaceDelta, error) {
	var delta *AttemptWorkspaceDelta
	if intent.record != nil {
		delta = &intent.record.AttemptWorkspaceDelta
		if delta.WorldID != isolated.worldID() || delta.BaselineDigest != isolated.prepared.isolated.baseline.digest() {
			return nil, rejectResultSourceRead(errors.New("prepared apply manifest differs from its original world"))
		}
	} else {
		var err error
		delta, err = isolated.world.Delta(ctx, isolated.prepared)
		if err != nil {
			return nil, rejectNormalizationRecoveryRead(err)
		}
	}
	if verifiedManifest != "" {
		if actual, err := normalizedDeltaDigest(delta); err != nil || actual != verifiedManifest {
			return nil, rejectResultSourceRead(errors.New("isolated workspace differs from the originally verified manifest"))
		}
	}
	return delta, nil
}

func (intent preparedApplyIntent) seal(root, key, worldDir, digest string, delta *AttemptWorkspaceDelta) error {
	if intent.record != nil {
		if intent.record.SealSHA256 != digest {
			return rejectResultSourceRead(errors.New("prepared apply candidate differs from its pending sealed intent"))
		}
		return pinRuntimeEffectDigest(root, key+"/prepared", digest)
	}
	body, err := json.Marshal(preparedApplyRecord{AttemptWorkspaceDelta: *delta, SealSHA256: digest})
	if err == nil {
		err = createSealedRuntimeEffect(preparedApplyRecordDir(worldDir), body)
	}
	if errors.Is(err, os.ErrExist) {
		return rejectResultSourceRead(errors.New("original attempt already has a sealed apply intent"))
	}
	if err != nil {
		return rejectLocalResultPersistence(fmt.Errorf("persist prepared apply intent: %w", err))
	}
	return pinRuntimeEffectDigest(root, key+"/prepared", digest)
}
