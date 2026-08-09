//go:build linux || darwin
// +build linux darwin

package tools

import (
	"github.com/kjelly/hufu/internal/hooks"
)

func WithWorkDir(dir string) ToolOption {
	return func(c *ToolConfig) {
		c.WorkDir = dir
	}
}

func WithAllowedPaths(paths []string) ToolOption {
	return func(c *ToolConfig) {
		c.AllowedPaths = paths
	}
}

func WithPathConsent(consent *PathConsent) ToolOption {
	return func(c *ToolConfig) {
		c.PathConsent = consent
	}
}

func WithPathReviewer(reviewer PathReviewer) ToolOption {
	return func(c *ToolConfig) {
		c.PathReviewer = reviewer
	}
}

func WithToolName(name string) ToolOption {
	return func(c *ToolConfig) {
		c.ToolName = name
	}
}

func WithWorkspaceName(name string) ToolOption {
	return func(c *ToolConfig) {
		c.WorkspaceName = name
	}
}

func WithHooks(h *hooks.HookRegistry) ToolOption {
	return func(c *ToolConfig) {
		c.Hooks = h
	}
}

func WithRestrictedBash(enabled bool) ToolOption {
	return func(c *ToolConfig) {
		c.RestrictedBash = enabled
	}
}

func WithRestrictedPath(path string) ToolOption {
	return func(c *ToolConfig) {
		c.RestrictedPath = path
	}
}

func WithNetworkBlock(enabled bool) ToolOption {
	return func(c *ToolConfig) {
		c.NetworkBlock = enabled
	}
}

func WithDirenv(enabled bool) ToolOption {
	return func(c *ToolConfig) {
		c.Direnv = enabled
	}
}

func WithForceMCP(enabled bool) ToolOption {
	return func(c *ToolConfig) {
		c.ForceMCP = enabled
	}
}

func ApplyOptions(opts []ToolOption) ToolConfig {
	var cfg ToolConfig
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}
