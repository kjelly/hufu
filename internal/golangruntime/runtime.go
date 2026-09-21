// Package golangruntime executes maintainer-authored Go action providers with
// Hufu's embedded Yaegi runtime. Programs run in a dedicated Hufu subprocess:
// they are trusted to use the invoking OS user's process and filesystem
// authority, while cancellation can still terminate their complete process
// tree.
package golangruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
	"github.com/traefik/yaegi/stdlib/unrestricted"
)

const (
	// ChildArg selects the hidden Hufu child mode used for trusted Go actions.
	ChildArg = "--hufu-golang-runtime-child"
	// TrustedStaticMode makes the provider's authority explicit in team.yaml.
	TrustedStaticMode = "trusted-static"

	defaultOutputLimit = 1024 * 1024
	defaultErrorLimit  = 16 * 1024
)

// Program is an immutable identity for a statically inspected source package.
type Program struct {
	Source string
	Digest string
}

// Result contains the child program's bounded output streams.
type Result struct {
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
}

// Prepare validates and hashes a package below teamDir. Symlinks are rejected
// so the authored source identity cannot be redirected after manifest review.
func Prepare(teamDir, source string) (Program, error) {
	teamDir = strings.TrimSpace(teamDir)
	source = strings.TrimSpace(source)
	if teamDir == "" {
		return Program{}, errors.New("go runtime requires a team directory")
	}
	if source == "" {
		return Program{}, errors.New("go runtime requires a source path")
	}
	teamRoot, err := filepath.Abs(teamDir)
	if err != nil {
		return Program{}, fmt.Errorf("resolve team directory: %w", err)
	}
	teamRoot, err = filepath.EvalSymlinks(teamRoot)
	if err != nil {
		return Program{}, fmt.Errorf("resolve team directory symlinks: %w", err)
	}
	candidate := source
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(teamRoot, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return Program{}, fmt.Errorf("resolve Go runtime source: %w", err)
	}
	if err := rejectSymlinkPath(teamRoot, candidate); err != nil {
		return Program{}, err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return Program{}, fmt.Errorf("resolve Go runtime source symlinks: %w", err)
	}
	if !pathWithin(teamRoot, resolved) {
		return Program{}, errors.New("go runtime source must remain within the team directory")
	}
	digest, err := inspectPackage(resolved)
	if err != nil {
		return Program{}, err
	}
	return Program{Source: resolved, Digest: digest}, nil
}

// Execute invokes a prepared program through the current Hufu executable.
func Execute(ctx context.Context, executable string, program Program, input []byte, env []string, outputLimit, errorLimit int) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(executable) == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return Result{}, fmt.Errorf("resolve Hufu executable: %w", err)
		}
	}
	if strings.TrimSpace(program.Source) == "" || strings.TrimSpace(program.Digest) == "" {
		return Result{}, errors.New("go runtime program is not prepared")
	}
	if outputLimit <= 0 {
		outputLimit = defaultOutputLimit
	}
	if errorLimit <= 0 {
		errorLimit = defaultErrorLimit
	}
	stdout := newLimitedBuffer(outputLimit)
	stderr := newLimitedBuffer(errorLimit)
	cmd := exec.Command(executable, ChildArg, program.Source, program.Digest)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(env) > 0 {
		cmd.Env = slices.Clone(env)
	}
	configureProcessAttributes(cmd)
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start embedded Go runtime: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	var runErr error
	select {
	case runErr = <-wait:
	case <-ctx.Done():
		terminateProcess(cmd)
		<-wait
		return Result{
			Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
			StdoutTruncated: stdout.Truncated(), StderrTruncated: stderr.Truncated(),
		}, ctx.Err()
	}
	result := Result{
		Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
		StdoutTruncated: stdout.Truncated(), StderrTruncated: stderr.Truncated(),
	}
	if runErr != nil {
		return result, fmt.Errorf("embedded Go runtime failed: %w", runErr)
	}
	return result, nil
}

// RunChild executes the hidden child mode. It is called before Cobra or any
// interactive runtime initialization in cmd/hufu.
func RunChild(source, expectedDigest string, in io.Reader, out, errOut io.Writer) int {
	digest, err := inspectPackage(source)
	if err != nil {
		writeDiagnostic(errOut, "inspect trusted Go action: %v\n", err)
		return 1
	}
	if digest != expectedDigest {
		writeDiagnostic(errOut, "trusted Go action source changed after preflight\n")
		return 1
	}
	goPath, _, cleanup, err := stagePackage(source, expectedDigest)
	if err != nil {
		writeDiagnostic(errOut, "stage trusted Go action: %v\n", err)
		return 1
	}
	defer cleanup()
	runtime := interp.New(interp.Options{GoPath: goPath, Stdin: in, Stdout: out, Stderr: errOut, Unrestricted: true})
	trustedSymbols := make(interp.Exports, len(stdlib.Symbols))
	for packagePath, symbols := range stdlib.Symbols {
		trustedSymbols[packagePath] = make(map[string]reflect.Value, len(symbols))
		for name, value := range symbols {
			trustedSymbols[packagePath][name] = value
		}
	}
	for packagePath, symbols := range unrestricted.Symbols {
		if trustedSymbols[packagePath] == nil {
			trustedSymbols[packagePath] = make(map[string]reflect.Value, len(symbols))
		}
		for name, value := range symbols {
			trustedSymbols[packagePath][name] = value
		}
	}
	if err := runtime.Use(trustedSymbols); err != nil {
		writeDiagnostic(errOut, "load Go standard library: %v\n", err)
		return 1
	}
	if err := runtime.Use(interp.Symbols); err != nil {
		writeDiagnostic(errOut, "load Go interpreter symbols: %v\n", err)
		return 1
	}
	if _, err := runtime.EvalPathWithContext(context.Background(), "action"); err != nil {
		writeDiagnostic(errOut, "load trusted Go action: %v\n", err)
		return 1
	}
	actionSymbols, ok := runtime.Symbols("action")["action"]
	if !ok {
		writeDiagnostic(errOut, "trusted Go action package symbols are unavailable\n")
		return 1
	}
	runValue := actionSymbols["Run"]
	if !runValue.IsValid() {
		writeDiagnostic(errOut, "trusted Go action does not export Run\n")
		return 1
	}
	want := reflect.TypeFor[func(context.Context, io.Reader, io.Writer) error]()
	if runValue.Type() != want {
		writeDiagnostic(errOut, "trusted Go action Run has type %s, want %s\n", runValue.Type(), want)
		return 1
	}
	run := runValue.Interface().(func(context.Context, io.Reader, io.Writer) error)
	if err := run(context.Background(), in, out); err != nil {
		writeDiagnostic(errOut, "trusted Go action: %v\n", err)
		return 1
	}
	return 0
}

func writeDiagnostic(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

func stagePackage(source, expectedDigest string) (string, string, func(), error) {
	goPath, err := os.MkdirTemp("", "hufu-golang-runtime-")
	if err != nil {
		return "", "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(goPath) }
	stagedSource := filepath.Join(goPath, "src", "action")
	if err := os.MkdirAll(stagedSource, 0o700); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(source, name))
		if readErr != nil {
			cleanup()
			return "", "", func() {}, readErr
		}
		if writeErr := os.WriteFile(filepath.Join(stagedSource, name), data, 0o600); writeErr != nil {
			cleanup()
			return "", "", func() {}, writeErr
		}
	}
	stagedDigest, err := inspectPackage(stagedSource)
	if err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	if stagedDigest != expectedDigest {
		cleanup()
		return "", "", func() {}, errors.New("trusted Go action source changed while staging")
	}
	return goPath, stagedSource, cleanup, nil
}

func inspectPackage(source string) (string, error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("stat Go runtime source: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("go runtime source must be a directory")
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return "", fmt.Errorf("read Go runtime source: %w", err)
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("go runtime source %q must not be a symlink", name)
		}
		if !entry.Type().IsRegular() {
			entryInfo, infoErr := entry.Info()
			if infoErr != nil || !entryInfo.Mode().IsRegular() {
				return "", fmt.Errorf("go runtime source %q must be a regular file", name)
			}
		}
		files = append(files, name)
	}
	if len(files) == 0 {
		return "", errors.New("go runtime source contains no non-test Go files")
	}
	slices.Sort(files)
	hash := sha256.New()
	runDeclarations := 0
	for _, name := range files {
		path := filepath.Join(source, name)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", fmt.Errorf("read Go runtime source %q: %w", name, readErr)
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, data, 0)
		if parseErr != nil {
			return "", fmt.Errorf("parse Go runtime source %q: %w", name, parseErr)
		}
		if parsed.Name.Name != "main" {
			return "", fmt.Errorf("go runtime source %q uses package %q, want main", name, parsed.Name.Name)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil {
				continue
			}
			switch function.Name.Name {
			case "main":
				return "", errors.New("trusted Go action must export Run instead of declaring main")
			case "Run":
				if !validRunSignature(parsed, function) {
					return "", errors.New("trusted Go action Run must have signature func(context.Context, io.Reader, io.Writer) error")
				}
				runDeclarations++
			}
		}
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
	}
	if runDeclarations != 1 {
		return "", fmt.Errorf("trusted Go action requires exactly one exported Run function; found %d", runDeclarations)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func validRunSignature(file *ast.File, function *ast.FuncDecl) bool {
	aliases := map[string]string{}
	for _, spec := range file.Imports {
		importPath := strings.Trim(spec.Path.Value, `"`)
		if importPath != "context" && importPath != "io" {
			continue
		}
		name := filepath.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		aliases[importPath] = name
	}
	params := flattenFieldTypes(function.Type.Params)
	results := flattenFieldTypes(function.Type.Results)
	return len(params) == 3 && len(results) == 1 &&
		selectorType(params[0], aliases["context"], "Context") &&
		selectorType(params[1], aliases["io"], "Reader") &&
		selectorType(params[2], aliases["io"], "Writer") &&
		identifierType(results[0], "error")
}

func flattenFieldTypes(fields *ast.FieldList) []ast.Expr {
	if fields == nil {
		return nil
	}
	values := make([]ast.Expr, 0, len(fields.List))
	for _, field := range fields.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for range count {
			values = append(values, field.Type)
		}
	}
	return values
}

func selectorType(expression ast.Expr, packageName, symbol string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != symbol {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && packageName != "" && identifier.Name == packageName
}

func identifierType(expression ast.Expr, name string) bool {
	identifier, ok := expression.(*ast.Ident)
	return ok && identifier.Name == name
}

func rejectSymlinkPath(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("go runtime source must remain within the team directory")
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return fmt.Errorf("inspect Go runtime source path: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("go runtime source path must not contain symlinks")
		}
	}
	return nil
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{remaining: limit}
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	if len(data) > b.remaining {
		data = data[:b.remaining]
		b.truncated = true
	}
	if len(data) > 0 {
		_, _ = b.buffer.Write(data)
		b.remaining -= len(data)
	}
	return original, nil
}

func (b *limitedBuffer) Bytes() []byte { return slices.Clone(b.buffer.Bytes()) }

func (b *limitedBuffer) Truncated() bool { return b.truncated }
