// Shared by all platforms: the runtime installs these hooks on the tool call
// context, and tools only consult them.
package tools

import (
	"context"

	"charm.land/fantasy"
)

// ToolOutputCapture is the complete, redacted, model-facing text of one
// finished tool call, captured before the tool applies its legacy display
// bound.
type ToolOutputCapture struct {
	// ToolName must equal the name the offloader was bound to.
	ToolName string
	Content  string
	IsError  bool
	// LegacyWouldTruncate reports that the tool's legacy bound would drop or
	// cut content.
	LegacyWouldTruncate bool
}

// ToolOutputOffloader stores a captured result and returns its bounded
// replacement. It returns false when the result stays on the legacy path.
type ToolOutputOffloader func(ctx context.Context, capture ToolOutputCapture) (fantasy.ToolResponse, bool)

type toolOutputOffloaderKey struct{}

// WithToolOutputOffloader installs fn for the tool call run under the
// returned context.
func WithToolOutputOffloader(ctx context.Context, fn ToolOutputOffloader) context.Context {
	return context.WithValue(ctx, toolOutputOffloaderKey{}, fn)
}

// OffloadToolOutput hands capture to the installed offloader. It returns
// false when none is installed or the offloader declined, so the caller
// returns its legacy bounded response.
func OffloadToolOutput(ctx context.Context, capture ToolOutputCapture) (fantasy.ToolResponse, bool) {
	fn, ok := ctx.Value(toolOutputOffloaderKey{}).(ToolOutputOffloader)
	if !ok || fn == nil {
		return fantasy.ToolResponse{}, false
	}
	return fn(ctx, capture)
}

// DefaultArtifactMaxReadBytes bounds a view byte-range read when the attempt
// sets no explicit limit.
const DefaultArtifactMaxReadBytes = 32 * 1024

type artifactMaxReadBytesKey struct{}

// WithArtifactMaxReadBytes sets the view byte-range limit for the attempt.
func WithArtifactMaxReadBytes(ctx context.Context, limit int) context.Context {
	return context.WithValue(ctx, artifactMaxReadBytesKey{}, limit)
}

// ArtifactMaxReadBytes returns the view byte-range limit for ctx.
func ArtifactMaxReadBytes(ctx context.Context) int {
	if limit, ok := ctx.Value(artifactMaxReadBytesKey{}).(int); ok && limit > 0 {
		return limit
	}
	return DefaultArtifactMaxReadBytes
}
