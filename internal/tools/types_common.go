// Shared type definitions used by all platforms (linux, darwin, windows).
package tools

import (
	"context"
	"io"

	"github.com/kjelly/hufu/internal/hooks"
)

type ToolOption func(*ToolConfig)

type PathReviewer func(ctx context.Context, command string, path string) (bool, error)

// ArtifactOpener resolves a runtime-issued opaque artifact reference. The
// caller owns authorization and integrity checks; filesystem tools must never
// reinterpret the reference as a path or send it through path consent.
type ArtifactOpener func(ctx context.Context, ref string) (io.ReadCloser, error)

type ToolConfig struct {
	WorkDir           string
	AllowedPaths      []string
	AllowedWritePaths []string
	PathConsent       *PathConsent
	PathReviewer      PathReviewer
	ArtifactOpener    ArtifactOpener
	ToolName          string
	WorkspaceName     string
	Hooks             *hooks.HookRegistry
	RestrictedBash    bool
	RestrictedPath    string
	NetworkBlock      bool
	Direnv            bool
	ForceMCP          bool
}
