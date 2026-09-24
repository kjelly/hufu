package team

import (
	"path/filepath"
	"sync"
)

// attemptWorkspaceIntegrationLocks serializes, per canonical subject root,
// copying the project into an attempt world (shared) against applying an
// attempt's changes back (exclusive), so a world never copies a
// half-applied change set from this process.
var attemptWorkspaceIntegrationLocks = struct {
	sync.Mutex
	byRoot map[string]*sync.RWMutex
}{byRoot: make(map[string]*sync.RWMutex)}

func attemptWorkspaceIntegrationLock(subjectRoot string) *sync.RWMutex {
	key := filepath.Clean(subjectRoot)
	if resolved, err := filepath.EvalSymlinks(key); err == nil {
		key = resolved
	}
	attemptWorkspaceIntegrationLocks.Lock()
	defer attemptWorkspaceIntegrationLocks.Unlock()
	lock := attemptWorkspaceIntegrationLocks.byRoot[key]
	if lock == nil {
		lock = &sync.RWMutex{}
		attemptWorkspaceIntegrationLocks.byRoot[key] = lock
	}
	return lock
}
