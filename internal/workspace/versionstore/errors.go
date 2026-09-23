package versionstore

import "errors"

// Typed errors. Every correctness-sensitive path fails closed with one of
// these (wrapped with context) instead of falling back to live state.
var (
	ErrSnapshotNotFound                 = errors.New("workspace snapshot not found")
	ErrWorkspaceUnstable                = errors.New("workspace kept changing during capture")
	ErrWorkspaceChangedAfterObservation = errors.New("workspace changed after it was observed")
	ErrCASCorrupt                       = errors.New("workspace object store is corrupt")
	ErrCASObjectMissing                 = errors.New("workspace object is missing")
	ErrMaterializationCollision         = errors.New("workspace materialization collides with an unmanaged path")
	ErrSnapshotNotMaterializable        = errors.New("workspace snapshot is not materializable")
	ErrBranchHeadConflict               = errors.New("workspace branch head moved concurrently")
	ErrWorkspaceSnapshotUnavailable     = errors.New("workspace snapshot is unavailable")
	ErrVersionedWorkspaceUnresolved     = errors.New("versioned workspace cannot be resolved")
	ErrVersionOperationInProgress       = errors.New("workspace version operation conflicts with active execution")
	ErrUnsupportedPathName              = errors.New("workspace path name is not valid UTF-8")
	ErrHufuignoreRequired               = errors.New(".hufuignore is required for a non-Git workspace in required mode")
	ErrWorkspaceRecoveryRequired        = errors.New("workspace versioning requires operator recovery")
	ErrModeDowngradeRefused             = errors.New("workspace versioning mode is below the recorded mode floor")
	ErrUnsupportedPlatform              = errors.New("workspace versioning is only supported on linux")
	ErrCaptureLimitExceeded             = errors.New("workspace capture limit exceeded")
	ErrUnsupportedFileType              = errors.New("workspace contains an unsupported file type")
	ErrInvalidTree                      = errors.New("invalid workspace tree object")
	ErrStoreNotFound                    = errors.New("workspace version store does not exist")
	ErrMaterializationVerifyFailed      = errors.New("materialized workspace does not match the snapshot")
)
