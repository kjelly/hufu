package config

import (
	"fmt"
	"time"
)

// WorkspaceVersioningConfig is the `workspace-versioning:` block of the global
// config. Mode is a subject-root property, so it deliberately lives here and
// not in team.yaml.
type WorkspaceVersioningConfig struct {
	// Mode is "", "off", "observe", or "required"; "" means off.
	Mode      string                     `yaml:"mode"`
	Capture   WorkspaceVersioningCapture `yaml:"capture"`
	Retention WorkspaceVersioningRetain  `yaml:"retention"`
}

// WorkspaceVersioningCapture overrides capture limits; zero keeps the
// version store default (MaxFiles) or means unlimited (byte limits).
type WorkspaceVersioningCapture struct {
	MaxFiles        int   `yaml:"max-files"`
	MaxFileBytes    int64 `yaml:"max-file-bytes"`
	MaxLogicalBytes int64 `yaml:"max-logical-bytes"`
}

// WorkspaceVersioningRetain configures GC retention.
type WorkspaceVersioningRetain struct {
	// KeepRecentSnapshots is per branch; 0 means the default of 50.
	KeepRecentSnapshots int `yaml:"keep-recent-snapshots"`
	// OrphanGrace is a time.ParseDuration string; "" means 24h.
	OrphanGrace string `yaml:"orphan-grace"`
}

// merge applies a later config file's non-zero values over the current ones,
// like the other LoadConfig fields.
func (w *WorkspaceVersioningConfig) merge(next WorkspaceVersioningConfig) {
	if next.Mode != "" {
		w.Mode = next.Mode
	}
	if next.Capture.MaxFiles > 0 {
		w.Capture.MaxFiles = next.Capture.MaxFiles
	}
	if next.Capture.MaxFileBytes > 0 {
		w.Capture.MaxFileBytes = next.Capture.MaxFileBytes
	}
	if next.Capture.MaxLogicalBytes > 0 {
		w.Capture.MaxLogicalBytes = next.Capture.MaxLogicalBytes
	}
	if next.Retention.KeepRecentSnapshots > 0 {
		w.Retention.KeepRecentSnapshots = next.Retention.KeepRecentSnapshots
	}
	if next.Retention.OrphanGrace != "" {
		w.Retention.OrphanGrace = next.Retention.OrphanGrace
	}
}

const (
	defaultKeepRecentSnapshots = 50
	defaultOrphanGrace         = 24 * time.Hour
)

// KeepRecent returns the effective per-branch retention count.
func (r WorkspaceVersioningRetain) KeepRecent() int {
	if r.KeepRecentSnapshots <= 0 {
		return defaultKeepRecentSnapshots
	}
	return r.KeepRecentSnapshots
}

// Grace returns the effective orphan grace period.
func (r WorkspaceVersioningRetain) Grace() (time.Duration, error) {
	if r.OrphanGrace == "" {
		return defaultOrphanGrace, nil
	}
	grace, err := time.ParseDuration(r.OrphanGrace)
	if err != nil || grace < 0 {
		return 0, fmt.Errorf("workspace-versioning.retention.orphan-grace %q is not a valid duration", r.OrphanGrace)
	}
	return grace, nil
}
