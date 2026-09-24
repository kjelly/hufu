package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/kjelly/hufu/internal/fsutil"
)

const (
	// attemptWorldBaselineFile records the copied state a world's delta is
	// computed against, so a world kept across a restart (protocol repair,
	// crash recovery) can still produce its delta.
	attemptWorldBaselineFile = "baseline.json"
	// attemptWorldDeltaFile is the delta being applied, written before
	// attempt_workspace_apply_started so a crash mid-apply can re-run it.
	attemptWorldDeltaFile = "delta.json"
)

// attemptWorldsDir is where attempt worlds live under a control root.
func attemptWorldsDir(controlRoot string) string {
	return filepath.Join(controlRoot, attemptWorldsDirName)
}

type attemptWorldBaselineRecord struct {
	Format  int                    `json:"format"`
	GitMode bool                   `json:"git_mode"`
	Digest  string                 `json:"digest"`
	Entries []AttemptManifestEntry `json:"entries"`
}

func writeAttemptWorldBaseline(worldDir string, gitMode bool, baseline attemptManifest) error {
	paths := make([]string, 0, len(baseline))
	for rel := range baseline {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	record := attemptWorldBaselineRecord{Format: attemptWorldFormat, GitMode: gitMode, Digest: baseline.digest(), Entries: make([]AttemptManifestEntry, 0, len(paths))}
	for _, rel := range paths {
		record.Entries = append(record.Entries, baseline[rel])
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode world baseline: %w", err)
	}
	if err := fsutil.AtomicWriteFile(filepath.Join(worldDir, attemptWorldBaselineFile), encoded, 0o644); err != nil {
		return fmt.Errorf("write world baseline: %w", err)
	}
	return nil
}

func readAttemptWorldBaseline(worldDir string) (bool, attemptManifest, error) {
	data, err := os.ReadFile(filepath.Join(worldDir, attemptWorldBaselineFile))
	if err != nil {
		return false, nil, fmt.Errorf("read world baseline: %w", err)
	}
	var record attemptWorldBaselineRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return false, nil, fmt.Errorf("decode world baseline: %w", err)
	}
	if record.Format != attemptWorldFormat {
		return false, nil, fmt.Errorf("world baseline format %d is not supported", record.Format)
	}
	baseline := make(attemptManifest, len(record.Entries))
	for _, entry := range record.Entries {
		baseline[entry.Path] = entry
	}
	if baseline.digest() != record.Digest {
		return false, nil, errors.New("world baseline does not match its digest")
	}
	return record.GitMode, baseline, nil
}

func writeAttemptWorldDelta(worldDir string, delta *AttemptWorkspaceDelta) error {
	encoded, err := json.Marshal(delta)
	if err != nil {
		return fmt.Errorf("encode world delta: %w", err)
	}
	if err := fsutil.AtomicWriteFile(filepath.Join(worldDir, attemptWorldDeltaFile), encoded, 0o644); err != nil {
		return fmt.Errorf("write world delta: %w", err)
	}
	return nil
}

// Load reopens a world a previous process prepared. The owner marker must
// name the directory and the recorded baseline must match its digest (and
// wantBaselineDigest, the digest attempt_workspace_prepared recorded, when
// the caller has it).
func (w *IsolatedCopyExecutionWorld) Load(worldDir, wantBaselineDigest string) (*PreparedExecutionWorld, attemptWorldOwner, error) {
	owner, err := readAttemptWorldOwner(worldDir)
	if err != nil {
		return nil, attemptWorldOwner{}, err
	}
	worldRoot := filepath.Join(worldDir, attemptWorldRootDir)
	if info, err := os.Lstat(worldRoot); err != nil || !info.IsDir() {
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			err = errors.New("world root is missing")
		}
		return nil, owner, fmt.Errorf("reopen attempt world %s: %w", owner.WorldID, err)
	}
	gitMode, baseline, err := readAttemptWorldBaseline(worldDir)
	if err != nil {
		return nil, owner, fmt.Errorf("reopen attempt world %s: %w", owner.WorldID, err)
	}
	if wantBaselineDigest != "" && baseline.digest() != wantBaselineDigest {
		return nil, owner, fmt.Errorf("reopen attempt world %s: baseline does not match the prepared event", owner.WorldID)
	}
	var bytes int64
	for _, entry := range baseline {
		bytes += entry.Size
	}
	return &PreparedExecutionWorld{
		ID: owner.WorldID, Provider: isolatedCopyExecutionWorldName,
		Root: worldRoot, CWD: worldRoot, WritableRoots: []string{worldRoot},
		isolated: &isolatedWorldState{
			worldID: owner.WorldID, worldDir: worldDir, sourceRoot: owner.SourceRoot, gitMode: gitMode,
			baseline: baseline, fileCount: len(baseline), byteCount: bytes,
		},
	}, owner, nil
}
