// Package versionstore is Hufu's runtime-independent workspace version store:
// immutable content-addressed objects (blobs, symlink targets, Merkle trees)
// plus a SQLite snapshot DAG with per-branch heads, operation records, and
// subject-root state. It never imports internal/team or internal/workspace;
// callers adapt their own types to the ones defined here.
package versionstore

import (
	"fmt"
	"time"
)

// SnapshotID identifies one immutable snapshot node ("wsv_<32 hex>"). It is
// deliberately not the root tree hash: two checkpoints with identical bytes
// still have distinct lineage.
type SnapshotID string

// OperationID identifies one operation row ("wop_<32 hex>").
type OperationID string

// SnapshotReason records why a snapshot node exists.
type SnapshotReason string

const (
	SnapshotBaseline      SnapshotReason = "baseline"
	SnapshotExternalDrift SnapshotReason = "external_drift"
	SnapshotRunCheckpoint SnapshotReason = "run_checkpoint"
	SnapshotFork          SnapshotReason = "fork"
	SnapshotCheckoutSave  SnapshotReason = "checkout_save"
	SnapshotRestore       SnapshotReason = "restore"
	SnapshotManual        SnapshotReason = "manual"
	SnapshotAdopt         SnapshotReason = "adopt"

	// SnapshotAttempt is reserved for attempt-level checkpoints (v1.x). v1
	// never produces it.
	SnapshotAttempt SnapshotReason = "attempt"
)

var validReasons = map[SnapshotReason]bool{
	SnapshotBaseline: true, SnapshotExternalDrift: true, SnapshotRunCheckpoint: true,
	SnapshotFork: true, SnapshotCheckoutSave: true, SnapshotRestore: true,
	SnapshotManual: true, SnapshotAdopt: true,
}

// Valid reports whether r may be recorded by v1.
func (r SnapshotReason) Valid() bool { return validReasons[r] }

// SnapshotState is the lifecycle of a snapshot row. Allowed transitions are
// pending→published, pending→orphaned, and published→pruned.
type SnapshotState string

const (
	StatePending   SnapshotState = "pending"
	StatePublished SnapshotState = "published"
	StateOrphaned  SnapshotState = "orphaned"
	StatePruned    SnapshotState = "pruned"
)

// Mode is the workspace-versioning mode of a subject root.
type Mode string

const (
	ModeOff      Mode = "off"
	ModeObserve  Mode = "observe"
	ModeRequired Mode = "required"
)

// ParseMode maps a configuration string to a Mode; the empty string is off.
func ParseMode(value string) (Mode, error) {
	switch Mode(value) {
	case "", ModeOff:
		return ModeOff, nil
	case ModeObserve, ModeRequired:
		return Mode(value), nil
	default:
		return "", fmt.Errorf("unknown workspace versioning mode %q (want off, observe, or required)", value)
	}
}

func (m Mode) rank() int {
	switch m {
	case ModeObserve:
		return 1
	case ModeRequired:
		return 2
	default:
		return 0
	}
}

// Below reports whether m is a lower mode than floor.
func (m Mode) Below(floor Mode) bool { return m.rank() < floor.rank() }

// CaptureLimits bounds one capture. MaxFileBytes and MaxLogicalBytes of zero
// mean unlimited; exceeding any limit fails the capture rather than omitting
// managed files.
type CaptureLimits struct {
	MaxFiles        int
	MaxFileBytes    int64
	MaxLogicalBytes int64
}

// DefaultMaxFiles matches the effect snapshotter's historical budget.
const DefaultMaxFiles = 200000

// DefaultCaptureLimits is the single source of capture defaults; config may
// only override individual fields.
func DefaultCaptureLimits() CaptureLimits {
	return CaptureLimits{MaxFiles: DefaultMaxFiles}
}

// WithDefaults fills zero MaxFiles with the default.
func (l CaptureLimits) WithDefaults() CaptureLimits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = DefaultMaxFiles
	}
	return l
}

// Snapshot is one row of the snapshot DAG.
type Snapshot struct {
	ID             SnapshotID
	WorkspaceID    string
	BranchID       string
	Parent         SnapshotID
	RootTreeHash   string
	ManifestDigest string
	FileCount      int
	LogicalBytes   int64
	Materializable bool
	Reason         SnapshotReason
	RunID          string
	TaskID         string
	Attempt        int
	AnchorEventID  string
	CommitEventID  string
	State          SnapshotState
	CreatedAt      time.Time
	PublishedAt    time.Time
	PrunedAt       time.Time
}

// CaptureStats describes the CAS effect of one capture.
type CaptureStats struct {
	NewCASBytes    int64
	ReusedCASBytes int64
	Duration       time.Duration
}
