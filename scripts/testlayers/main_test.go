package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestClassifyWholeTestGuards(t *testing.T) {
	for _, tt := range []struct {
		name, source       string
		short, integration []string
		wantError          bool
	}{
		{"unit and guarded", `package p
import "testing"
func TestUnit(t *testing.T) {}
func TestUnnamed(*testing.T) {}
func Testhelper(t *testing.T) {}
func TestHelper() {}
func TestProcess(t *testing.T) { if testing.Short() { t.Skip("integration") }; run() }
func TestMain(m *testing.M) {}
`, []string{"TestUnit", "TestUnnamed"}, []string{"TestProcess"}, false},
		{"aliased testing", `package p
import check "testing"
func TestProcess(test *check.T) { if check.Short() { test.Skipf("%s", "integration") } }
`, nil, []string{"TestProcess"}, false},
		{"nested guard", `package p
import "testing"
func TestProcess(t *testing.T) { t.Run("child", func(t *testing.T) { if testing.Short() { t.Skip("integration") } }) }
`, nil, nil, true},
		{"late guard", `package p
import "testing"
func TestProcess(t *testing.T) { setup(); if testing.Short() { t.Skip("integration") } }
`, nil, nil, true},
		{"inverted guard", `package p
import "testing"
func TestProcess(t *testing.T) { if !testing.Short() { run() } }
`, nil, nil, true},
		{"helper guard", `package p
import "testing"
func helper(t *testing.T) { if testing.Short() { t.Skip("integration") } }
`, nil, nil, true},
		{"guard does not skip", `package p
import "testing"
func TestProcess(t *testing.T) { if testing.Short() { t.Log("integration") } }
`, nil, nil, true},
		{"guard with else", `package p
import "testing"
func TestProcess(t *testing.T) { if testing.Short() { t.Skip("integration") } else { run() } }
`, nil, nil, true},
		{"ordinary skip remains unit", `package p
import "testing"
func TestLive(t *testing.T) { if disabled() { t.Skip("opt in") } }
`, []string{"TestLive"}, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "test.go", tt.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			short, integration, err := classify(file)
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v", err)
			}
			if !reflect.DeepEqual(short, tt.short) || !reflect.DeepEqual(integration, tt.integration) {
				t.Fatalf("short=%v integration=%v", short, integration)
			}
		})
	}
}

func TestDiscoverUsesOnlyActiveGoListFilesIncludingExternalTests(t *testing.T) {
	if fixture := os.Getenv("HUFU_TESTLAYERS_LIST_FIXTURE"); fixture != "" {
		fmt.Fprintln(os.Stderr, "go: downloading example/dependency v1.0.0")
		if fixture == "failure" {
			fmt.Fprintln(os.Stderr, "module discovery failed")
			os.Exit(7)
		}
		fmt.Fprintln(os.Stdout, fixture)
		os.Exit(0)
	}
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"unit_test.go":             "package p; import \"testing\"; func TestUnit(t *testing.T) {}",
		"process_test.go":          "package p_test; import \"testing\"; func TestProcess(t *testing.T) { if testing.Short() { t.Skip(\"integration\") } }",
		"inactive_windows_test.go": "invalid Go source must not be parsed",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pkg := testPackage{ImportPath: "example/p", Dir: dir, TestGoFiles: []string{"unit_test.go"}, XTestGoFiles: []string{"process_test.go"}}
	data, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := discover(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	want := []suite{{Package: "example/p", Short: []string{"TestUnit"}, Integration: []string{"TestProcess"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory=%+v", got)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []string{string(data), "failure"} {
		command := exec.CommandContext(t.Context(), testBinary, "-test.run=^TestDiscoverUsesOnlyActiveGoListFilesIncludingExternalTests$")
		command.Env = append(os.Environ(), "HUFU_TESTLAYERS_LIST_FIXTURE="+fixture)
		var diagnostics bytes.Buffer
		got, err := discoverCommand(command, &diagnostics)
		if !strings.Contains(diagnostics.String(), "go: downloading example/dependency") {
			t.Fatalf("module download diagnostic lost: %q", diagnostics.String())
		}
		if fixture == "failure" {
			if err == nil || !strings.Contains(diagnostics.String(), "module discovery failed") {
				t.Fatalf("failed command was not reported: %v, %q", err, diagnostics.String())
			}
		} else if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("stderr contaminated package JSON: %v, inventory=%+v", err, got)
		}
	}
	if _, err := discover(strings.NewReader("{invalid")); err == nil {
		t.Fatal("accepted broken package inventory")
	}
	pkg.TestGoFiles = append(pkg.TestGoFiles, "missing_test.go")
	data, _ = json.Marshal(pkg)
	if _, err := discover(strings.NewReader(string(data))); err == nil {
		t.Fatal("accepted missing test source")
	}
}

func TestExactPatternAndPackageScopes(t *testing.T) {
	pattern := regexp.MustCompile(exactPattern([]string{"TestProcess", "TestProcessLonger"}))
	for name, want := range map[string]bool{"TestProcess": true, "TestProcessLonger": true, "TestProcessExtra": false, "OtherTestProcess": false} {
		if pattern.MatchString(name) != want {
			t.Fatalf("pattern incorrectly selects %s", name)
		}
	}
	for path, want := range map[string]string{
		"github.com/kjelly/hufu/cmd/hufu":                               "cli",
		"github.com/kjelly/hufu/internal/team":                          "team",
		"github.com/kjelly/hufu/internal/improver":                      "improver",
		"github.com/kjelly/hufu/internal/newpackage":                    "other",
		"github.com/kjelly/hufu/.agent-teams/hufu-dev/diagnostic-pilot": "other",
	} {
		if got := packageScope(path); got != want {
			t.Fatalf("scope(%s)=%s, want %s", path, got, want)
		}
	}
}

func TestLayerCommandsPreserveCoverageAndIsolation(t *testing.T) {
	selected := []suite{
		{Package: "example/a", Short: []string{"TestUnit"}, Integration: []string{"TestProcess"}},
		{Package: "example/b", Short: []string{"TestOther"}},
		{Package: "example/c", Integration: []string{"TestDatabase"}},
	}
	base := []string{"test", "-p", "2", "-count=1", "-timeout", "20m", "-race"}
	for _, layer := range []string{"short", "full"} {
		got, err := testCommands(layer, selected, []string{"-race"})
		if err != nil {
			t.Fatal(err)
		}
		want := append([]string{}, base...)
		if layer == "short" {
			want = append(want, "-short")
		}
		want = append(want, "example/a", "example/b", "example/c")
		if !reflect.DeepEqual(got, [][]string{want}) {
			t.Fatalf("%s commands=%v", layer, got)
		}
	}
	got, err := testCommands("integration", selected, []string{"-race"})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		append(append([]string{}, base...), "-run", "^(TestDatabase|TestProcess)$", "example/a", "example/c"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("integration commands=%v", got)
	}
	selected[2].Short = []string{"TestProcess"}
	got, err = testCommands("integration", selected, []string{"-race"})
	if err != nil {
		t.Fatal(err)
	}
	want = [][]string{
		append(append([]string{}, base...), "-run", "^(TestProcess)$", "example/a"),
		append(append([]string{}, base...), "-run", "^(TestDatabase)$", "example/c"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("colliding unit name was not isolated: %v", got)
	}
	if _, err := testCommands("integration", selected[1:2], nil); err == nil {
		t.Fatal("empty integration selection passed")
	}
}

func TestPackageTimeoutOverridePreservesRaceAndFreshExecution(t *testing.T) {
	selected := []suite{{Package: "example/team", Integration: []string{"TestPersistence"}}}
	for _, layer := range []string{"short", "integration"} {
		commands, err := testCommands(layer, selected, []string{"-race", "-json", "-timeout", "30m"})
		if err != nil {
			t.Fatal(err)
		}
		// Go uses the final value of a repeated flag. Check the effective
		// deadline, as well as the flags protecting execution/coverage.
		var timeout string
		for i, arg := range commands[0] {
			if arg == "-timeout" {
				timeout = commands[0][i+1]
			}
		}
		command := strings.Join(commands[0], " ")
		if timeout != "30m" || !strings.Contains(command, "-count=1") || !strings.Contains(command, "-race -json") {
			t.Fatalf("%s command = %v", layer, commands[0])
		}
	}
}
