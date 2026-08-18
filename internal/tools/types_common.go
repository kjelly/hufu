// Shared type definitions used by all platforms (linux, darwin, windows).
package tools

import (
	"context"
	"io"
	"sync"

	"github.com/kjelly/hufu/internal/hooks"
)

type ToolOption func(*ToolConfig)

type PathReviewer func(ctx context.Context, command string, path string) (bool, error)

// ConsentResult records the user's path-access decision.
type ConsentResult int

const (
	ConsentDenied ConsentResult = iota
	ConsentOnce
	ConsentAlways
)

// AgentInfo identifies the active agent while a path-consent decision is made.
type AgentInfo struct {
	Name string
	Task string
}

// PathConsent is shared by the platform-specific implementations. Keeping the
// state type platform-neutral lets the non-interactive Windows stub compile
// while Linux and macOS retain the full consent workflow.
type PathConsent struct {
	mu           sync.Mutex
	remembered   []string
	denied       []string
	currentAgent func() AgentInfo
	persistPath  string
}

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
