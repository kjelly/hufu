package versionstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/fsutil"
)

const (
	maxProviderRecoveryBytes = 32 * 1024
	providerRecoveryPrefix   = "provider-recovery-"
)

type providerRecovery struct {
	SubjectRoot string `json:"subject_root"`
	Code        string `json:"code"`
	Detail      string `json:"detail"`
	ID          string `json:"id,omitempty"`
}

func providerRecoveryPath(root, subject string) string {
	return filepath.Join(root, providerRecoveryPrefix+SubjectKey(subject)+".json")
}

// RecordProviderRecovery atomically replaces the current provider disposition.
// This record owns provider recovery even when SQLite cannot accept writes.
// Only an explicit restore or adopt clears it.
func RecordProviderRecovery(stateDir, canonicalRoot, code, detail string) error {
	body, err := json.Marshal(providerRecovery{SubjectRoot: canonicalRoot, Code: code, Detail: detail, ID: rand.Text()})
	if err != nil {
		return err
	}
	if len(body) > maxProviderRecoveryBytes {
		return errors.New("workspace provider recovery record exceeds its byte limit")
	}
	root := StorageRoot(stateDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("prepare workspace provider recovery record: %w", err)
	}
	path := providerRecoveryPath(root, canonicalRoot)
	lock, err := lockProviderRecovery(context.Background(), path)
	if err != nil {
		return err
	}
	return errors.Join(fsutil.AtomicWriteFile(path, body, 0o600), lock.Close())
}

func (s *Store) readProviderRecovery(state *SubjectState) error {
	if state.RecoveryRequired {
		state.legacyRecoveryGeneration = state.Generation
	}
	marker, err := readProviderRecoveryFile(providerRecoveryPath(s.layout.root, state.SubjectRoot))
	if err != nil {
		return err
	}
	if marker.Code != "" {
		marker.applyTo(state)
	}
	return nil
}

func (marker providerRecovery) applyTo(state *SubjectState) {
	state.RecoveryRequired, state.RecoveryCode, state.RecoveryDetail = true, marker.Code, marker.Detail
	state.providerRecoveryID = marker.ID
}

func readProviderRecoveryFile(path string) (providerRecovery, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return providerRecovery{}, nil
	}
	if err != nil {
		return providerRecovery{}, fmt.Errorf("open workspace provider recovery record: %w", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, maxProviderRecoveryBytes+1))
	if err != nil {
		return providerRecovery{}, fmt.Errorf("read workspace provider recovery record: %w", err)
	}
	var marker providerRecovery
	if len(body) > maxProviderRecoveryBytes || json.Unmarshal(body, &marker) != nil || marker.Code == "" ||
		strings.Contains(marker.ID, ":") || marker.SubjectRoot == "" || path != providerRecoveryPath(filepath.Dir(path), marker.SubjectRoot) {
		return providerRecovery{}, fmt.Errorf("%w: invalid workspace provider recovery record", ErrWorkspaceRecoveryRequired)
	}
	if marker.ID == "" {
		marker.ID = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	return marker, nil
}

// RecoveryToken identifies the exact disposition inspected by an operator,
// including the observable absence of a guard.
func (state SubjectState) RecoveryToken() string {
	return strconv.FormatInt(state.legacyRecoveryGeneration, 10) + ":" + state.providerRecoveryID
}

// CheckRecoveryToken detects an already superseded disposition without locking
// publishers. Use WithRecoveryToken for the acceptance itself.
func (s *Store) CheckRecoveryToken(ctx context.Context, canonicalRoot, expected string) error {
	state, found, err := s.GetSubjectState(ctx, canonicalRoot)
	if err != nil {
		return err
	}
	if !found || state.RecoveryToken() != expected {
		return ErrWorkspaceRecoveryChanged
	}
	return nil
}

// WithRecoveryToken keeps the inspected disposition current while accepting
// captured files. Run the capture before acquiring the lock so a provider can
// publish a new disposition during capture and invalidate those files.
func (s *Store) WithRecoveryToken(ctx context.Context, canonicalRoot, expected string, accept func() error) (err error) {
	if err := s.requireWritable(); err != nil {
		return err
	}
	lock, err := lockProviderRecovery(ctx, providerRecoveryPath(s.layout.root, canonicalRoot))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := s.CheckRecoveryToken(ctx, canonicalRoot, expected); err != nil {
		return err
	}
	return accept()
}

// ClearRecovery clears only the disposition the operator inspected. Restore
// recovery may repeat this call after the original process was interrupted.
func (s *Store) ClearRecovery(ctx context.Context, canonicalRoot, expected string) error {
	if err := s.requireWritable(); err != nil {
		return err
	}
	if expected == "" {
		return nil // Older restore operations did not record authority to clear.
	}
	path := providerRecoveryPath(s.layout.root, canonicalRoot)
	lock, err := lockProviderRecovery(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	state, _, err := s.GetSubjectState(ctx, canonicalRoot)
	if err != nil || !state.RecoveryRequired {
		return err
	}
	_, providerID, valid := strings.Cut(expected, ":")
	// Allow an interrupted clear to resume after its legacy SQL projection
	// was cleared, but require the same provider record throughout.
	if !valid || state.providerRecoveryID != providerID ||
		(state.legacyRecoveryGeneration != 0 && state.RecoveryToken() != expected) {
		return ErrWorkspaceRecoveryChanged
	}
	if state.legacyRecoveryGeneration != 0 {
		if err := s.clearLegacyRecovery(ctx, canonicalRoot, state.legacyRecoveryGeneration); err != nil {
			return err
		}
	}
	if state.providerRecoveryID == "" {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove workspace provider recovery record: %w", err)
	}
	return fsutil.SyncDir(s.layout.root)
}

func (s *Store) clearLegacyRecovery(ctx context.Context, canonicalRoot string, generation int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE subject_state SET recovery_required=0, recovery_code='',
recovery_detail='', generation=generation+1, updated_at=? WHERE subject_key=? AND generation=?`,
		s.nowMillis(), SubjectKey(canonicalRoot), generation)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return errors.Join(ErrWorkspaceRecoveryChanged, err)
	}
	return nil
}

func lockProviderRecovery(ctx context.Context, recordPath string) (*fsutil.FileLock, error) {
	for {
		lock, err := fsutil.TryLockExclusive(recordPath + ".lock")
		if !errors.Is(err, fsutil.ErrWouldBlock) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
