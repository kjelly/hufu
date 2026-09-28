package agent

import "fmt"

// Defaults and hard limits for the opt-in context-artifacts policy. The
// default min-bytes equals the legacy bash display bound, so a result the
// legacy path delivers unchanged is still delivered unchanged.
const (
	DefaultContextArtifactMinBytes               = 51200
	DefaultContextArtifactPreviewBytes           = 8192
	DefaultContextArtifactMaxArtifactBytes       = 1 << 20
	DefaultContextArtifactMaxReadBytes           = 32 << 10
	DefaultContextArtifactMaxArtifactsPerAttempt = 32

	MinContextArtifactPreviewBytes = 1024
	// MaxContextArtifactBytesLimit bounds the whole-file hash each byte-range
	// read performs; MaxContextArtifactReadBytesLimit keeps one view call
	// within the existing 512 KiB single-read limit.
	MaxContextArtifactBytesLimit     = 4 << 20
	MaxContextArtifactReadBytesLimit = 512 << 10
)

// ContextArtifactsPolicy controls whether a large, complete tool result is
// stored in the artifact store and shown to the model as a bounded preview
// plus an opaque reference. The zero value is disabled.
type ContextArtifactsPolicy struct {
	Enabled                bool
	MinBytes               int
	PreviewBytes           int
	MaxArtifactBytes       int
	MaxReadBytes           int
	MaxArtifactsPerAttempt int
}

// DefaultContextArtifactsPolicy returns the disabled policy with every limit
// at its default.
func DefaultContextArtifactsPolicy() ContextArtifactsPolicy {
	return ContextArtifactsPolicy{
		MinBytes:               DefaultContextArtifactMinBytes,
		PreviewBytes:           DefaultContextArtifactPreviewBytes,
		MaxArtifactBytes:       DefaultContextArtifactMaxArtifactBytes,
		MaxReadBytes:           DefaultContextArtifactMaxReadBytes,
		MaxArtifactsPerAttempt: DefaultContextArtifactMaxArtifactsPerAttempt,
	}
}

// Validate checks the resolved limits. It applies whether or not the policy
// is enabled, so a declared block cannot hide an invalid value.
func (p ContextArtifactsPolicy) Validate() error {
	for _, field := range []struct {
		name  string
		value int
	}{
		{"min-bytes", p.MinBytes},
		{"preview-bytes", p.PreviewBytes},
		{"max-artifact-bytes", p.MaxArtifactBytes},
		{"max-read-bytes", p.MaxReadBytes},
		{"max-artifacts-per-attempt", p.MaxArtifactsPerAttempt},
	} {
		if field.value <= 0 {
			return fmt.Errorf("%s must be positive, got %d", field.name, field.value)
		}
	}
	if p.PreviewBytes < MinContextArtifactPreviewBytes {
		return fmt.Errorf("preview-bytes must be at least %d, got %d", MinContextArtifactPreviewBytes, p.PreviewBytes)
	}
	if p.PreviewBytes >= p.MinBytes {
		return fmt.Errorf("preview-bytes (%d) must be less than min-bytes (%d)", p.PreviewBytes, p.MinBytes)
	}
	if p.MinBytes > p.MaxArtifactBytes {
		return fmt.Errorf("min-bytes (%d) must not exceed max-artifact-bytes (%d)", p.MinBytes, p.MaxArtifactBytes)
	}
	if p.MaxArtifactBytes > MaxContextArtifactBytesLimit {
		return fmt.Errorf("max-artifact-bytes must not exceed %d, got %d", MaxContextArtifactBytesLimit, p.MaxArtifactBytes)
	}
	if limit := min(p.MaxArtifactBytes, MaxContextArtifactReadBytesLimit); p.MaxReadBytes > limit {
		return fmt.Errorf("max-read-bytes must not exceed %d, got %d", limit, p.MaxReadBytes)
	}
	return nil
}
