// Shared type definitions used by all platforms (linux, darwin, windows).
package tools

import (
	"context"

	"github.com/kjelly/hufu/internal/hooks"
)

type ToolOption func(*ToolConfig)

type PathReviewer func(ctx context.Context, command string, path string) (bool, error)

type ToolConfig struct {
	WorkDir        string
	AllowedPaths   []string
	PathConsent    *PathConsent
	PathReviewer   PathReviewer
	ToolName       string
	WorkspaceName  string
	Hooks          *hooks.HookRegistry
	RestrictedBash bool
	RestrictedPath string
	NetworkBlock   bool
	Direnv         bool
	ForceMCP       bool
}
