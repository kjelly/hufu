//go:build !linux && !darwin
// +build !linux,!darwin

package tools

import (
	"context"
	"sync"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/hooks"
)

type ToolOption func(*ToolConfig)

type PathReviewer func(ctx context.Context, command string, path string) (bool, error)

type ToolConfig struct {
	WorkDir         string
	AllowedPaths    []string
	PathConsent     *PathConsent
	PathReviewer    PathReviewer
	ToolName        string
	WorkspaceName   string
	Hooks           *hooks.HookRegistry
	RestrictedBash  bool
	RestrictedPath  string
	NetworkBlock    bool
	Direnv          bool
}

type PathConsent struct {
	mu           sync.Mutex
	remembered   []string
	denied       []string
	currentAgent func() AgentInfo
}

func (c *PathConsent) IsAllowed(path string) bool { return true }
func (c *PathConsent) SetAgentInfoSource(fn func() AgentInfo) {}
func (c *PathConsent) AskConsent(path, operation, toolName, toolArgs string) (ConsentResult, error) {
	return ConsentResult{Allowed: true}, nil
}
func (c *PathConsent) IsDenied(path string) bool { return false }
func (c *PathConsent) IsRemembered(path string) bool { return false }

type AgentInfo struct {
	Name string
	Task string
}

type ConsentResult struct {
	Allowed  bool
	Remember bool
	Reason   string
}

func WithWorkDir(dir string) ToolOption                     { return func(c *ToolConfig) { c.WorkDir = dir } }
func WithAllowedPaths(paths []string) ToolOption            { return func(c *ToolConfig) { c.AllowedPaths = paths } }
func WithPathConsent(consent *PathConsent) ToolOption       { return func(c *ToolConfig) { c.PathConsent = consent } }
func WithPathReviewer(reviewer PathReviewer) ToolOption    { return func(c *ToolConfig) { c.PathReviewer = reviewer } }
func WithToolName(name string) ToolOption                   { return func(c *ToolConfig) { c.ToolName = name } }
func WithWorkspaceName(name string) ToolOption             { return func(c *ToolConfig) { c.WorkspaceName = name } }
func WithHooks(h *hooks.HookRegistry) ToolOption            { return func(c *ToolConfig) { c.Hooks = h } }
func WithRestrictedBash(enabled bool) ToolOption               { return func(c *ToolConfig) { c.RestrictedBash = enabled } }
func WithRestrictedPath(path string) ToolOption             { return func(c *ToolConfig) { c.RestrictedPath = path } }
func WithNetworkBlock(enabled bool) ToolOption                    { return func(c *ToolConfig) { c.NetworkBlock = enabled } }
func WithDirenv(enabled bool) ToolOption                    { return func(c *ToolConfig) { c.Direnv = enabled } }

func ApplyOptions(opts []ToolOption) ToolConfig { return ToolConfig{} }

func AllTools(opts ...ToolOption) []fantasy.AgentTool       { return []fantasy.AgentTool{} }
func FilterTools(all []fantasy.AgentTool, allowed map[string]bool) []fantasy.AgentTool { return all }
func SetGuardReviewer(tools []fantasy.AgentTool, fn GuardReviewFn) {}
func SetPathReviewer(tools []fantasy.AgentTool, fn PathReviewer)    {}

type coreTool struct {
	info    fantasy.ToolInfo
	handler func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error)
	pOpts   fantasy.ProviderOptions
	hooks   *hooks.HookRegistry
}

func (t *coreTool) Info() fantasy.ToolInfo                          { return t.info }
func (t *coreTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *coreTool) SetProviderOptions(opts fantasy.ProviderOptions) {}
func (t *coreTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextErrorResponse("tool not available on this platform"), nil
}

func NewMathTool(opts ...ToolOption) fantasy.AgentTool {
	return &coreTool{info: fantasy.ToolInfo{Name: "math"}}
}

func SetOnAskUserTUI(fn func(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (string, bool)) {}
func SetOnAskUserStart(fn func()) {}
func SetOnAskUserDone(fn func()) {}
func SetAskUserActive(active bool) {}
func IsAskUserActive() bool { return false }

func TryAskUserTUI(ctx context.Context, question, qtype string, opts []AskUserTUIOption, allowAny bool) (string, bool) {
	return "", false
}

func NewPathConsent() *PathConsent                                  { return &PathConsent{} }
func NewPathConsentWithAgentInfo(fn func() AgentInfo) *PathConsent { return &PathConsent{} }

