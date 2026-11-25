package team

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
)

const normalizedVerificationRecordMaxBytes = 4 * resultPayloadMaxBytes

type normalizedVerificationRecord struct {
	Binding string
	Outcome normalizedObjectiveOutcome
}

type pendingVerificationPublication struct {
	key    string
	digest string
}

// Persist the start outside the mutable checkpoint before the verifier can run.
// A lost or rolled-back checkpoint cannot authorize repeating this effect.
func (checkpoint *normalizedCompletionCheckpoint) createCheckStart(name string) error {
	if err := createRuntimeEffectRecord(checkpoint.root, checkpoint.key+"/"+name, checkpoint.BindingSHA256); err != nil {
		if errors.Is(err, os.ErrExist) {
			return unknownVerificationOutcome(name)
		}
		return rejectLocalResultPersistence(fmt.Errorf("persist verification start: %w", err))
	}
	return nil
}

// Reopen completed evidence without changing SQLite. Validation and preview
// use this same reader and cannot publish or launch checks.
func (checkpoint *normalizedCompletionCheckpoint) restoreCheckEvidence() error {
	objective, err := checkpoint.readCheckOutcome(objectiveVerification, checkpoint.Objective)
	if err != nil {
		return err
	}
	skeptic, err := checkpoint.readCheckOutcome(adversarialVerification, normalizedObjectiveOutcome{normalizedCheckOutcome: checkpoint.Skeptic})
	if err != nil {
		return err
	}
	checkpoint.Objective, checkpoint.Skeptic = objective, skeptic.normalizedCheckOutcome
	return nil
}

func (checkpoint *normalizedCompletionCheckpoint) readCheckOutcome(name string, outcome normalizedObjectiveOutcome) (normalizedObjectiveOutcome, error) {
	key := checkpoint.key + "/" + name
	binding, err := readRuntimeEffectRecord(checkpoint.root, key)
	started := err == nil
	if !started && !errors.Is(err, os.ErrNotExist) {
		return outcome, rejectNormalizationRecoveryRead(err)
	}
	sealed, published, err := readRuntimeEffectCompletion(checkpoint.root, key+"/outcome", normalizedVerificationRecordMaxBytes)
	if err != nil {
		return outcome, err
	}
	if !started && (outcome.Started || sealed != nil) {
		return outcome, rejectResultSourceRead(fmt.Errorf("checkpoint lacks the original %s start", name))
	}
	if started && binding != checkpoint.BindingSHA256 {
		return outcome, rejectResultSourceRead(fmt.Errorf("checkpoint does not match the original %s start", name))
	}
	var projection []byte
	if outcome.Completed {
		projection, err = marshalNormalizedCheckpoint(normalizedVerificationRecord{Binding: binding, Outcome: outcome})
		if err != nil {
			return outcome, rejectResultSourceRead(err)
		}
	}
	if err := validateRuntimeEffectProjection(sealed, projection, published); err != nil {
		return outcome, rejectResultSourceRead(fmt.Errorf("checkpoint does not match the sealed %s outcome: %w", name, err))
	}
	if sealed == nil {
		if started && !outcome.Started {
			return outcome, unknownVerificationOutcome(name)
		}
		return outcome, nil
	}
	var record normalizedVerificationRecord
	if json.Unmarshal(sealed, &record, jsonv1.DefaultOptionsV1(), json.RejectUnknownMembers(true)) != nil ||
		record.Binding != binding || !record.Outcome.valid() || !record.Outcome.Completed ||
		name == adversarialVerification && record.Outcome.Result != nil {
		return outcome, rejectResultSourceRead(fmt.Errorf("invalid sealed %s outcome", name))
	}
	if !published {
		checkpoint.pendingChecks = append(checkpoint.pendingChecks, pendingVerificationPublication{key + "/outcome", sha256HexBytes(sealed)})
	}
	return record.Outcome, nil
}

func (checkpoint *normalizedCompletionCheckpoint) sealCheckOutcome(name string) error {
	outcome := normalizedObjectiveOutcome{normalizedCheckOutcome: checkpoint.Skeptic}
	if name == objectiveVerification {
		outcome = checkpoint.Objective
	}
	body, err := marshalNormalizedCheckpoint(normalizedVerificationRecord{Binding: checkpoint.BindingSHA256, Outcome: outcome})
	if err != nil {
		return rejectLocalResultPersistence(err)
	}
	if len(body) > normalizedVerificationRecordMaxBytes {
		return rejectResultSourceRead(errors.New("sealed verification outcome exceeds its evidence limit"))
	}
	key := checkpoint.key + "/" + name + "/outcome"
	if err := createRuntimeEffectBody(checkpoint.root, key, body); err != nil {
		return rejectLocalResultPersistence(fmt.Errorf("persist verification outcome: %w", err))
	}
	checkpoint.pendingChecks = append(checkpoint.pendingChecks, pendingVerificationPublication{key, sha256HexBytes(body)})
	return nil
}

func (checkpoint *normalizedCompletionCheckpoint) publishPendingChecks() error {
	if len(checkpoint.pendingChecks) == 0 {
		return nil
	}
	if err := checkpoint.save(); err != nil {
		return err
	}
	for _, pending := range checkpoint.pendingChecks {
		if err := publishRuntimeEffectProjection(checkpoint.root, pending.key, pending.digest); err != nil {
			return err
		}
	}
	checkpoint.pendingChecks = nil
	return nil
}
