package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/utils"
)

// TranslateLegacyVerification converts only legacy commands whose assertion
// semantics are unambiguous in the typed verifier model. The typed
// file_exists verifier means "any existing file or directory", so only test
// -e is equivalent. Regular-file (-f), directory (-d), and more complex shell
// commands deliberately remain command_exit to preserve their exact
// predicate/pipeline semantics.
func TranslateLegacyVerification(command string) (VerificationSpec, bool) {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return VerificationSpec{}, false
	}

	var path string
	switch {
	case len(fields) == 3 && fields[0] == "test" && fields[1] == "-e":
		path = fields[2]
	case len(fields) == 4 && fields[0] == "[" && fields[1] == "-e" && fields[3] == "]":
		path = fields[2]
	default:
		return VerificationSpec{}, false
	}

	// Keep the translator conservative: shell quoting, substitutions, and
	// operators require command_exit semantics and must not become a literal
	// path or silently lose part of the original command.
	if path == "" || strings.ContainsAny(path, "'\"`$;&|<>\n\r") {
		return VerificationSpec{}, false
	}
	return VerificationSpec{Type: VerifyFileExists, Path: path}, true
}

// NormalizeVerificationSpec fills in defaults and translates legacy command/mode.
func NormalizeVerificationSpec(spec VerificationSpec, legacyCommand, legacyMode string) VerificationSpec {
	normalized := cloneVerificationSpec(spec)
	if normalized.Type == "" {
		// An explicit typed shape wins. Only an otherwise untyped legacy command
		// is eligible for conservative migration.
		if normalized.Path == "" && len(normalized.Assertions) == 0 {
			if translated, ok := TranslateLegacyVerification(legacyCommand); ok {
				normalized.Type = translated.Type
				normalized.Path = translated.Path
			}
		}
		if normalized.Type == "" && normalized.Path != "" {
			if len(normalized.Assertions) > 0 {
				normalized.Type = VerifyJSONAssert
			} else {
				normalized.Type = VerifyFileExists
			}
		} else if normalized.Type == "" && len(normalized.Assertions) > 0 {
			normalized.Type = VerifyJSONAssert
		} else if normalized.Type == "" {
			normalized.Type = VerifyCommandExit
		}
	}
	if normalized.Command == "" && legacyCommand != "" && normalized.Type == VerifyCommandExit {
		normalized.Command = legacyCommand
	}
	if normalized.Mode == "" && legacyMode != "" {
		normalized.Mode = legacyMode
	}
	if normalized.Mode == "" {
		normalized.Mode = "success"
	}
	return normalized
}

// canonicalJSONAssertions returns assertions ordered by their full all-of
// contract. json_assert permits multiple assertions for one path, so sorting
// by path alone leaves equivalent contracts order-dependent.
func canonicalJSONAssertions(assertions []JSONAssertion) []JSONAssertion {
	canonical := append([]JSONAssertion(nil), assertions...)
	sort.SliceStable(canonical, func(i, j int) bool {
		if canonical[i].Path != canonical[j].Path {
			return canonical[i].Path < canonical[j].Path
		}
		return canonicalJSONAssertionValue(canonical[i].Equals) < canonicalJSONAssertionValue(canonical[j].Equals)
	})
	return canonical
}

func canonicalJSONAssertionValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Validation rejects non-scalar assertion values before execution. Keep
		// identity generation deterministic for malformed, pre-validation specs.
		return fmt.Sprintf("!invalid:%T:%v", value, value)
	}
	return string(encoded)
}

// ComputeVerificationFingerprint generates a deterministic evidence fingerprint.
// revision is the acceptance contract revision (empty for task-level verifications).
// securityMode is the execution security mode (e.g. "", "rbash", "no-net").
func ComputeVerificationFingerprint(spec VerificationSpec, result *VerificationResult, workDir string) string {
	return ComputeVerificationFingerprintFull(spec, result, workDir, "", "")
}

// ComputeVerificationFingerprintFull generates a fully qualified evidence fingerprint
// including acceptance contract revision and security/execution profile.
func ComputeVerificationFingerprintFull(spec VerificationSpec, result *VerificationResult, workDir, acceptanceRevision, securityMode string) string {
	h := sha256.New()
	exitCode := -1
	if result != nil {
		exitCode = result.ExitCode
	}
	_, _ = fmt.Fprintf(h, "type:%s|mode:%s|cmd:%s|path:%s|workdir:%s|exit:%d|",
		spec.Type, spec.Mode, spec.Command, spec.Path, workDir, exitCode)
	if acceptanceRevision != "" {
		_, _ = fmt.Fprintf(h, "revision:%s|", acceptanceRevision)
	}
	if securityMode != "" {
		_, _ = fmt.Fprintf(h, "security:%s|", securityMode)
	}

	// Canonically encode assertions by both path and JSON value so that all-of
	// contracts with repeated paths have an order-independent fingerprint.
	assertions := canonicalJSONAssertions(spec.Assertions)
	for _, a := range assertions {
		_, _ = fmt.Fprintf(h, "a:%s=%s|", a.Path, canonicalJSONAssertionValue(a.Equals))
	}

	targetPath := spec.Path
	if targetPath != "" {
		absPath := targetPath
		if !filepath.IsAbs(absPath) && workDir != "" {
			absPath = filepath.Join(workDir, targetPath)
		}
		if data, err := os.ReadFile(absPath); err == nil {
			fileHash := sha256.Sum256(data)
			_, _ = fmt.Fprintf(h, "filehash:%s|", hex.EncodeToString(fileHash[:]))
		} else {
			_, _ = fmt.Fprintf(h, "filestat:%v|", err != nil)
		}
	}

	return "vfp_" + hex.EncodeToString(h.Sum(nil))[:16]
}

// CheckWeakVerifierWarning checks whether a zero exit command output contains failure shapes.
func CheckWeakVerifierWarning(stdout, stderr string) (bool, string) {
	combined := strings.ToLower(stdout + "\n" + stderr)
	lines := strings.Split(combined, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "status: failed") ||
			strings.Contains(trimmed, `"status": "failed"`) ||
			strings.Contains(trimmed, `"status":"failed"`) ||
			strings.Contains(trimmed, "result: failure") ||
			strings.Contains(trimmed, "state: failed") ||
			strings.Contains(trimmed, "assertion failed") ||
			strings.Contains(trimmed, "test failed") ||
			strings.Contains(trimmed, "errors: [") ||
			strings.HasPrefix(trimmed, "error:") ||
			strings.HasPrefix(trimmed, "failed:") {
			return true, fmt.Sprintf("command exited 0 but output contains failure indicators: %q", utils.TruncateString(trimmed, 200))
		}
	}
	return false, ""
}

// CheckDefinitiveVerifierFailure recognizes the compact status emitted by
// process-based runners (for example, "failed -1"). Unlike the broader weak
// warning check, this shape is an explicit failure result and must not be
// accepted merely because the wrapper itself exited zero.
func CheckDefinitiveVerifierFailure(stdout, stderr string) (bool, string) {
	for _, line := range strings.Split(stdout+"\n"+stderr, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && strings.EqualFold(fields[0], "failed") {
			if _, err := strconv.Atoi(fields[1]); err == nil {
				return true, fmt.Sprintf("verifier reported explicit failure %q", strings.TrimSpace(line))
			}
		}
	}
	return false, ""
}

// EvaluateJSONAssertion checks a JSON path against expected value.
func EvaluateJSONAssertion(jsonData any, assertion JSONAssertion) error {
	path := strings.TrimSpace(assertion.Path)
	path = strings.TrimPrefix(path, ".")
	val, err := resolveJSONPath(jsonData, path)
	if err != nil {
		return err
	}
	if !equalJSONValues(val, assertion.Equals) {
		return fmt.Errorf("JSON assertion failed at %q: expected %v (%T), got %v (%T)", assertion.Path, assertion.Equals, assertion.Equals, val, val)
	}
	return nil
}

func resolveJSONPath(data any, path string) (any, error) {
	if path == "" || path == "." {
		return data, nil
	}
	parts := strings.Split(path, ".")
	curr := data
	for i, part := range parts {
		if curr == nil {
			return nil, fmt.Errorf("property %q is nil at part %q", strings.Join(parts[:i], "."), part)
		}
		switch v := curr.(type) {
		case map[string]any:
			val, ok := v[part]
			if !ok {
				return nil, fmt.Errorf("key %q not found", part)
			}
			curr = val
		case []any:
			idx, err := strconv.Atoi(part)
			if err != nil || idx < 0 || idx >= len(v) {
				return nil, fmt.Errorf("index %q out of bounds or invalid for slice", part)
			}
			curr = v[idx]
		default:
			return nil, fmt.Errorf("cannot dereference %q on type %T", part, curr)
		}
	}
	return curr, nil
}

func equalJSONValues(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	if reflect.DeepEqual(a, b) {
		return true
	}

	// JSON assertions use exact scalar equality. Numeric representations may
	// differ after decoding (for example, YAML gives an int while JSON retains
	// a json.Number), so compare numeric values as arbitrary-precision
	// rationals. In particular, never coerce a string such as "42" into the
	// number 42, and do not round integers larger than 2^53 through float64.
	numA, okA := toExactJSONNumber(a)
	numB, okB := toExactJSONNumber(b)
	if okA && okB {
		return numA.Cmp(numB) == 0
	}
	return false
}

// toExactJSONNumber converts supported numeric scalar representations without
// losing precision. json.Decoder.UseNumber keeps JSON source values out of
// float64; the float cases remain for programmatic callers of
// EvaluateJSONAssertion.
func toExactJSONNumber(v any) (*big.Rat, bool) {
	var text string
	switch n := v.(type) {
	case float32:
		text = strconv.FormatFloat(float64(n), 'g', -1, 32)
	case float64:
		text = strconv.FormatFloat(n, 'g', -1, 64)
	case int:
		text = strconv.Itoa(n)
	case int8:
		text = strconv.FormatInt(int64(n), 10)
	case int16:
		text = strconv.FormatInt(int64(n), 10)
	case int64:
		text = strconv.FormatInt(n, 10)
	case int32:
		text = strconv.FormatInt(int64(n), 10)
	case uint:
		text = strconv.FormatUint(uint64(n), 10)
	case uint8:
		text = strconv.FormatUint(uint64(n), 10)
	case uint16:
		text = strconv.FormatUint(uint64(n), 10)
	case uint32:
		text = strconv.FormatUint(uint64(n), 10)
	case uint64:
		text = strconv.FormatUint(n, 10)
	case json.Number:
		text = n.String()
	default:
		return nil, false
	}
	rational, ok := new(big.Rat).SetString(text)
	return rational, ok
}

// ExecuteVerificationSpec executes a typed verification specification.
func ExecuteVerificationSpec(parentCtx context.Context, shell, workDir string, spec VerificationSpec) (result *VerificationResult, err error) {
	return ExecuteVerificationSpecWithSteps(parentCtx, shell, workDir, spec, nil)
}

// ExecuteVerificationSpecWithSteps is ExecuteVerificationSpec plus the
// current task attempt's own tool-call/tool-result history, needed only by
// VerifyToolCallAssert. steps is nil for every non-task-attempt caller
// (acceptance/criteria verification, or a runtime-action task kind that
// never ran an agent conversation); VerifyToolCallAssert fails closed in that
// case rather than silently passing.
func ExecuteVerificationSpecWithSteps(parentCtx context.Context, shell, workDir string, spec VerificationSpec, steps []fantasy.StepResult) (result *VerificationResult, err error) {
	defer func() {
		if result != nil && result.EvaluatedAt.IsZero() {
			result.EvaluatedAt = time.Now().UTC()
		}
	}()
	spec = NormalizeVerificationSpec(spec, "", "")

	res := &VerificationResult{
		WorkDir: workDir,
		Spec:    &spec,
	}

	if err := validateVerificationSpec(spec); err != nil {
		res.ExitCode = -1
		res.Fingerprint = ComputeVerificationFingerprint(spec, res, workDir)
		return res, fmt.Errorf("malformed verification spec (failed closed): %w", err)
	}

	switch spec.Type {
	case VerifyCommandExit:
		res, err := executeCommandVerification(parentCtx, shell, workDir, spec)
		if res != nil {
			res.Fingerprint = ComputeVerificationFingerprint(spec, res, workDir)
		}
		return res, err

	case VerifyFileExists:
		res, err := executeFileExistsVerification(workDir, spec)
		if res != nil {
			res.Fingerprint = ComputeVerificationFingerprint(spec, res, workDir)
		}
		return res, err

	case VerifyFileAbsent:
		res, err := executeFileAbsentVerification(workDir, spec)
		if res != nil {
			res.Fingerprint = ComputeVerificationFingerprint(spec, res, workDir)
		}
		return res, err

	case VerifyJSONAssert:
		res, err := executeJSONAssertVerification(parentCtx, shell, workDir, spec)
		if res != nil {
			res.Fingerprint = ComputeVerificationFingerprint(spec, res, workDir)
		}
		return res, err

	case VerifyToolCallAssert:
		res, err := executeToolCallAssertVerification(workDir, spec, steps)
		if res != nil {
			res.Fingerprint = ComputeVerificationFingerprint(spec, res, workDir)
		}
		return res, err

	default:
		res.ExitCode = -1
		res.Fingerprint = ComputeVerificationFingerprint(spec, res, workDir)
		return res, fmt.Errorf("unknown verification type %q (failed closed)", spec.Type)
	}
}

func validateVerificationSpec(spec VerificationSpec) error {
	if err := validateVerificationMode(spec.Mode); err != nil {
		return err
	}
	switch spec.Type {
	case VerifyCommandExit:
		if strings.TrimSpace(spec.Command) == "" {
			return errors.New("command_exit verification requires a non-empty command")
		}
	case VerifyFileExists, VerifyFileAbsent:
		if strings.TrimSpace(spec.Path) == "" {
			return fmt.Errorf("%s verification requires a non-empty path", spec.Type)
		}
	case VerifyJSONAssert:
		if strings.TrimSpace(spec.Path) == "" && strings.TrimSpace(spec.Command) == "" {
			return errors.New("json_assert verification requires path or command")
		}
		if len(spec.Assertions) == 0 {
			return errors.New("json_assert verification requires at least one assertion")
		}
		for i, assertion := range spec.Assertions {
			if !isJSONScalar(assertion.Equals) {
				return fmt.Errorf("json_assert assertion %d requires a scalar equals value", i)
			}
		}
	case VerifyToolCallAssert:
		if len(spec.ToolCallAssertions) == 0 {
			return errors.New("tool_call_assert verification requires at least one assertion")
		}
		for i, assertion := range spec.ToolCallAssertions {
			if strings.TrimSpace(assertion.Tool) == "" {
				return fmt.Errorf("tool_call_assert assertion %d requires a non-empty tool name", i)
			}
			if assertion.MinCount < 0 {
				return fmt.Errorf("tool_call_assert assertion %d has a negative min_count", i)
			}
		}
	default:
		return fmt.Errorf("unsupported verification type %q", spec.Type)
	}
	return nil
}

func validateVerificationMode(mode string) error {
	switch mode {
	case "", "success", "expected_failure", "observation":
		return nil
	default:
		return fmt.Errorf("invalid verify_mode %q", mode)
	}
}

// isJSONScalar restricts json_assert to the intentionally small first
// delivery contract: exact equality for JSON scalar values only. In
// particular, accepting maps or slices here would silently extend the public
// assertion language beyond the documented scope and make config behaviour
// harder to preserve across YAML and JSON decoders.
func isJSONScalar(value any) bool {
	if value == nil {
		return true
	}
	switch value.(type) {
	case string, bool,
		float64, float32,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		json.Number:
		return true
	default:
		return false
	}
}

func isExpectedVerificationExit(err error, result *VerificationResult) bool {
	if result == nil || result.TimedOut || result.ExitCode == 0 || result.ExitCode == -1 {
		return false
	}
	// An unresolved verifier is an environment/configuration failure, not an
	// expected assertion failure. This must remain false even for
	// expected_failure and observation modes; otherwise a missing executable
	// can satisfy an acceptance gate merely by returning exit 127.
	if errors.Is(err, errUnresolvedExecutable) ||
		hasEnvironmentFailureSignal(result.Stdout) || hasEnvironmentFailureSignal(result.Stderr) {
		return false
	}
	return true
}

func applyVerificationMode(res *VerificationResult, err error, mode string) (*VerificationResult, error) {
	switch mode {
	case "", "success":
		return res, err

	case "expected_failure":
		if isExpectedVerificationExit(err, res) {
			return res, nil
		}
		if err == nil {
			return res, fmt.Errorf("verification expected a non-zero exit but succeeded")
		}
		return res, err

	case "observation":
		if err == nil || isExpectedVerificationExit(err, res) {
			return res, nil
		}
		return res, err

	default:
		return res, fmt.Errorf("invalid verify_mode %q", mode)
	}
}

// errUnresolvedExecutable marks a verifier that could not be started because
// one or more command-stage executables failed preflight resolution.
var errUnresolvedExecutable = errors.New("verification executable unresolved")

func executeCommandVerification(parentCtx context.Context, shell, workDir string, spec VerificationSpec) (*VerificationResult, error) {
	return executeCommandVerificationWithRawOutput(parentCtx, shell, workDir, spec, false)
}

// executeCommandVerificationWithRawOutput runs a command verifier. Full stdout
// is retained only for json_assert parsing; normal VerificationResult evidence
// must remain bounded both on disk and in memory.
func executeCommandVerificationWithRawOutput(parentCtx context.Context, shell, workDir string, spec VerificationSpec, retainRawOutput bool) (*VerificationResult, error) {
	command := strings.TrimSpace(spec.Command)
	if shell == "" {
		shell = "sh"
	}

	if err := parentCtx.Err(); err != nil {
		return nil, fmt.Errorf("task deadline exceeded before the verify command could run: %w", err)
	}

	ctx := parentCtx
	var cancel context.CancelFunc
	if _, hasDeadline := parentCtx.Deadline(); !hasDeadline {
		ctx, cancel = context.WithTimeout(parentCtx, 120*time.Second)
		defer cancel()
	}

	// Resolve every pipeline stage before invoking the shell. A missing
	// upstream command can be swallowed by a downstream stage (for example
	// `missing show 2>&1 | grep -c running`), leaving only an ordinary exit
	// status and making the failure look like a verifier-polarity problem.
	if findings := ResolveCommandExecutables(command, workDir); len(findings) > 0 {
		message := formatUnresolvedExecutableFindings(findings)
		res := &VerificationResult{
			Command:  command,
			WorkDir:  workDir,
			ExitCode: 127,
			Stderr:   utils.TruncateString(message, 2000),
			Spec:     &spec,
		}
		return applyVerificationMode(res, fmt.Errorf("%w: %s", errUnresolvedExecutable, message), spec.Mode)
	}

	cmd := exec.CommandContext(ctx, shell, "-c", command)
	cmd.Env = utils.SanitizeSubprocessEnv(os.Environ())
	if workDir != "" && workDir != "the hufu process working directory" {
		cmd.Dir = workDir
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	started := time.Now()
	err := cmd.Run()

	res := &VerificationResult{
		Command:  command,
		WorkDir:  workDir,
		ExitCode: 0,
		Stdout:   utils.TruncateString(strings.TrimSpace(stdout.String()), 2000),
		Stderr:   utils.TruncateString(strings.TrimSpace(stderr.String()), 2000),
		Duration: time.Since(started),
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
		Spec:     &spec,
	}
	if retainRawOutput {
		res.rawStdout = stdout.String()
	}

	if err != nil {
		res.ExitCode = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		}
		detail := strings.TrimSpace(strings.Join([]string{res.Stdout, res.Stderr}, "\n"))
		if detail != "" {
			detail = ": " + utils.TruncateString(detail, 500)
		}
		if res.TimedOut {
			if parentErr := parentCtx.Err(); parentErr != nil && res.Duration < 120*time.Second {
				return applyVerificationMode(res, fmt.Errorf("task deadline exceeded while the verify command was running (killed after %s): %w", res.Duration.Round(time.Millisecond), parentErr), spec.Mode)
			}
			return applyVerificationMode(res, fmt.Errorf("verification timed out after %s%s", res.Duration.Round(time.Millisecond), detail), spec.Mode)
		}
		if parentErr := parentCtx.Err(); parentErr != nil {
			return applyVerificationMode(res, fmt.Errorf("task context ended while the verify command was running: %w", parentErr), spec.Mode)
		}
		if res.ExitCode == 127 {
			err = fmt.Errorf("%v%s — exit 127 means the command was not found: the verify field must be a runnable shell command (e.g. 'test -f report.md'), not a natural-language description of the expected outcome", err, detail)
		} else if containsNonASCII(command) {
			err = fmt.Errorf("%w%s — the verify field appears to contain non-ASCII text (possibly natural language). The verify field must be a runnable shell command, e.g. 'test -f report.md' or 'virsh list --all | grep -c running', not a description of the expected outcome", err, detail)
		} else if res.ExitCode == 1 && strings.TrimSpace(res.Stdout) == "0" &&
			(strings.Contains(command, "grep -c") || strings.Contains(command, "grep-c")) {
			err = fmt.Errorf("%w: %v%s — wrong polarity: the verify command checked that a resource EXISTS (grep-c returned 0 = not found), but this looks like a cleanup task where success means the resource is GONE. Use '!' negation for delete/cleanup verify, e.g. '! ovs-vsctl show 2>&1 | grep -q br-verify'", errWrongVerificationPolarity, err, detail)
		} else {
			err = fmt.Errorf("%w%s", err, detail)
		}
	} else {
		if failed, reason := CheckDefinitiveVerifierFailure(res.Stdout, res.Stderr); failed {
			res.ExitCode = 1
			res.Overturned = true
			res.OverturnReason = reason
			return applyVerificationMode(res, errors.New(reason), spec.Mode)
		} else if isWeak, reason := CheckWeakVerifierWarning(res.Stdout, res.Stderr); isWeak {
			res.WeakWarning = true
			res.WeakReason = reason
		}
	}

	return applyVerificationMode(res, err, spec.Mode)
}

func executeFileExistsVerification(workDir string, spec VerificationSpec) (*VerificationResult, error) {
	target := strings.TrimSpace(spec.Path)
	absPath := target
	if !filepath.IsAbs(absPath) && workDir != "" {
		absPath = filepath.Join(workDir, target)
	}

	res := &VerificationResult{
		WorkDir: workDir,
		Spec:    &spec,
	}

	info, err := os.Stat(absPath)
	if err != nil {
		res.ExitCode = 1
		res.Stderr = fmt.Sprintf("file_exists failed: path %q does not exist", absPath)
		return applyVerificationMode(res, fmt.Errorf("required file/directory missing: %s", target), spec.Mode)
	}

	res.ExitCode = 0
	res.Stdout = fmt.Sprintf("file_exists passed: %s (size: %d)", absPath, info.Size())
	return applyVerificationMode(res, nil, spec.Mode)
}

func executeFileAbsentVerification(workDir string, spec VerificationSpec) (*VerificationResult, error) {
	target := strings.TrimSpace(spec.Path)
	absPath := target
	if !filepath.IsAbs(absPath) && workDir != "" {
		absPath = filepath.Join(workDir, target)
	}

	res := &VerificationResult{
		WorkDir: workDir,
		Spec:    &spec,
	}

	_, err := os.Stat(absPath)
	if os.IsNotExist(err) {
		res.ExitCode = 0
		res.Stdout = fmt.Sprintf("file_absent passed: %s does not exist", absPath)
		return applyVerificationMode(res, nil, spec.Mode)
	}
	if err == nil {
		res.ExitCode = 1
		res.Stderr = fmt.Sprintf("file_absent failed: path %q exists", absPath)
		return applyVerificationMode(res, fmt.Errorf("file/directory exists but was expected to be absent: %s", target), spec.Mode)
	}

	res.ExitCode = -1
	res.Stderr = fmt.Sprintf("file_absent error statting %q: %v", absPath, err)
	return applyVerificationMode(res, fmt.Errorf("stat failed for path %s: %w", target, err), spec.Mode)
}

func executeJSONAssertVerification(parentCtx context.Context, shell, workDir string, spec VerificationSpec) (*VerificationResult, error) {
	res := &VerificationResult{
		WorkDir: workDir,
		Spec:    &spec,
	}

	var jsonBytes []byte
	if spec.Path != "" {
		absPath := spec.Path
		if !filepath.IsAbs(absPath) && workDir != "" {
			absPath = filepath.Join(workDir, spec.Path)
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			res.ExitCode = 1
			res.Stderr = fmt.Sprintf("json_assert failed to read file %q: %v", absPath, err)
			return applyVerificationMode(res, fmt.Errorf("json_assert file missing or unreadable: %w", err), spec.Mode)
		}
		jsonBytes = data
	} else if spec.Command != "" {
		cmdRes, cmdErr := executeCommandVerificationWithRawOutput(parentCtx, shell, workDir, VerificationSpec{
			Type:    VerifyCommandExit,
			Mode:    "success",
			Command: spec.Command,
		}, true)
		if cmdErr != nil || (cmdRes != nil && cmdRes.ExitCode != 0) {
			res.ExitCode = 1
			if cmdRes != nil {
				res.Stdout = cmdRes.Stdout
				res.Stderr = cmdRes.Stderr
			}
			return applyVerificationMode(res, fmt.Errorf("json_assert command failed: %w", cmdErr), spec.Mode)
		}
		// The persisted result deliberately truncates stdout, but parsing a JSON
		// assertion must use the complete output from this single invocation.
		// Running the command again would be both nondeterministic and unsafe for
		// a verifier with side effects.
		jsonBytes = []byte(cmdRes.rawStdout)
	}

	var parsedData any
	decoder := json.NewDecoder(bytes.NewReader(jsonBytes))
	decoder.UseNumber()
	if err := decoder.Decode(&parsedData); err != nil {
		res.ExitCode = 1
		res.Stderr = fmt.Sprintf("json_assert invalid JSON output: %v", err)
		return applyVerificationMode(res, fmt.Errorf("json_assert output is not valid JSON: %w", err), spec.Mode)
	}
	// A verifier is expected to produce exactly one JSON document. Reject
	// trailing non-whitespace data rather than accepting an ambiguous prefix.
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		res.ExitCode = 1
		res.Stderr = "json_assert invalid JSON output: expected exactly one JSON document"
		return applyVerificationMode(res, errors.New("json_assert output must contain exactly one JSON document"), spec.Mode)
	}

	for _, assertion := range spec.Assertions {
		if err := EvaluateJSONAssertion(parsedData, assertion); err != nil {
			res.ExitCode = 1
			res.Stderr = fmt.Sprintf("json_assert assertion failed: %v", err)
			return applyVerificationMode(res, err, spec.Mode)
		}
	}

	res.ExitCode = 0
	res.Stdout = fmt.Sprintf("json_assert passed (%d assertion(s))", len(spec.Assertions))
	return applyVerificationMode(res, nil, spec.Mode)
}

// executeToolCallAssertVerification asserts directly against the current
// task attempt's own already-parsed tool-call/tool-result history -- no
// team-owned script re-parsing a serialized NDJSON transcript, and none of
// the JSON-escaping or SIGPIPE-under-pipefail failure modes that come with
// doing this in bash. steps is nil for every caller that has no per-attempt
// agent conversation (acceptance/criteria verification, non-agent task
// kinds); that fails closed below rather than silently passing.
func executeToolCallAssertVerification(workDir string, spec VerificationSpec, steps []fantasy.StepResult) (*VerificationResult, error) {
	res := &VerificationResult{
		WorkDir: workDir,
		Spec:    &spec,
	}

	if steps == nil {
		res.ExitCode = 1
		res.Stderr = "tool_call_assert failed: no task-attempt tool-call history is available in this verification context (acceptance/criteria and non-agent task kinds are not supported)"
		return applyVerificationMode(res, errors.New("tool_call_assert requires per-attempt tool-call history, which this verification context does not have"), spec.Mode)
	}

	var failures []string
	for i, assertion := range spec.ToolCallAssertions {
		minCount := assertion.MinCount
		if minCount <= 0 {
			minCount = 1
		}
		callCount := 0
		resultCount := 0
		for _, step := range steps {
			for _, call := range step.Content.ToolCalls() {
				if call.ToolName != assertion.Tool {
					continue
				}
				if assertion.InputContains == "" || strings.Contains(call.Input, assertion.InputContains) {
					callCount++
				}
			}
			if assertion.ResultContains != "" {
				for _, tr := range step.Content.ToolResults() {
					if tr.ToolName != assertion.Tool {
						continue
					}
					text, _ := toolResultOutputText(tr.Result)
					if strings.Contains(text, assertion.ResultContains) {
						resultCount++
					}
				}
			}
		}
		if callCount < minCount {
			failures = append(failures, fmt.Sprintf("assertion %d: tool %q with input containing %q called %d time(s), want at least %d", i, assertion.Tool, assertion.InputContains, callCount, minCount))
			continue
		}
		if assertion.ResultContains != "" && resultCount < minCount {
			failures = append(failures, fmt.Sprintf("assertion %d: tool %q result containing %q matched %d time(s), want at least %d", i, assertion.Tool, assertion.ResultContains, resultCount, minCount))
		}
	}

	if len(failures) > 0 {
		res.ExitCode = 1
		res.Stderr = "tool_call_assert failed: " + strings.Join(failures, "; ")
		return applyVerificationMode(res, fmt.Errorf("tool_call_assert failed: %s", strings.Join(failures, "; ")), spec.Mode)
	}

	res.ExitCode = 0
	res.Stdout = fmt.Sprintf("tool_call_assert passed (%d assertion(s))", len(spec.ToolCallAssertions))
	return applyVerificationMode(res, nil, spec.Mode)
}
