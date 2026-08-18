//go:build !linux && !darwin
// +build !linux,!darwin

package tools

import "charm.land/fantasy"

// Stub implementations for non-Linux/Darwin platforms.
// Type definitions are in types_common.go shared across all platforms.

func (c *PathConsent) IsAllowed(path string) bool             { return true }
func (c *PathConsent) SetAgentInfoSource(fn func() AgentInfo) {}
func (c *PathConsent) AskConsent(path, operation, toolName, toolArgs string) (ConsentResult, string, error) {
	return ConsentAlways, "", nil
}
func (c *PathConsent) IsDenied(path string) bool     { return false }
func (c *PathConsent) IsRemembered(path string) bool { return false }

func NewPathConsent() *PathConsent {
	return &PathConsent{currentAgent: func() AgentInfo { return AgentInfo{} }}
}

func NewPathConsentWithAgentInfo(fn func() AgentInfo) *PathConsent {
	if fn == nil {
		return NewPathConsent()
	}
	return &PathConsent{currentAgent: fn}
}

func NewTeamPathConsent(teamDir string) (*PathConsent, error) { return NewPathConsent(), nil }

type PathConsentPolicy struct {
	Allowed []string
	Denied  []string
}

func PathConsentPolicyPath(teamDir string) string { return "" }
func LoadPathConsentPolicy(teamDir string) (PathConsentPolicy, error) {
	return PathConsentPolicy{}, nil
}
func SavePathConsentPolicy(teamDir string, policy PathConsentPolicy) error { return nil }
func UpdatePathConsentPolicy(teamDir, action, path string) (PathConsentPolicy, error) {
	return PathConsentPolicy{}, nil
}

func WithWorkDir(dir string) ToolOption           { return func(c *ToolConfig) { c.WorkDir = dir } }
func WithAllowedPaths(p []string) ToolOption      { return func(c *ToolConfig) { c.AllowedPaths = p } }
func WithPathConsent(pc *PathConsent) ToolOption  { return func(c *ToolConfig) { c.PathConsent = pc } }
func WithPathReviewer(pr PathReviewer) ToolOption { return func(c *ToolConfig) { c.PathReviewer = pr } }
func WithArtifactOpener(op ArtifactOpener) ToolOption {
	return func(c *ToolConfig) { c.ArtifactOpener = op }
}
func WithToolName(n string) ToolOption       { return func(c *ToolConfig) { c.ToolName = n } }
func WithWorkspaceName(n string) ToolOption  { return func(c *ToolConfig) { c.WorkspaceName = n } }
func WithHooks(h any) ToolOption             { return func(c *ToolConfig) {} }
func WithRestrictedBash(b bool) ToolOption   { return func(c *ToolConfig) { c.RestrictedBash = b } }
func WithRestrictedPath(p string) ToolOption { return func(c *ToolConfig) { c.RestrictedPath = p } }
func WithNetworkBlock(b bool) ToolOption     { return func(c *ToolConfig) { c.NetworkBlock = b } }
func WithDirenv(b bool) ToolOption           { return func(c *ToolConfig) { c.Direnv = b } }
func WithForceMCP(b bool) ToolOption         { return func(c *ToolConfig) { c.ForceMCP = b } }

func ApplyOptions(opts []ToolOption) ToolConfig {
	var cfg ToolConfig
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

// AllTools is unavailable on platforms without the supported terminal and
// filesystem implementation. Returning an empty set keeps discovery and
// cross-platform builds explicit rather than exposing non-functional tools.
func AllTools(opts ...ToolOption) []fantasy.AgentTool { return nil }
