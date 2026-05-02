package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	WorkDir      string
	AllowedPaths []string
	PathConsent  *PathConsent
	ToolName     string
}

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

func WithToolName(name string) ToolOption {
	return func(c *ToolConfig) {
		c.ToolName = name
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
	if err := validateToolInput(call.Input, t.info); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
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

func validateRequired(input string, required []string) error {
	if input == "" || input == "{}" {
		for _, field := range required {
			return fmt.Errorf("parameter %q is required", field)
		}
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return nil
	}
	for _, field := range required {
		val, ok := raw[field]
		if !ok || val == nil {
			return fmt.Errorf("parameter %q is required", field)
		}
		if s, ok := val.(string); ok && s == "" {
			return fmt.Errorf("parameter %q must not be empty", field)
		}
	}
	return nil
}

func validateParamType(input string, paramName string, expectedType string) error {
	if input == "" || input == "{}" {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return nil
	}
	val, ok := raw[paramName]
	if !ok || val == nil {
		return nil
	}
	switch expectedType {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("parameter %q must be a string", paramName)
		}
	case "number":
		if _, ok := val.(float64); !ok {
			if _, ok := val.(int); !ok {
				return fmt.Errorf("parameter %q must be a number", paramName)
			}
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("parameter %q must be a boolean", paramName)
		}
	case "array":
		if _, ok := val.([]any); !ok {
			return fmt.Errorf("parameter %q must be an array", paramName)
		}
	}
	return nil
}

func validateToolInput(input string, info fantasy.ToolInfo) error {
	if err := validateRequired(input, info.Required); err != nil {
		return err
	}
	if input == "" || input == "{}" {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(input), &raw); err != nil {
		return nil
	}
	for paramName, paramDef := range info.Parameters {
		defMap, ok := paramDef.(map[string]any)
		if !ok {
			continue
		}
		typeVal, ok := defMap["type"].(string)
		if !ok {
			continue
		}
		if _, exists := raw[paramName]; exists {
			if err := validateParamType(input, paramName, typeVal); err != nil {
				return err
			}
		}
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

func resolveAndValidatePath(path, workDir string) (string, error) {
	absPath, err := resolvePathWithWorkDir(path, workDir)
	if err != nil {
		return "", err
	}
	projectDir := workDir
	if projectDir == "" {
		projectDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get working directory: %w", err)
		}
	}
	projectDir = filepath.Clean(projectDir)

	evaluatedProjDir, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve project directory: %w", err)
	}
	evaluatedProjDir = filepath.Clean(evaluatedProjDir)

	evaluatedPath, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		if !strings.HasPrefix(evaluatedPath, evaluatedProjDir+string(filepath.Separator)) && evaluatedPath != evaluatedProjDir {
			return "", fmt.Errorf("path '%s' is outside the project directory", path)
		}
		return evaluatedPath, nil
	}

	evaluatedDir, err := filepath.EvalSymlinks(filepath.Dir(absPath))
	if err != nil {
		return "", fmt.Errorf("path '%s' is invalid or cannot be resolved: %w", path, err)
	}

	if !strings.HasPrefix(evaluatedDir, evaluatedProjDir+string(filepath.Separator)) && evaluatedDir != evaluatedProjDir {
		return "", fmt.Errorf("path '%s' is outside the project directory", path)
	}
	return filepath.Join(evaluatedDir, filepath.Base(absPath)), nil
}

func isPathAllowed(absPath string, allowedPaths []string) bool {
	evalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		evalPath = absPath
	}
	evalPath = filepath.Clean(evalPath)

	checkPath := evalPath

	for _, allowed := range allowedPaths {
		evalAllowed, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			evalAllowed = allowed
		}
		evalAllowed = filepath.Clean(evalAllowed)

		if checkPath == evalAllowed || strings.HasPrefix(checkPath, evalAllowed+string(filepath.Separator)) {
			return true
		}

		parentDir := filepath.Dir(absPath)
		evalParent, err := filepath.EvalSymlinks(parentDir)
		if err == nil {
			evalParent = filepath.Clean(evalParent)
			if evalParent == evalAllowed || strings.HasPrefix(evalParent, evalAllowed+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func checkPathOrConsent(path, workDir, operation string, cfg ToolConfig) (string, error) {
	absPath, err := resolvePathWithWorkDir(path, workDir)
	if err != nil {
		return "", err
	}

	if isPathAllowed(absPath, cfg.AllowedPaths) {
		return absPath, nil
	}

	if cfg.PathConsent != nil {
		result, err := cfg.PathConsent.AskConsent(absPath, operation, cfg.ToolName, path)
		if err != nil {
			return "", fmt.Errorf("path '%s' is outside allowed paths and consent failed: %w", path, err)
		}
		switch result {
		case ConsentOnce, ConsentAlways:
			evalPath, evalErr := filepath.EvalSymlinks(absPath)
			if evalErr == nil {
				evalPath = filepath.Clean(evalPath)
				if evalPath != absPath && !isPathAllowed(evalPath, cfg.AllowedPaths) {
					return "", fmt.Errorf("path '%s' resolves to '%s' which is outside allowed paths", path, evalPath)
				}
			}
			return absPath, nil
		default:
			return "", fmt.Errorf("path '%s' is outside allowed paths — access denied by user", path)
		}
	}

	return "", fmt.Errorf("path '%s' is outside allowed paths", path)
}

func resolveAndValidatePathWithConsent(path string, cfg ToolConfig) (string, error) {
	absPath, err := resolvePathWithWorkDir(path, cfg.WorkDir)
	if err != nil {
		return "", err
	}

	if isPathAllowed(absPath, cfg.AllowedPaths) {
		return resolveAndValidatePath(path, cfg.WorkDir)
	}

	if cfg.PathConsent != nil {
		result, err := cfg.PathConsent.AskConsent(absPath, "edit", cfg.ToolName, path)
		if err != nil {
			return "", fmt.Errorf("path '%s' is outside allowed paths and consent failed: %w", path, err)
		}
		switch result {
		case ConsentOnce, ConsentAlways:
			projectDir := cfg.WorkDir
			if projectDir == "" {
				projectDir, _ = os.Getwd()
			}
			return resolveAndValidatePathForAllowedPath(absPath, projectDir, cfg.AllowedPaths)
		default:
			return "", fmt.Errorf("path '%s' is outside allowed paths — access denied by user", path)
		}
	}

	return "", fmt.Errorf("path '%s' is outside allowed paths", path)
}

func resolveAndValidatePathForAllowedPath(absPath, projectDir string, allowedPaths []string) (string, error) {
	evaluatedAbsPath, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		return validatedPathInAllowedDirs(evaluatedAbsPath, allowedPaths)
	}

	parentDir := filepath.Dir(absPath)
	evaluatedParent, err := filepath.EvalSymlinks(parentDir)
	if err != nil {
		return "", fmt.Errorf("path '%s' is invalid or cannot be resolved: %w", absPath, err)
	}

	result, err := validatedPathInAllowedDirs(evaluatedParent, allowedPaths)
	if err != nil {
		return "", err
	}
	return filepath.Join(result, filepath.Base(absPath)), nil
}

func validatedPathInAllowedDirs(evaluatedPath string, allowedPaths []string) (string, error) {
	evaluatedPath = filepath.Clean(evaluatedPath)
	for _, allowed := range allowedPaths {
		evalAllowed, err := filepath.EvalSymlinks(allowed)
		if err != nil {
			evalAllowed = allowed
		}
		evalAllowed = filepath.Clean(evalAllowed)

		if evaluatedPath == evalAllowed || strings.HasPrefix(evaluatedPath, evalAllowed+string(filepath.Separator)) {
			return evaluatedPath, nil
		}
	}
	return "", fmt.Errorf("path is outside allowed directories")
}

func AllTools(opts ...ToolOption) []fantasy.AgentTool {
	return []fantasy.AgentTool{
		NewBashTool(opts...),
		NewSudoTool(opts...),
		NewSshTool(opts...),
		NewViewTool(opts...),
		NewWriteTool(opts...),
		NewEditTool(opts...),
		NewMultiEditTool(opts...),
		NewGrepTool(opts...),
		NewGlobTool(opts...),
		NewLsTool(opts...),
		NewLuaTool(opts...),
		NewGolangTool(opts...),
		NewAskUserTool(opts...),
		NewDownloadTool(opts...),
		NewFetchTool(opts...),
		NewAgenticFetchTool(opts...),
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
