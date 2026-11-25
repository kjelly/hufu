package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/build"
	"hash"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/processutil"
)

const goTestSummaryHalf = 4096

type goTestLimits struct {
	Package     time.Duration
	Command     time.Duration
	Reserve     time.Duration
	OutputBytes int
}

func defaultGoTestLimits() goTestLimits {
	// The team's 25m action timeout leaves time for snapshot preparation,
	// command teardown and durable artifact publication.
	return goTestLimits{Package: 15 * time.Minute, Command: 20 * time.Minute, Reserve: 30 * time.Second, OutputBytes: 64 * 1024 * 1024}
}

// goTestOutput retains the complete merged stream on disk and only a bounded
// head/tail in memory. Hitting the disk budget is an error, never valid evidence.
type goTestOutput struct {
	file        *os.File
	digest      hash.Hash
	cancel      context.CancelFunc
	limit       int
	count       int
	head        []byte
	tail        []byte
	probe       string
	testTimeout bool
	err         error
}

func (o *goTestOutput) Write(data []byte) (int, error) {
	if o.err != nil {
		return 0, o.err
	}
	if len(data) > o.limit-o.count {
		o.err = errors.New("go test diagnostic exceeds durable output budget")
		o.cancel()
		return 0, o.err
	}
	n, err := o.file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	_, _ = o.digest.Write(data[:n])
	o.count += n
	remaining := goTestSummaryHalf - len(o.head)
	if remaining > len(data[:n]) {
		remaining = n
	}
	o.head = append(o.head, data[:remaining]...)
	o.tail = append(o.tail, data[:n]...)
	if len(o.tail) > goTestSummaryHalf {
		o.tail = append([]byte(nil), o.tail[len(o.tail)-goTestSummaryHalf:]...)
	}
	// This is the Go test runner's alarm diagnostic, not task-prose parsing.
	// Keep overlap so a pipe write splitting the marker cannot hide the alarm.
	probe := o.probe + string(data[:n])
	if strings.HasPrefix(probe, "panic: test timed out after ") || strings.Contains(probe, "\npanic: test timed out after ") {
		o.testTimeout = true
	}
	if len(probe) > 64 {
		probe = probe[len(probe)-64:]
	}
	o.probe = probe
	if err != nil {
		o.err = err
		o.cancel()
	}
	return n, err
}

func (o *goTestOutput) summary() string {
	if o.count <= len(o.head) {
		return string(o.head)
	}
	if o.count <= 2*goTestSummaryHalf {
		return string(o.head) + string(o.tail[len(o.tail)-(o.count-len(o.head)):])
	}
	return string(o.head) + "\n[summary shortened; complete output is in output_artifact_id]\n" + string(o.tail)
}

func snapshotGoTestCommands(snapshot *reviewSourceSnapshot, packages []string, packageTimeout time.Duration) [][]string {
	grouped := make(map[string][]string)
	for _, pkg := range packages {
		module := "."
		if snapshot.identity != nil {
			if root, _ := snapshot.goModule(path.Clean(pkg)); root != "" && root != "." {
				module = root
				pkg = strings.TrimPrefix(pkg, "./"+root)
				if pkg == "" {
					pkg = "."
				} else {
					pkg = "." + pkg
				}
			}
		}
		grouped[module] = append(grouped[module], pkg)
	}
	roots := make([]string, 0, len(grouped))
	for root := range grouped {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	commands := make([][]string, 0, len(roots))
	for _, root := range roots {
		command := []string{"go"}
		if root != "." {
			command = append(command, "-C", filepath.FromSlash(root))
		}
		command = append(command, "test", "-p", "2", "-mod=readonly", "-count=1", "-timeout", packageTimeout.String())
		commands = append(commands, append(command, grouped[root]...))
	}
	return commands
}

type goModuleFile struct {
	Module goModuleVersion
	Use    []struct {
		DiskPath string
	}
	Replace []goModuleReplacement
}

type goModuleVersion struct {
	Path    string
	Version string
}

type goModuleReplacement struct {
	Old goModuleVersion
	New goModuleVersion
}

var errSnapshotGoDependency = errors.New("frozen Go module configuration has an unavailable local dependency")

func frozenGoPath(root, directory, name string) (string, error) {
	if !filepath.IsAbs(name) {
		name = filepath.Join(directory, name)
	}
	name = filepath.Clean(name)
	if !pathWithin(root, name) {
		return "", fmt.Errorf("%w: %s escapes the snapshot", errSnapshotGoDependency, name)
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", errSnapshotGoDependency, name, err)
	}
	if !pathWithin(root, resolved) {
		return "", fmt.Errorf("%w: %s resolves outside the snapshot", errSnapshotGoDependency, name)
	}
	return resolved, nil
}

func readFrozenGoModule(ctx context.Context, root, filename, kind string, environment []string, cache map[string]goModuleFile) (goModuleFile, error) {
	if file, ok := cache[filename]; ok {
		return file, nil
	}
	if _, err := frozenGoPath(root, root, filename); err != nil {
		return goModuleFile{}, err
	}
	probe := exec.CommandContext(ctx, "go", kind, "edit", "-json", filename)
	probe.Dir = filepath.Dir(filename)
	probe.Env = replaceEnvironmentValue(environment, "GOWORK", "off")
	output := &cappedBuffer{limit: 1024 * 1024}
	probe.Stdout = output
	if err := processutil.RunContextCommand(probe, 5*time.Second); err != nil {
		return goModuleFile{}, fmt.Errorf("%w: parse %s: %v", errSnapshotGoDependency, filename, err)
	}
	var file goModuleFile
	if err := json.Unmarshal([]byte(output.String()), &file); err != nil || output.truncated {
		return goModuleFile{}, fmt.Errorf("%w: invalid or oversized %s", errSnapshotGoDependency, filename)
	}
	cache[filename] = file
	return file, nil
}

func checkFrozenGoReplacements(root, directory string, file goModuleFile, workspace []goModuleReplacement) error {
	for _, replacement := range file.Replace {
		if replacement.New.Version != "" || goWorkspaceOverrides(workspace, replacement.Old) {
			continue
		}
		if err := checkFrozenGoReplacement(root, directory, replacement); err != nil {
			return err
		}
	}
	return nil
}

func checkFrozenGoReplacement(root, directory string, replacement goModuleReplacement) error {
	dependency, err := frozenGoPath(root, directory, replacement.New.Path)
	if err != nil {
		return err
	}
	_, err = frozenGoPath(root, dependency, "go.mod")
	return err
}

func goWorkspaceOverrides(workspace []goModuleReplacement, old goModuleVersion) bool {
	for _, replacement := range workspace {
		if replacement.Old.Path == old.Path && (replacement.Old.Version == "" || replacement.Old.Version == old.Version) {
			return true
		}
	}
	return false
}

func frozenGoWorkspace(ctx context.Context, root, module string, environment []string, cache map[string]goModuleFile) (string, goModuleFile, error) {
	moduleRoot := filepath.Join(root, module)
	for directory := moduleRoot; ; directory = filepath.Dir(directory) {
		filename := filepath.Join(directory, "go.work")
		if _, err := os.Lstat(filename); !os.IsNotExist(err) {
			workspace, err := readFrozenGoModule(ctx, root, filename, "work", environment, cache)
			if err != nil {
				return "", goModuleFile{}, err
			}
			for _, use := range workspace.Use {
				member := filepath.Clean(use.DiskPath)
				if !filepath.IsAbs(member) {
					member = filepath.Join(directory, member)
				}
				if member == moduleRoot {
					return filename, workspace, nil
				}
			}
			break
		}
		if directory == root {
			break
		}
	}
	return "off", goModuleFile{}, nil
}

func snapshotGoTestWorkspace(ctx context.Context, root, module string, environment []string, cache map[string]goModuleFile) (string, error) {
	filename, workspace, err := frozenGoWorkspace(ctx, root, module, environment, cache)
	if err != nil {
		return "", err
	}
	return filename, checkFrozenGoTestModule(ctx, root, module, filename, workspace, environment, cache)
}

func checkFrozenGoTestModule(ctx context.Context, root, module, filename string, workspace goModuleFile, environment []string, cache map[string]goModuleFile) error {
	if filename != "off" {
		return checkFrozenGoWorkspace(ctx, root, filename, workspace, environment, cache)
	}
	return checkFrozenGoModule(ctx, root, filepath.Join(root, module), environment, cache, nil)
}

func checkFrozenGoWorkspace(ctx context.Context, root, filename string, workspace goModuleFile, environment []string, cache map[string]goModuleFile) error {
	directory := filepath.Dir(filename)
	if err := checkFrozenGoReplacements(root, directory, workspace, nil); err != nil {
		return err
	}
	shadowed := make(map[string]error)
	for _, use := range workspace.Use {
		member, err := frozenGoPath(root, directory, use.DiskPath)
		if err != nil {
			return err
		}
		file, err := readFrozenGoModule(ctx, root, filepath.Join(member, "go.mod"), "mod", environment, cache)
		if err != nil {
			return err
		}
		for _, replacement := range file.Replace {
			if replacement.New.Version != "" || goWorkspaceOverrides(workspace.Replace, replacement.Old) {
				continue
			}
			if err := checkFrozenGoReplacement(root, member, replacement); err != nil {
				if replacement.Old.Version != "" || !hasExactWorkspaceReplacement(workspace.Replace, replacement.Old.Path) {
					return err
				}
				shadowed[replacement.Old.Path] = err
			}
		}
	}
	return verifyShadowedWorkspaceReplacements(ctx, filename, workspace.Replace, shadowed, environment)
}

func hasExactWorkspaceReplacement(workspace []goModuleReplacement, module string) bool {
	for _, replacement := range workspace {
		if replacement.Old.Path == module && replacement.Old.Version != "" {
			return true
		}
	}
	return false
}

// Ask Go which versions are active without granting the probe access to any
// escaping replacement. Exact workspace directives take precedence over the
// guard wildcard. The original workspace can be used only after every guard
// is shown to be shadowed by an exact directive.
func verifyShadowedWorkspaceReplacements(ctx context.Context, filename string, replacements []goModuleReplacement, shadowed map[string]error, environment []string) error {
	if len(shadowed) == 0 {
		return nil
	}
	source, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	guard, err := os.CreateTemp(filepath.Dir(filename), ".review-go-work-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(guard.Name()) }()
	modules := make([]string, 0, len(shadowed))
	for module := range shadowed {
		modules = append(modules, module)
	}
	sort.Strings(modules)
	if err := writeGuardedGoWorkspace(source, guard, modules); err != nil {
		return err
	}
	args := append([]string{"list", "-m", "-mod=readonly", "-json", "--"}, modules...)
	probe := exec.CommandContext(ctx, "go", args...)
	probe.Dir = filepath.Dir(filename)
	probe.Env = replaceEnvironmentValue(environment, "GOWORK", guard.Name())
	output := &cappedBuffer{limit: 1024 * 1024}
	probe.Stdout = output
	if err := processutil.RunContextCommand(probe, 5*time.Second); err != nil || output.truncated {
		return fmt.Errorf("%w: unable to verify replacement versions", shadowed[modules[0]])
	}
	decoder := json.NewDecoder(strings.NewReader(output.String()))
	active := make(map[string]bool)
	for {
		var module goModuleVersion
		if err := decoder.Decode(&module); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("%w: invalid replacement version evidence", shadowed[modules[0]])
		}
		active[module.Path] = module.Version != "" && goWorkspaceOverrides(replacements, module)
	}
	for _, module := range modules {
		if !active[module] {
			return shadowed[module]
		}
	}
	return nil
}

func writeGuardedGoWorkspace(source, guard *os.File, modules []string) error {
	const limit = 1024 * 1024
	count, err := io.Copy(guard, io.LimitReader(source, limit+1))
	if err == nil && count > limit {
		err = fmt.Errorf("%w: workspace exceeds size limit", errSnapshotGoDependency)
	}
	for _, module := range modules {
		if err != nil {
			break
		}
		// The destination is the guard file itself and cannot hold a go.mod.
		_, err = fmt.Fprintf(guard, "\nreplace %s => ./%s\n", strconv.Quote(module), filepath.Base(guard.Name()))
	}
	return errors.Join(err, guard.Close())
}

func checkFrozenGoModule(ctx context.Context, root, directory string, environment []string, cache map[string]goModuleFile, workspace []goModuleReplacement) error {
	file, err := readFrozenGoModule(ctx, root, filepath.Join(directory, "go.mod"), "mod", environment, cache)
	if err != nil {
		return err
	}
	return checkFrozenGoReplacements(root, directory, file, workspace)
}

func (p *snapshotGoTestPlan) resolveImporters(ctx context.Context, root string, environment []string) error {
	if len(p.importers) == 0 {
		return nil
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	environment = withSnapshotGoBuildEnvironment(environment)
	cache := make(map[string]goModuleFile)
	modules := make([]string, 0, len(p.importers))
	for module := range p.importers {
		modules = append(modules, module)
	}
	sort.Strings(modules)
	for _, module := range modules {
		paths := make([]string, 0, len(p.importers[module]))
		for imported, callers := range p.importers[module] {
			for _, caller := range callers {
				if !p.directories[path.Dir(caller.path)] {
					paths = append(paths, imported)
					break
				}
			}
		}
		if len(paths) == 0 {
			continue
		}
		filename, workspace, err := frozenGoWorkspace(ctx, root, module, environment, cache)
		if err != nil {
			return err
		}
		paths, err = p.frozenLocalImports(ctx, root, module, filename, workspace, environment, cache, paths)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			continue
		}
		if err := checkFrozenGoTestModule(ctx, root, module, filename, workspace, environment, cache); err != nil {
			return err
		}
		env := replaceEnvironmentValue(environment, "GOWORK", filename)
		packages, err := listSnapshotGoImports(ctx, filepath.Join(root, module), env, paths)
		if err != nil {
			return err
		}
		for _, pkg := range packages {
			dir, err := filepath.Rel(root, pkg.Dir)
			if err != nil || !p.selected[filepath.ToSlash(dir)] {
				continue
			}
			for _, importer := range p.importers[module][pkg.ImportPath] {
				dir := path.Dir(importer.path)
				if p.directories[dir] {
					continue
				}
				matched, err := matchesSnapshotGoBuild(root, importer)
				if err != nil {
					return err
				}
				if matched {
					p.directories[dir] = true
				}
			}
		}
	}
	return nil
}

// Cross-module imports can reach a different frozen directory only through
// an effective workspace member or local module replacement. Probe Go's
// actual resolution for those routes without preflighting unrelated imports.
func (p *snapshotGoTestPlan) frozenLocalImports(ctx context.Context, root, module, filename string, workspace goModuleFile, env []string, cache map[string]goModuleFile, paths []string) ([]string, error) {
	possible := make(map[string]bool)
	match := func(prefix, base string) {
		for _, imported := range paths {
			suffix, ok := strings.CutPrefix(imported, prefix)
			if !ok || suffix != "" && !strings.HasPrefix(suffix, "/") {
				continue
			}
			physical, err := frozenGoPath(root, base, strings.TrimPrefix(suffix, "/"))
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(root, physical)
			dir := filepath.ToSlash(rel)
			if err == nil && p.selected[dir] && p.directories[dir] {
				possible[imported] = true
			}
		}
	}
	matchLocal := func(directory string, replacements, overrides []goModuleReplacement) {
		for _, replacement := range replacements {
			if replacement.New.Version != "" || goWorkspaceOverrides(overrides, replacement.Old) {
				continue
			}
			base, err := frozenGoPath(root, directory, replacement.New.Path)
			if err == nil {
				match(replacement.Old.Path, base)
			}
		}
	}
	if filename == "off" {
		directory := filepath.Join(root, module)
		file, err := readFrozenGoModule(ctx, root, filepath.Join(directory, "go.mod"), "mod", env, cache)
		if err != nil {
			return nil, err
		}
		matchLocal(directory, file.Replace, nil)
	} else {
		directory := filepath.Dir(filename)
		matchLocal(directory, workspace.Replace, nil)
		for _, use := range workspace.Use {
			member, err := frozenGoPath(root, directory, use.DiskPath)
			if err != nil {
				continue
			}
			file, err := readFrozenGoModule(ctx, root, filepath.Join(member, "go.mod"), "mod", env, cache)
			if err != nil {
				return nil, err
			}
			match(file.Module.Path, member)
			matchLocal(member, file.Replace, workspace.Replace)
		}
	}
	selected := make([]string, 0, len(possible))
	for imported := range possible {
		selected = append(selected, imported)
	}
	return selected, nil
}

func listSnapshotGoImports(ctx context.Context, root string, environment, paths []string) ([]listedGoPackage, error) {
	sort.Strings(paths)
	arguments := append([]string{"list", "-e", "-find", "-mod=readonly", "-json=Dir,ImportPath", "--"}, paths...)
	command := exec.CommandContext(ctx, "go", arguments...)
	command.Dir = root
	command.Env = environment
	output, diagnostic := &cappedBuffer{limit: 1024 * 1024}, &cappedBuffer{limit: 8192}
	command.Stdout, command.Stderr = output, diagnostic
	if err := processutil.RunContextCommand(command, 5*time.Second); err != nil {
		return nil, fmt.Errorf("resolve frozen Go imports: %w: %s", err, strings.TrimSpace(diagnostic.String()))
	}
	if output.truncated {
		return nil, errors.New("frozen Go import metadata exceeded the limit")
	}
	decoder := json.NewDecoder(strings.NewReader(output.String()))
	var packages []listedGoPackage
	for {
		var pkg listedGoPackage
		if err := decoder.Decode(&pkg); err != nil {
			if errors.Is(err, io.EOF) {
				return packages, nil
			}
			return nil, fmt.Errorf("decode frozen Go import metadata: %w", err)
		}
		packages = append(packages, pkg)
	}
}

func withSnapshotGoBuildEnvironment(environment []string) []string {
	cgo := "0"
	if build.Default.CgoEnabled {
		cgo = "1"
	}
	environment = replaceEnvironmentValue(environment, "CGO_ENABLED", cgo)
	environment = replaceEnvironmentValue(environment, "GOOS", build.Default.GOOS)
	return replaceEnvironmentValue(environment, "GOARCH", build.Default.GOARCH)
}

func snapshotGoTestEnvironments(ctx context.Context, snapshot *reviewSourceSnapshot, commands [][]string, environment []string) ([][]string, error) {
	environments := make([][]string, len(commands))
	if snapshot.identity == nil {
		for i := range environments {
			environments[i] = environment
		}
		return environments, nil
	}
	root, err := filepath.EvalSymlinks(snapshot.root)
	if err != nil {
		return nil, err
	}
	environment = withSnapshotGoBuildEnvironment(environment)
	cache := make(map[string]goModuleFile)
	for i, arguments := range commands {
		module := "."
		if arguments[1] == "-C" {
			module = arguments[2]
		}
		workspace, err := snapshotGoTestWorkspace(ctx, root, module, environment, cache)
		if err != nil {
			return nil, err
		}
		environments[i] = replaceEnvironmentValue(environment, "GOWORK", workspace)
	}
	return environments, nil
}

func executeSnapshotGoTests(ctx context.Context, root string, commands, environments [][]string, output *goTestOutput) ([][]string, error) {
	executed := [][]string{}
	var runErr error
	for i, arguments := range commands {
		if ctx.Err() != nil {
			break
		}
		if len(commands) > 1 {
			if _, err := fmt.Fprintf(output, "\n$ %q\n", arguments); err != nil {
				break
			}
		}
		command := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
		command.Dir = root
		command.Env = environments[i]
		// A shared writer makes os/exec serialize stdout/stderr into one stream.
		command.Stdout = output
		command.Stderr = command.Stdout
		executed = append(executed, arguments)
		if err := processutil.RunContextCommand(command, 5*time.Second); err != nil {
			runErr = err
			var exitErr *exec.ExitError
			if output.testTimeout || !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				break
			}
		}
	}
	return executed, runErr
}

type snapshotGoTestTargets struct {
	packages []string
	plan     snapshotGoTestPlan
}

func runSnapshotGoTests(ctx context.Context, snapshot *reviewSourceSnapshot, targets snapshotGoTestTargets, environment []string, outputPath string, limits goTestLimits) (goTestVerification, error) {
	if limits.Package <= 0 || limits.Command <= 0 || limits.OutputBytes <= 0 || limits.Reserve < 0 {
		return goTestVerification{}, errors.New("invalid Go test execution limits")
	}
	budget := limits.Command
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline)-limits.Reserve < budget {
		budget = time.Until(deadline) - limits.Reserve
	}
	if budget <= 0 {
		return goTestVerification{}, errors.New("no time remains to run and seal Go test evidence")
	}
	packageTimeout := limits.Package
	// Reserve teardown time even when a caller supplies a shorter deadline.
	if budget/2 < packageTimeout && budget < limits.Package+5*time.Second {
		packageTimeout = budget / 2
	}
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return goTestVerification{}, fmt.Errorf("create complete Go test output: %w", err)
	}
	output := &goTestOutput{file: file, digest: sha256.New(), cancel: cancel, limit: limits.OutputBytes}
	packages := targets.packages
	if targets.plan.selected != nil {
		packages = targets.plan.packages()
	}
	var runErr error
	if environment == nil && len(packages) > 0 {
		environment, runErr = sanitizedGoTestEnvironment(runCtx)
	}
	if runErr == nil && len(packages) > 0 && targets.plan.selected != nil {
		runErr = targets.plan.resolveImporters(runCtx, snapshot.root, environment)
		if runErr == nil {
			packages = targets.plan.packages()
		}
	}
	commands := snapshotGoTestCommands(snapshot, packages, packageTimeout)
	var executed [][]string
	if runErr == nil && len(commands) > 0 {
		var environments [][]string
		environments, runErr = snapshotGoTestEnvironments(runCtx, snapshot, commands, environment)
		if runErr == nil {
			executed, runErr = executeSnapshotGoTests(runCtx, snapshot.root, commands, environments, output)
		}
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if output.err != nil || syncErr != nil || closeErr != nil {
		return goTestVerification{}, fmt.Errorf("preserve complete Go test output: %w", errors.Join(output.err, syncErr, closeErr))
	}
	return summarizeSnapshotGoTests(ctx, runCtx, snapshot, packages, commands, executed, output, runErr), nil
}

func summarizeSnapshotGoTests(ctx, runCtx context.Context, snapshot *reviewSourceSnapshot, packages []string, commands, executed [][]string, output *goTestOutput, runErr error) goTestVerification {
	exitCode := 0
	if runErr != nil {
		exitCode = -1
		// The trusted-static interpreter does not expose errors.AsType yet.
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	kind := "none"
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		kind = "canceled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		kind = "caller_deadline"
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) && len(executed) == 0:
		kind = "preflight"
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		kind = "command"
	case output.testTimeout && runErr != nil:
		kind = "test"
	}
	if kind != "none" && exitCode == 0 {
		exitCode = -1
	}
	diagnostic := output.summary()
	if runErr != nil || kind != "none" {
		diagnostic = fmt.Sprintf("Go test exit=%d; timeout_kind=%s\n", exitCode, kind) + diagnostic
		if output.count == 0 && runErr != nil {
			diagnostic += runErr.Error()
		}
	}
	gap := ""
	timedOut := kind == "test" || kind == "command" || kind == "preflight"
	if timedOut {
		gap = "test_timeout_gap"
		if exitCode == 0 {
			exitCode = -1
		}
	}
	if kind == "none" && errors.Is(runErr, errSnapshotGoDependency) {
		gap = "test_dependency_outside_snapshot"
	}
	if len(packages) == 0 && kind == "none" {
		gap, exitCode, diagnostic = "test_scope_empty", -1, "The selected snapshot worksets contain no Go packages."
	}
	if packages == nil {
		packages = []string{}
	}
	commandLine := []string{}
	if len(executed) > 0 {
		commandLine = executed[0]
		if timedOut {
			commandLine = executed[len(executed)-1]
		}
	}
	if len(commands) < 2 {
		executed = nil
	}
	return goTestVerification{
		Gap:              gap,
		Completed:        gap == "" && kind == "none" && (exitCode == 0 || exitCode == 1),
		Passed:           gap == "" && runErr == nil && kind == "none",
		ReviewedRevision: snapshot.revision, BaseRevision: snapshot.baseRevision,
		Packages: packages, Command: commandLine, Commands: executed, ExitCode: exitCode,
		TimedOut: timedOut || kind == "caller_deadline", TimeoutKind: kind,
		OutputSHA256: hex.EncodeToString(output.digest.Sum(nil)), OutputBytes: output.count,
		SummaryTruncated: output.count > 2*goTestSummaryHalf, Diagnostic: diagnostic,
	}
}
