package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/golangruntime"
	"github.com/kjelly/hufu/internal/utils"
)

// Action represents a generic structured action intent.
type Action struct {
	// Capability selects the registered provider. It is bound by a static team
	// contract and is never accepted from the coordinator's agent tool.
	Capability string `json:"capability" yaml:"capability"`
	Type       string `json:"type" yaml:"type"`
	Payload    string `json:"payload" yaml:"payload"`
	// InputBindings are repository-authored replacements into Payload. They are
	// consumed before task admission and must never cross the coordinator or
	// provider JSON boundary.
	InputBindings []ActionInputBinding `json:"-" yaml:"input-bindings,omitempty"`
}

// ActionResult is the provider-neutral result envelope. Artifact identity and
// provenance are rewritten by the coordinator after the provider returns; a
// provider must never be able to smuggle an already trusted reference through
// this boundary.
type ActionResult struct {
	Outputs   map[string]any `json:"outputs,omitempty"`
	Artifacts []ArtifactRef  `json:"artifacts,omitempty"`
}

func decodeActionResult(value interface{}) (ActionResult, error) {
	if value == nil {
		return ActionResult{}, nil
	}
	if result, ok := value.(ActionResult); ok {
		return result, nil
	}
	if result, ok := value.(*ActionResult); ok {
		if result == nil {
			return ActionResult{}, nil
		}
		return *result, nil
	}
	// Preserve the pre-envelope scalar adapter behavior while all structured
	// results use the bounded JSON envelope below. Existing providers can adopt
	// the envelope incrementally without changing the execution boundary.
	if text, ok := value.(string); ok {
		return ActionResult{Outputs: map[string]any{"result": text}}, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ActionResult{}, fmt.Errorf("encode action result: %w", err)
	}
	var result ActionResult
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return ActionResult{}, fmt.Errorf("decode action result envelope: %w", err)
	}
	if result.Outputs != nil {
		for name, output := range result.Outputs {
			if strings.TrimSpace(name) == "" {
				return ActionResult{}, fmt.Errorf("action result contains an empty output name")
			}
			if _, err := json.Marshal(output); err != nil {
				return ActionResult{}, fmt.Errorf("action result output %q is not JSON-serializable: %w", name, err)
			}
		}
	}
	return result, nil
}

func encodeActionResult(result ActionResult) string {
	encoded, err := json.Marshal(result)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// ActionProvider defines the generic mechanism to validate and execute actions.
type ActionProvider interface {
	Validate(action Action) error
	Execute(ctx context.Context, action Action) (interface{}, error)
}

// RunInputResolverProvider is the provider-neutral admission seam for a
// deterministic, side-effect-free run-input resolver. Implementations receive
// the resolver envelope directly rather than an Action so coordinator-authored
// payload fields cannot enter this boundary.
type RunInputResolverProvider interface {
	ResolveRunInput(context.Context, RunInputResolverRequest) (RunInputResolverResponse, error)
}

// NamedActionProvider optionally exposes the stable adapter identity used in
// lifecycle telemetry. Capability and provider are deliberately separate:
// one capability may be backed by different adapters in different teams.
type NamedActionProvider interface {
	ProviderName() string
}

// ProviderRegistry holds the registered action providers.
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]ActionProvider
}

// NewProviderRegistry creates a new registry.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
		providers: make(map[string]ActionProvider),
	}
}

// Register registers an action provider for a capability.
func (r *ProviderRegistry) Register(capability string, provider ActionProvider) {
	capability = normalizeCapability(capability)
	if capability == "" || provider == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[capability] = provider
}

// Get returns the provider for a capability.
func (r *ProviderRegistry) Get(capability string) (ActionProvider, bool) {
	if r == nil {
		return nil, false
	}
	capability = normalizeCapability(capability)
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[capability]
	return p, ok
}

// Has checks if a capability is registered.
func (r *ProviderRegistry) Has(capability string) bool {
	if r == nil {
		return false
	}
	capability = normalizeCapability(capability)
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.providers[capability]
	return ok
}

// ProviderName returns a stable identity for the provider bound to a
// capability. Unnamed providers retain a useful type identity for telemetry.
func (r *ProviderRegistry) ProviderName(capability string) string {
	provider, ok := r.Get(capability)
	if !ok || provider == nil {
		return ""
	}
	if named, ok := provider.(NamedActionProvider); ok && strings.TrimSpace(named.ProviderName()) != "" {
		return strings.TrimSpace(named.ProviderName())
	}
	return fmt.Sprintf("%T", provider)
}

// Clone returns an isolated registry snapshot. Team-specific adapter
// registration therefore cannot mutate the process-wide default registry or
// leak one team's provider configuration into another team.
func (r *ProviderRegistry) Clone() (*ProviderRegistry, error) {
	clone := NewProviderRegistry()
	if r == nil {
		return clone, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for capability, provider := range r.providers {
		if provider == nil {
			return nil, fmt.Errorf("provider registry contains nil provider for %q", capability)
		}
		clone.providers[capability] = provider
	}
	return clone, nil
}

func normalizeCapability(capability string) string {
	return strings.ToLower(strings.TrimSpace(capability))
}

// DefaultProviderRegistry is the global registry.
var DefaultProviderRegistry = NewProviderRegistry()

// commandActionProvider is a domain-neutral process adapter. A configured
// command receives the Action JSON on stdin and must emit one JSON value on
// stdout. Individual teams can bind any external system behind this boundary
// without teaching Hufu about its executable, action format, or semantics.
type commandActionProvider struct {
	capability string
	command    []string
	dir        string
	timeout    time.Duration
}

type golangRuntimeExecutor func(context.Context, golangruntime.Program, []byte, []string, int, int) (golangruntime.Result, error)

// golangActionProvider executes a maintainer-authored static source package
// through Hufu's embedded Go runtime. The source is trusted and may launch
// processes or access files; those domain capabilities remain in the script,
// not in Hufu's provider contract.
type golangActionProvider struct {
	capability string
	program    golangruntime.Program
	timeout    time.Duration
	execute    golangRuntimeExecutor
}

func (p *commandActionProvider) ProviderName() string {
	if p == nil || len(p.command) == 0 {
		return "command"
	}
	return "command:" + p.command[0]
}

func registerConfiguredActionProviders(registry *ProviderRegistry, configs map[string]agent.ActionProviderConfig, teamDir string) error {
	if len(configs) == 0 {
		return nil
	}
	if registry == nil {
		return fmt.Errorf("action provider registry is nil")
	}
	for rawCapability, config := range configs {
		capability := normalizeCapability(rawCapability)
		if capability == "" {
			return fmt.Errorf("action-providers contains an empty capability")
		}
		if config.Timeout < 0 {
			return fmt.Errorf("action provider %q timeout cannot be negative", capability)
		}
		runtimeName := strings.ToLower(strings.TrimSpace(config.Runtime))
		switch runtimeName {
		case "", "command":
			if strings.TrimSpace(config.Source) != "" || strings.TrimSpace(config.Mode) != "" {
				return fmt.Errorf("action provider %q command runtime does not accept source or mode", capability)
			}
			if len(config.Command) == 0 || strings.TrimSpace(config.Command[0]) == "" {
				return fmt.Errorf("action provider %q requires a command", capability)
			}
			registry.Register(capability, &commandActionProvider{
				capability: capability,
				command:    append([]string(nil), config.Command...),
				dir:        strings.TrimSpace(config.Dir),
				timeout:    time.Duration(config.Timeout) * time.Second,
			})
		case "golang":
			if len(config.Command) > 0 || strings.TrimSpace(config.Dir) != "" {
				return fmt.Errorf("action provider %q golang runtime does not accept command or dir", capability)
			}
			if strings.TrimSpace(config.Mode) != golangruntime.TrustedStaticMode {
				return fmt.Errorf("action provider %q golang runtime requires mode %q", capability, golangruntime.TrustedStaticMode)
			}
			program, err := golangruntime.Prepare(teamDir, config.Source)
			if err != nil {
				return fmt.Errorf("action provider %q: %w", capability, err)
			}
			registry.Register(capability, &golangActionProvider{
				capability: capability,
				program:    program,
				timeout:    time.Duration(config.Timeout) * time.Second,
				execute:    executeGolangRuntime,
			})
		default:
			return fmt.Errorf("action provider %q has unsupported runtime %q", capability, config.Runtime)
		}
	}
	return nil
}

func executeGolangRuntime(ctx context.Context, program golangruntime.Program, input []byte, env []string, outputLimit, errorLimit int) (golangruntime.Result, error) {
	return golangruntime.Execute(ctx, "", program, input, env, outputLimit, errorLimit)
}

func (p *commandActionProvider) Validate(action Action) error {
	if p == nil || len(p.command) == 0 {
		return fmt.Errorf("provider is not configured")
	}
	if normalizeCapability(action.Capability) != p.capability {
		return fmt.Errorf("action capability %q does not match provider capability %q", action.Capability, p.capability)
	}
	if strings.TrimSpace(action.Type) == "" {
		return fmt.Errorf("action type is required")
	}
	return nil
}

func (p *golangActionProvider) ProviderName() string {
	if p == nil || strings.TrimSpace(p.program.Digest) == "" {
		return "golang"
	}
	return "golang:" + p.program.Digest
}

func (p *golangActionProvider) Validate(action Action) error {
	if p == nil || strings.TrimSpace(p.program.Source) == "" || p.execute == nil {
		return fmt.Errorf("provider is not configured")
	}
	if normalizeCapability(action.Capability) != p.capability {
		return fmt.Errorf("action capability %q does not match provider capability %q", action.Capability, p.capability)
	}
	if strings.TrimSpace(action.Type) == "" {
		return fmt.Errorf("action type is required")
	}
	return nil
}

type actionEnvironmentKey struct{}

// ActionEnvironment encapsulates runtime paths and identity passed to action provider adapters.
type ActionEnvironment struct {
	Workspace          string
	Repository         string
	TeamName           string
	RunID              string
	TaskID             string
	Attempt            int
	ActionInvocationID string
}

// WithActionEnvironment attaches ActionEnvironment to a context.
func WithActionEnvironment(ctx context.Context, env ActionEnvironment) context.Context {
	return context.WithValue(ctx, actionEnvironmentKey{}, env)
}

// ActionEnvironmentFromContext extracts ActionEnvironment from a context.
func ActionEnvironmentFromContext(ctx context.Context) ActionEnvironment {
	if ctx == nil {
		return ActionEnvironment{}
	}
	val, _ := ctx.Value(actionEnvironmentKey{}).(ActionEnvironment)
	return val
}

func (p *commandActionProvider) Execute(ctx context.Context, action Action) (interface{}, error) {
	if err := p.Validate(action); err != nil {
		return nil, err
	}
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	payload, err := json.Marshal(action)
	if err != nil {
		return nil, fmt.Errorf("encode action: %w", err)
	}
	cmd := exec.CommandContext(ctx, p.command[0], p.command[1:]...)
	cmd.Dir = p.dir
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = actionCommandEnvironment(ctx)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("adapter command failed: %s", utils.RedactSecrets(detail))
	}
	var result interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("adapter command returned invalid JSON: %w", err)
	}
	return result, nil
}

func (p *golangActionProvider) Execute(ctx context.Context, action Action) (interface{}, error) {
	if err := p.Validate(action); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(action)
	if err != nil {
		return nil, fmt.Errorf("encode action: %w", err)
	}
	result, err := p.run(ctx, payload, 1024*1024, maxRunInputResolverDiagnosticBytes)
	if err != nil {
		return nil, fmt.Errorf("go action failed: %w", err)
	}
	if result.StdoutTruncated {
		return nil, fmt.Errorf("go action output exceeds %d bytes", 1024*1024)
	}
	var value interface{}
	if err := json.Unmarshal(result.Stdout, &value); err != nil {
		return nil, fmt.Errorf("go action returned invalid JSON: %w", err)
	}
	return value, nil
}

func (p *commandActionProvider) ResolveRunInput(ctx context.Context, request RunInputResolverRequest) (RunInputResolverResponse, error) {
	if p == nil || len(p.command) == 0 {
		return RunInputResolverResponse{}, fmt.Errorf("resolver provider is not configured")
	}
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return RunInputResolverResponse{}, fmt.Errorf("encode resolver request: %w", err)
	}
	cmd := exec.CommandContext(ctx, p.command[0], p.command[1:]...)
	cmd.Dir = p.dir
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = actionCommandEnvironment(ctx)
	stdout := newBoundedBuffer(maxRunInputResolverOutputBytes)
	stderr := newBoundedBuffer(maxRunInputResolverDiagnosticBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return RunInputResolverResponse{}, ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return RunInputResolverResponse{}, fmt.Errorf("resolver command failed: %s", utils.RedactSecrets(detail))
	}
	if stdout.Truncated() {
		return RunInputResolverResponse{}, fmt.Errorf("resolver output exceeds %d bytes", maxRunInputResolverOutputBytes)
	}
	var response RunInputResolverResponse
	decoder := json.NewDecoder(strings.NewReader(stdout.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return RunInputResolverResponse{}, fmt.Errorf("decode resolver response: %w", err)
	}
	if err := ensureRunInputJSONEOF(decoder); err != nil {
		return RunInputResolverResponse{}, fmt.Errorf("decode resolver response: %w", err)
	}
	if err := validateRunInputResolverResponse(response); err != nil {
		return RunInputResolverResponse{}, err
	}
	return response, nil
}

func (p *golangActionProvider) ResolveRunInput(ctx context.Context, request RunInputResolverRequest) (RunInputResolverResponse, error) {
	if p == nil || strings.TrimSpace(p.program.Source) == "" || p.execute == nil {
		return RunInputResolverResponse{}, fmt.Errorf("resolver provider is not configured")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return RunInputResolverResponse{}, fmt.Errorf("encode resolver request: %w", err)
	}
	result, err := p.run(ctx, payload, maxRunInputResolverOutputBytes, maxRunInputResolverDiagnosticBytes)
	if err != nil {
		return RunInputResolverResponse{}, fmt.Errorf("go resolver failed: %w", err)
	}
	if result.StdoutTruncated {
		return RunInputResolverResponse{}, fmt.Errorf("resolver output exceeds %d bytes", maxRunInputResolverOutputBytes)
	}
	var response RunInputResolverResponse
	decoder := json.NewDecoder(bytes.NewReader(result.Stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return RunInputResolverResponse{}, fmt.Errorf("decode resolver response: %w", err)
	}
	if err := ensureRunInputJSONEOF(decoder); err != nil {
		return RunInputResolverResponse{}, fmt.Errorf("decode resolver response: %w", err)
	}
	if err := validateRunInputResolverResponse(response); err != nil {
		return RunInputResolverResponse{}, err
	}
	return response, nil
}

func (p *golangActionProvider) run(ctx context.Context, payload []byte, outputLimit, errorLimit int) (golangruntime.Result, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	result, err := p.execute(ctx, p.program, payload, actionCommandEnvironment(ctx), outputLimit, errorLimit)
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	detail := strings.TrimSpace(string(result.Stderr))
	if detail == "" {
		detail = err.Error()
	}
	if result.StderrTruncated {
		detail += " (diagnostic truncated)"
	}
	return result, errors.New(utils.RedactSecrets(detail))
}

func actionCommandEnvironment(ctx context.Context) []string {
	env := os.Environ()
	actionEnv := ActionEnvironmentFromContext(ctx)
	if actionEnv.Workspace != "" {
		env = append(env, "HUFU_WORKSPACE="+actionEnv.Workspace)
	}
	if actionEnv.Repository != "" {
		env = append(env, "HUFU_REPOSITORY="+actionEnv.Repository)
	}
	if actionEnv.TeamName != "" {
		env = append(env, "HUFU_TEAM="+actionEnv.TeamName)
	}
	if actionEnv.RunID != "" {
		env = append(env, "HUFU_RUN_ID="+actionEnv.RunID)
	}
	if actionEnv.TaskID != "" {
		env = append(env, "HUFU_TASK_ID="+actionEnv.TaskID)
	}
	if actionEnv.Attempt > 0 {
		env = append(env, fmt.Sprintf("HUFU_ATTEMPT=%d", actionEnv.Attempt))
	}
	if actionEnv.ActionInvocationID != "" {
		env = append(env, "HUFU_ACTION_INVOCATION_ID="+actionEnv.ActionInvocationID)
	}
	return env
}

// ActionProviderError represents a provider execution error.
type ActionProviderError struct {
	Capability string
	Cause      error
}

func (e ActionProviderError) Error() string {
	return fmt.Sprintf("provider for %q failed: %v", e.Capability, e.Cause)
}

func (e ActionProviderError) Unwrap() error { return e.Cause }

// ActionValidationError marks a permanent invalid-action response from a
// provider. It is distinct from an execution failure so the retry engine does
// not repeatedly send an invalid action to an adapter.
type ActionValidationError struct {
	Capability string
	Cause      error
}

func (e ActionValidationError) Error() string {
	return fmt.Sprintf("provider for %q rejected action: %v", e.Capability, e.Cause)
}

func (e ActionValidationError) Unwrap() error { return e.Cause }
