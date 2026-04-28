package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"charm.land/fantasy"
)

var StdinMu sync.Mutex

var askUserActive atomic.Int32

var onAskUserDone func()

func SetOnAskUserDone(fn func()) {
	onAskUserDone = fn
}

func SetAskUserActive(active bool) {
	if active {
		askUserActive.Store(1)
	} else {
		askUserActive.Store(0)
		if onAskUserDone != nil {
			onAskUserDone()
		}
	}
}

func IsAskUserActive() bool {
	return askUserActive.Load() == 1
}

type ToolOption func(*ToolConfig)

type ToolConfig struct {
	WorkDir string
}

func WithWorkDir(dir string) ToolOption {
	return func(c *ToolConfig) {
		c.WorkDir = dir
	}
}

func ApplyOptions(opts []ToolOption) ToolConfig {
	var cfg ToolConfig
	for _, o := range opts {
		o(&cfg)
	}
	return cfg
}

type coreTool struct {
	info    fantasy.ToolInfo
	handler func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error)
	pOpts   fantasy.ProviderOptions
}

func (t *coreTool) Info() fantasy.ToolInfo                          { return t.info }
func (t *coreTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *coreTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *coreTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return t.handler(ctx, call)
}

func parseArgs(input string, target any) error {
	if input == "" || input == "{}" {
		return nil
	}
	if err := json.Unmarshal([]byte(input), target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func resolvePathWithWorkDir(path, workDir string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	baseDir := workDir
	if baseDir == "" {
		var err error
		baseDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get working directory: %w", err)
		}
	}
	return filepath.Clean(filepath.Join(baseDir, path)), nil
}

func AllTools(opts ...ToolOption) []fantasy.AgentTool {
	return []fantasy.AgentTool{
		NewBashTool(opts...),
		NewReadTool(opts...),
		NewWriteTool(opts...),
		NewEditTool(opts...),
		NewGrepTool(opts...),
		NewFindTool(opts...),
		NewLsTool(opts...),
		NewLuaTool(opts...),
		NewGolangTool(opts...),
		NewAskUserTool(opts...),
	}
}

func FilterTools(all []fantasy.AgentTool, allowed map[string]bool) []fantasy.AgentTool {
	if len(allowed) == 0 {
		return all
	}
	var filtered []fantasy.AgentTool
	for _, t := range all {
		if allowed[t.Info().Name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}
