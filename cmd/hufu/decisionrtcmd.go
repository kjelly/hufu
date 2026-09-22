package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend/rule"
)

const decisionRTDefaultProviderURL = "http://127.0.0.1:11434/v1"

type decisionRTDeps struct {
	registryFactory func(RegistryOptions) BackendRegistry
	stdin           io.Reader
	stdout          io.Writer
	stderr          io.Writer
	stdinIsTerminal func() bool
	getenv          func(string) string
}

type decisionRTExitError struct {
	code  int
	msg   string
	cause error
}

func (e *decisionRTExitError) Error() string {
	return e.msg
}

func (e *decisionRTExitError) Unwrap() error {
	return e.cause
}

func (e *decisionRTExitError) ProcessExitCode() int {
	return e.code
}

type decisionRTExecutionOptions struct {
	backend           string
	timeout           time.Duration
	minConfidence     float64
	requireCalibrated bool
	noFallback        bool
	sidecarModel      string
	providerURL       string
	providerAPIKey    string
	jsonOutput        bool
	receipt           bool
}

type decisionRTSpecOptions struct {
	id       string
	version  string
	purpose  string
	question string
	options  []string
	minimum  int64
	maximum  int64
	context  decisionRTContextOptions
}

func defaultDecisionRTDeps() decisionRTDeps {
	return decisionRTDeps{
		registryFactory: NewDefaultRegistry,
		stdin:           os.Stdin,
		stdout:          os.Stdout,
		stderr:          os.Stderr,
		stdinIsTerminal: func() bool { return term.IsTerminal(os.Stdin.Fd()) },
		getenv:          os.Getenv,
	}
}

func newDecisionRTCommand(deps decisionRTDeps) *cobra.Command {
	command := &cobra.Command{
		Use:               "decisionrt",
		Short:             "Run a bounded typed decision without an agent team",
		SilenceErrors:     true,
		SilenceUsage:      true,
		PersistentPreRun:  func(*cobra.Command, []string) {},
		Args:              decisionRTNoArgs(deps),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, _ []string) error {
			return reportDecisionRTError(deps, command, decisionRTInvalidRequest("a decisionrt subcommand is required"))
		},
	}
	command.SetIn(deps.stdin)
	command.SetOut(deps.stdout)
	command.SetErr(deps.stderr)
	command.SetFlagErrorFunc(decisionRTFlagError(deps))
	command.AddCommand(
		newDecisionRTChoiceCommand(deps),
		newDecisionRTBooleanCommand(deps),
		newDecisionRTIntegerCommand(deps),
		newDecisionRTRunCommand(deps),
		newDecisionRTValidateCommand(deps),
		newDecisionRTBackendsCommand(deps),
	)
	return command
}

func newDecisionRTChoiceCommand(deps decisionRTDeps) *cobra.Command {
	specOptions := new(decisionRTSpecOptions)
	execution := new(decisionRTExecutionOptions)
	command := newDecisionRTLeaf(deps, "choice", func(command *cobra.Command) error {
		if err := requireDecisionRTSpecFields(*specOptions); err != nil {
			return err
		}
		options, err := parseDecisionRTOptions(specOptions.options)
		if err != nil {
			return err
		}
		contextValues, err := buildDecisionRTContextForCommand(command, specOptions.context)
		if err != nil {
			return err
		}
		request := decisionrt.Request{
			Purpose: specOptions.purpose,
			Spec: decisionrt.Spec{
				ID: specOptions.id, Version: specOptions.version, Kind: decisionrt.KindChoice,
				Question: specOptions.question, Options: options,
			},
			Context: contextValues,
		}
		return executeDecisionRT(command, deps, request, *execution)
	})
	bindDecisionRTSpecFlags(command, specOptions, true)
	bindDecisionRTExecutionFlags(command, execution)
	return command
}

func newDecisionRTBooleanCommand(deps decisionRTDeps) *cobra.Command {
	specOptions := new(decisionRTSpecOptions)
	execution := new(decisionRTExecutionOptions)
	command := newDecisionRTLeaf(deps, "boolean", func(command *cobra.Command) error {
		if err := requireDecisionRTSpecFields(*specOptions); err != nil {
			return err
		}
		contextValues, err := buildDecisionRTContextForCommand(command, specOptions.context)
		if err != nil {
			return err
		}
		request := decisionrt.Request{
			Purpose: specOptions.purpose,
			Spec: decisionrt.Spec{
				ID: specOptions.id, Version: specOptions.version, Kind: decisionrt.KindBoolean, Question: specOptions.question,
			},
			Context: contextValues,
		}
		return executeDecisionRT(command, deps, request, *execution)
	})
	bindDecisionRTSpecFlags(command, specOptions, false)
	bindDecisionRTExecutionFlags(command, execution)
	return command
}

func newDecisionRTIntegerCommand(deps decisionRTDeps) *cobra.Command {
	specOptions := new(decisionRTSpecOptions)
	execution := new(decisionRTExecutionOptions)
	command := newDecisionRTLeaf(deps, "integer", func(command *cobra.Command) error {
		if err := requireDecisionRTSpecFields(*specOptions); err != nil {
			return err
		}
		if !command.Flags().Changed("min") || !command.Flags().Changed("max") {
			return decisionRTInvalidRequest("--min and --max are required")
		}
		contextValues, err := buildDecisionRTContextForCommand(command, specOptions.context)
		if err != nil {
			return err
		}
		request := decisionrt.Request{
			Purpose: specOptions.purpose,
			Spec: decisionrt.Spec{
				ID: specOptions.id, Version: specOptions.version, Kind: decisionrt.KindIntegerRange,
				Question: specOptions.question, Range: &decisionrt.IntegerRange{Min: specOptions.minimum, Max: specOptions.maximum},
			},
			Context: contextValues,
		}
		return executeDecisionRT(command, deps, request, *execution)
	})
	bindDecisionRTSpecFlags(command, specOptions, false)
	command.Flags().Int64Var(&specOptions.minimum, "min", 0, "Minimum allowed integer")
	command.Flags().Int64Var(&specOptions.maximum, "max", 0, "Maximum allowed integer")
	bindDecisionRTExecutionFlags(command, execution)
	return command
}

func newDecisionRTRunCommand(deps decisionRTDeps) *cobra.Command {
	input := new(decisionRTInputOptions)
	execution := new(decisionRTExecutionOptions)
	command := newDecisionRTLeaf(deps, "run", func(command *cobra.Command) error {
		if err := validateDecisionRTInputFlags(command, *input); err != nil {
			return err
		}
		request, err := readDecisionRTRequest(*input, deps.stdin, deps.stdinIsTerminal())
		if err != nil {
			return err
		}
		return executeDecisionRT(command, deps, request, *execution)
	})
	bindDecisionRTInputFlags(command, input)
	bindDecisionRTExecutionFlags(command, execution)
	return command
}

func newDecisionRTValidateCommand(deps decisionRTDeps) *cobra.Command {
	input := new(decisionRTInputOptions)
	var jsonOutput bool
	command := newDecisionRTLeaf(deps, "validate", func(command *cobra.Command) error {
		if err := validateDecisionRTInputFlags(command, *input); err != nil {
			return err
		}
		request, err := readDecisionRTRequest(*input, deps.stdin, deps.stdinIsTerminal())
		if err != nil {
			return err
		}
		digest, err := decisionrt.Digest(request)
		if err != nil {
			return err
		}
		return writeDecisionRTValidation(deps.stdout, digest, jsonOutput)
	})
	bindDecisionRTInputFlags(command, input)
	command.Flags().BoolVar(&jsonOutput, "json", false, "Write JSON to stdout")
	return command
}

func newDecisionRTBackendsCommand(deps decisionRTDeps) *cobra.Command {
	options := RegistryOptions{ProviderURL: decisionRTDefaultProviderURL}
	var jsonOutput bool
	command := newDecisionRTLeaf(deps, "backends", func(*cobra.Command) error {
		registry := deps.registryFactory(options)
		return writeDecisionRTBackends(deps.stdout, registry.List(), jsonOutput)
	})
	command.Flags().StringVar(&options.SidecarModel, "sidecar-model", "", "Sidecar model ID")
	command.Flags().StringVar(&options.ProviderURL, "provider-url", decisionRTDefaultProviderURL, "OpenAI-compatible provider URL")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Write JSON to stdout")
	return command
}

func newDecisionRTLeaf(deps decisionRTDeps, name string, handler func(*cobra.Command) error) *cobra.Command {
	command := &cobra.Command{
		Use:               name,
		SilenceErrors:     true,
		SilenceUsage:      true,
		Args:              decisionRTNoArgs(deps),
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := rejectDecisionRTInheritedFlags(command); err != nil {
				return reportDecisionRTError(deps, command, err)
			}
			if err := handler(command); err != nil {
				return reportDecisionRTError(deps, command, err)
			}
			return nil
		},
	}
	command.SetFlagErrorFunc(decisionRTFlagError(deps))
	return command
}

func executeDecisionRT(command *cobra.Command, deps decisionRTDeps, request decisionrt.Request, options decisionRTExecutionOptions) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if options.timeout <= 0 || options.timeout > 10*time.Second {
		return decisionRTInvalidRequest("--timeout must be within (0,10s]")
	}
	if command.Flags().Changed("min-confidence") && (math.IsNaN(options.minConfidence) || math.IsInf(options.minConfidence, 0) || options.minConfidence < 0 || options.minConfidence > 1) {
		return decisionRTInvalidRequest("--min-confidence must be within [0,1]")
	}

	apiKey := options.providerAPIKey
	if apiKey == "" {
		apiKey = deps.getenv("HUFU_PROVIDER_API_KEY")
	}
	registry := deps.registryFactory(RegistryOptions{
		SidecarModel: options.sidecarModel, ProviderURL: options.providerURL, ProviderAPIKey: apiKey,
	})
	primary, err := registry.Resolve(command.Context(), options.backend)
	if err != nil {
		return err
	}
	var fallback decisionrt.Backend
	if options.backend == "sidecar" && !options.noFallback {
		fallback = rule.AlwaysAbstain()
	}
	policy := decisionrt.AcceptancePolicy{RequireCalibratedConfidence: options.requireCalibrated}
	if command.Flags().Changed("min-confidence") || options.requireCalibrated {
		policy.MinConfidence = new(options.minConfidence)
	}
	runtime, err := decisionrt.NewRuntime(decisionrt.RuntimeConfig{
		Primary: primary, Fallback: fallback, Policy: policy, Timeout: options.timeout,
	})
	if err != nil {
		return err
	}
	result, receipt, err := runtime.Decide(command.Context(), request)
	if err != nil {
		return err
	}
	if err := writeDecisionRTResult(deps.stdout, result, receipt, options.jsonOutput, options.receipt); err != nil {
		return err
	}
	if result.Status == decisionrt.StatusAbstained {
		return &decisionRTExitError{code: 3, msg: "decisionrt: abstained"}
	}
	return nil
}

func bindDecisionRTSpecFlags(command *cobra.Command, options *decisionRTSpecOptions, includeOptions bool) {
	flags := command.Flags()
	flags.StringVar(&options.id, "id", "", "Decision specification ID")
	flags.StringVar(&options.version, "version", "", "Decision specification version")
	flags.StringVar(&options.purpose, "purpose", "", "Stable decision purpose")
	flags.StringVar(&options.question, "question", "", "Bounded decision question")
	if includeOptions {
		flags.StringArrayVar(&options.options, "option", nil, "Candidate as ID=description (repeatable)")
	}
	flags.StringArrayVar(&options.context.values, "context", nil, "Context scalar as key=value (repeatable)")
	flags.StringVar(&options.context.jsonText, "context-json", "", "Context JSON object")
	flags.StringVar(&options.context.file, "context-file", "", "Path to a context JSON object")
}

func bindDecisionRTExecutionFlags(command *cobra.Command, options *decisionRTExecutionOptions) {
	flags := command.Flags()
	flags.StringVar(&options.backend, "backend", "rule", "Decision backend: rule or sidecar")
	flags.DurationVar(&options.timeout, "timeout", 2*time.Second, "Per-attempt timeout")
	flags.Float64Var(&options.minConfidence, "min-confidence", 0, "Minimum accepted confidence")
	flags.BoolVar(&options.requireCalibrated, "require-calibrated", false, "Require calibrated confidence")
	flags.BoolVar(&options.noFallback, "no-fallback", false, "Disable the sidecar-to-rule fallback")
	flags.StringVar(&options.sidecarModel, "sidecar-model", "", "Sidecar model ID")
	flags.StringVar(&options.providerURL, "provider-url", decisionRTDefaultProviderURL, "OpenAI-compatible provider URL")
	flags.StringVar(&options.providerAPIKey, "provider-api-key", "", "Provider API key")
	flags.BoolVar(&options.jsonOutput, "json", false, "Write JSON to stdout")
	flags.BoolVar(&options.receipt, "receipt", false, "Write a JSON result and receipt envelope")
}

func bindDecisionRTInputFlags(command *cobra.Command, options *decisionRTInputOptions) {
	command.Flags().StringVar(&options.file, "file", "", "Read a request JSON object from this file")
	command.Flags().BoolVar(&options.stdin, "stdin", false, "Read a request JSON object from stdin")
}

func parseDecisionRTOptions(values []string) ([]decisionrt.Option, error) {
	options := make([]decisionrt.Option, 0, len(values))
	for _, value := range values {
		id, description, ok := strings.Cut(value, "=")
		if !ok || id == "" {
			return nil, decisionRTInvalidRequest("--option must use ID=description")
		}
		options = append(options, decisionrt.Option{ID: id, Description: description})
	}
	return options, nil
}

func buildDecisionRTContextForCommand(command *cobra.Command, options decisionRTContextOptions) (map[string]any, error) {
	if command.Flags().Changed("context-json") && options.jsonText == "" {
		return nil, decisionRTInvalidRequest("--context-json requires a JSON object")
	}
	if command.Flags().Changed("context-file") && options.file == "" {
		return nil, decisionRTInvalidRequest("--context-file requires a path")
	}
	return buildDecisionRTContext(options)
}

func validateDecisionRTInputFlags(command *cobra.Command, options decisionRTInputOptions) error {
	if command.Flags().Changed("file") && options.file == "" {
		return decisionRTInvalidRequest("--file requires a path")
	}
	return nil
}

func requireDecisionRTSpecFields(options decisionRTSpecOptions) error {
	if options.id == "" || options.version == "" || options.purpose == "" || options.question == "" {
		return decisionRTInvalidRequest("--id, --version, --purpose, and --question are required")
	}
	return nil
}

func rejectDecisionRTInheritedFlags(command *cobra.Command) error {
	for _, name := range []string{"workspace", "decision-profile", "profile", "execution-profile", "goal-mode", "no-color"} {
		flag := command.Flag(name)
		if flag != nil && flag.Changed {
			return decisionRTInvalidRequest("inherited flags are not supported")
		}
	}
	return nil
}

func decisionRTNoArgs(deps decisionRTDeps) cobra.PositionalArgs {
	return func(command *cobra.Command, args []string) error {
		if err := cobra.NoArgs(command, args); err != nil {
			return reportDecisionRTError(deps, command, decisionRTInvalidRequest("positional arguments are not supported"))
		}
		return nil
	}
}

func decisionRTFlagError(deps decisionRTDeps) func(*cobra.Command, error) error {
	return func(command *cobra.Command, cause error) error {
		return reportDecisionRTError(deps, command, &decisionrt.RuntimeError{Kind: decisionrt.ErrorInvalidRequest, Err: cause})
	}
}

func reportDecisionRTError(deps decisionRTDeps, command *cobra.Command, cause error) error {
	var exitError *decisionRTExitError
	if errors.As(cause, &exitError) {
		return cause
	}
	code := decisionRTErrorCode(cause)
	message := decisionRTDiagnostic(code)
	_, _ = fmt.Fprintf(deps.stderr, "hufu decisionrt %s: %s\n", command.Name(), message)
	return &decisionRTExitError{code: code, msg: "decisionrt: " + message, cause: cause}
}

func decisionRTErrorCode(err error) int {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 4
	}
	if typed, ok := errors.AsType[*decisionrt.RuntimeError](err); ok {
		if typed == nil {
			return 4
		}
		switch typed.Kind {
		case decisionrt.ErrorInvalidRequest:
			return 2
		case decisionrt.ErrorBackendFailure, decisionrt.ErrorInvalidBackendOutput:
			return 4
		case decisionrt.ErrorConfiguration, decisionrt.ErrorBackendUnavailable:
			return 5
		}
	}
	return 4
}

func decisionRTDiagnostic(code int) string {
	switch code {
	case 2:
		return "invalid request or usage"
	case 4:
		return "backend or runtime failure"
	case 5:
		return "configuration or backend unavailable"
	default:
		return "operation failed"
	}
}
