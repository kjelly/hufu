// Command testlayers runs the short, integration, or complete offline suite.
// Integration membership comes from canonical testing.Short guards in the
// active test files, so adding a guard also adds its replacement CI execution.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type testPackage struct {
	ImportPath   string
	Dir          string
	TestGoFiles  []string
	XTestGoFiles []string
}

type suite struct {
	Package     string   `json:"package"`
	Short       []string `json:"short"`
	Integration []string `json:"integration"`
}

func main() {
	layer := flag.String("layer", "short", "short, integration, or full")
	scope := flag.String("scope", "all", "all, cli, team, improver, or other")
	list := flag.Bool("list", false, "print active top-level Test inventory as JSON without executing tests")
	flag.Parse()
	if err := run(*layer, *scope, *list, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(layer, scope string, list bool, extra []string) error {
	if !slices.Contains([]string{"short", "integration", "full"}, layer) ||
		!slices.Contains([]string{"all", "cli", "team", "improver", "other"}, scope) {
		return fmt.Errorf("invalid layer %q or scope %q", layer, scope)
	}
	output, err := exec.Command("go", "list", "-json", "./...", "./.agent-teams/hufu-dev/diagnostic-pilot/...").CombinedOutput()
	if err != nil {
		return fmt.Errorf("discover packages: %w\n%s", err, output)
	}
	inventory, err := discover(strings.NewReader(string(output)))
	if err != nil {
		return err
	}
	var selected []suite
	for _, entry := range inventory {
		if scope == "all" || packageScope(entry.Package) == scope {
			selected = append(selected, entry)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("no packages selected for %s", scope)
	}
	if list {
		return json.NewEncoder(os.Stdout).Encode(selected)
	}
	commands, err := testCommands(layer, selected, extra)
	if err != nil {
		return err
	}
	for _, command := range commands {
		if err := execute(command); err != nil {
			return err
		}
	}
	return nil
}

func testCommands(layer string, selected []suite, extra []string) ([][]string, error) {
	// One invocation preserves package concurrency for short/full. Integration
	// uses exact filters, avoiding duplicate unit execution.
	base := append([]string{"test", "-p", "2", "-count=1", "-timeout", "20m"}, extra...)
	if layer != "integration" {
		if layer == "short" {
			base = append(base, "-short")
		}
		for _, entry := range selected {
			base = append(base, entry.Package)
		}
		return [][]string{base}, nil
	}
	var active []suite
	var names []string
	for _, entry := range selected {
		if len(entry.Integration) != 0 {
			active = append(active, entry)
			names = append(names, entry.Integration...)
		}
	}
	if len(active) == 0 {
		return nil, fmt.Errorf("no integration tests selected")
	}
	slices.Sort(names)
	names = slices.Compact(names)
	// A shared filter is safe only when it cannot select a short test in any
	// participating package. Otherwise keep exact per-package filters.
	collision := false
	for _, entry := range active {
		for _, name := range entry.Short {
			if _, found := slices.BinarySearch(names, name); found {
				collision = true
			}
		}
	}
	if !collision {
		command := append(slices.Clone(base), "-run", exactPattern(names))
		for _, entry := range active {
			command = append(command, entry.Package)
		}
		return [][]string{command}, nil
	}
	var commands [][]string
	for _, entry := range active {
		commands = append(commands, append(slices.Clone(base), "-run", exactPattern(entry.Integration), entry.Package))
	}
	return commands, nil
}

func execute(args []string) error {
	fmt.Fprintln(os.Stderr, "go", strings.Join(args, " "))
	command := exec.Command("go", args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func packageScope(path string) string {
	for suffix, scope := range map[string]string{
		"/cmd/hufu": "cli", "/internal/team": "team", "/internal/improver": "improver",
	} {
		if strings.HasSuffix(path, suffix) {
			return scope
		}
	}
	return "other"
}

func exactPattern(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = regexp.QuoteMeta(name)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

func discover(input io.Reader) ([]suite, error) {
	decoder := json.NewDecoder(input)
	var inventory []suite
	for {
		var pkg testPackage
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		entry := suite{Package: pkg.ImportPath}
		for _, name := range append(slices.Clone(pkg.TestGoFiles), pkg.XTestGoFiles...) {
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(pkg.Dir, name), nil, 0)
			if err != nil {
				return nil, err
			}
			short, integration, err := classify(file)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", filepath.Join(pkg.Dir, name), err)
			}
			entry.Short = append(entry.Short, short...)
			entry.Integration = append(entry.Integration, integration...)
		}
		slices.Sort(entry.Short)
		slices.Sort(entry.Integration)
		inventory = append(inventory, entry)
	}
	slices.SortFunc(inventory, func(a, b suite) int { return strings.Compare(a.Package, b.Package) })
	return inventory, nil
}

// classify accepts only a whole-test short guard as the first statement.
// Partial/nested short skips fail discovery rather than silently losing cases.
func classify(file *ast.File) (short, integration []string, err error) {
	testingName := ""
	for _, imported := range file.Imports {
		path, _ := strconv.Unquote(imported.Path.Value)
		if path == "testing" {
			testingName = "testing"
			if imported.Name != nil {
				if imported.Name.Name == "." {
					return nil, nil, fmt.Errorf("dot import of testing is unsupported")
				}
				testingName = imported.Name.Name
			}
		}
	}
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		hasShort := false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && selector(call.Fun, testingName, "Short") {
				hasShort = true
			}
			return true
		})
		isTest := testFunction(fn, testingName)
		if !hasShort {
			if isTest {
				short = append(short, fn.Name.Name)
			}
			continue
		}
		if !isTest || !canonicalGuard(fn, testingName) {
			return nil, nil, fmt.Errorf("%s: testing.Short must be a first-statement whole-test skip guard", fn.Name.Name)
		}
		integration = append(integration, fn.Name.Name)
	}
	return short, integration, nil
}

func selector(expr ast.Expr, receiver, name string) bool {
	selected, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	identifier, ok := selected.X.(*ast.Ident)
	return ok && receiver != "" && identifier.Name == receiver && selected.Sel.Name == name
}

func testFunction(fn *ast.FuncDecl, testingName string) bool {
	if fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Type.Results != nil ||
		len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) > 1 {
		return false
	}
	suffix, _ := utf8.DecodeRuneInString(strings.TrimPrefix(fn.Name.Name, "Test"))
	if unicode.IsLower(suffix) {
		return false
	}
	pointer, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	return ok && selector(pointer.X, testingName, "T")
}

func canonicalGuard(fn *ast.FuncDecl, testingName string) bool {
	if len(fn.Body.List) == 0 || len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
		return false
	}
	guard, ok := fn.Body.List[0].(*ast.IfStmt)
	if !ok || guard.Init != nil || guard.Else != nil || len(guard.Body.List) != 1 {
		return false
	}
	condition, ok := guard.Cond.(*ast.CallExpr)
	if !ok || len(condition.Args) != 0 || !selector(condition.Fun, testingName, "Short") {
		return false
	}
	statement, ok := guard.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := statement.X.(*ast.CallExpr)
	parameter := fn.Type.Params.List[0].Names[0].Name
	return ok && (selector(call.Fun, parameter, "Skip") || selector(call.Fun, parameter, "Skipf"))
}
