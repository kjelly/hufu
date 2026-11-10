package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestShardInventoryIsCompleteDisjointAndStable(t *testing.T) {
	selected := []suite{
		{Package: "example/a", Short: []string{"TestAlpha", "TestBeta", "TestShared"}, Integration: []string{"TestProcess"}},
		{Package: "example/b", Short: []string{"TestGamma", "TestShared"}, Integration: []string{"TestDatabase"}},
	}
	before := fmt.Sprint(selected)
	for _, count := range []int{1, 2, 3, 7} {
		seen := map[string]int{}
		for index := range count {
			shard := testShard{Index: index, Count: count}
			for _, entry := range shard.inventory(selected) {
				for _, name := range append(slices.Clone(entry.Short), entry.Integration...) {
					seen[entry.Package+"/"+name]++
				}
			}
			// Reordering sources and adding an unrelated test cannot move an
			// existing test to a different shard.
			expanded := []suite{selected[1], {Package: selected[0].Package, Short: append([]string{"TestNew"}, selected[0].Short...), Integration: selected[0].Integration}}
			chosen := shard.inventory(expanded)
			for _, entry := range shard.inventory(selected) {
				other := chosen[0]
				if other.Package != entry.Package {
					other = chosen[1]
				}
				for _, name := range entry.Short {
					if !slices.Contains(other.Short, name) {
						t.Fatalf("test %s moved after an unrelated addition", name)
					}
				}
			}
		}
		for _, entry := range selected {
			for _, name := range append(slices.Clone(entry.Short), entry.Integration...) {
				if seen[entry.Package+"/"+name] != 1 {
					t.Fatalf("shards=%d, assignment count for %s/%s = %d", count, entry.Package, name, seen[entry.Package+"/"+name])
				}
			}
		}
	}
	if fmt.Sprint(selected) != before {
		t.Fatal("shard selection mutated the complete inventory")
	}
}

func TestShardedCommandsKeepLayersAndPackageFilters(t *testing.T) {
	selected := []suite{
		{Package: "example/a", Short: []string{"TestAlpha", "TestShared"}, Integration: []string{"TestProcess"}},
		{Package: "example/b", Short: []string{"TestShared", "TestBeta"}, Integration: []string{"TestAlpha"}},
		{Package: "example/auxiliary"},
	}
	for _, layer := range []string{"short", "integration", "full"} {
		t.Run(layer, func(t *testing.T) {
			for index := range 3 {
				shard := testShard{Index: index, Count: 3}
				commands, err := shard.commands(layer, selected, []string{"-race", "-json", "-timeout", "30m"})
				if err != nil {
					t.Fatal(err)
				}
				matches := map[string]int{}
				for _, command := range commands {
					patternIndex := slices.Index(command, "-run")
					if patternIndex < 0 || slices.Contains(command, "-short") != (layer == "short") || !slices.Contains(command, "-race") || !slices.Contains(command, "-count=1") {
						t.Fatalf("lost filter or validation flags: %v", command)
					}
					pattern := regexp.MustCompile(command[patternIndex+1])
					for _, entry := range selected {
						if !slices.Contains(command[patternIndex+2:], entry.Package) {
							continue
						}
						for _, name := range append(append(slices.Clone(entry.Short), entry.Integration...), "Example", "FuzzInput", "TestAlphaExtra") {
							if pattern.MatchString(name) {
								matches[entry.Package+"/"+name]++
							}
						}
					}
				}
				for _, entry := range selected {
					for _, name := range append(append(slices.Clone(entry.Short), entry.Integration...), "Example", "FuzzInput", "TestAlphaExtra") {
						want := 0
						eligible := (layer != "integration" && slices.Contains(entry.Short, name)) || (layer != "short" && slices.Contains(entry.Integration, name))
						if eligible && shard.contains(entry.Package, name) || (index == 0 && layer != "integration" && (name == "Example" || name == "FuzzInput")) {
							want = 1
						}
						if got := matches[entry.Package+"/"+name]; got != want {
							t.Errorf("%s shard %d: %s/%s selected %d times, want %d", layer, index, entry.Package, name, got, want)
						}
					}
				}
			}
		})
	}
}

func TestShardValidationAndDefaultCommands(t *testing.T) {
	selected := []suite{{Package: "example/a", Short: []string{"TestAlpha"}, Integration: []string{"TestProcess"}}}
	for _, shard := range []testShard{{Count: 0}, {Index: -1, Count: 3}, {Index: 3, Count: 3}} {
		if _, err := shard.commands("short", selected, nil); err == nil {
			t.Fatalf("invalid shard accepted: %+v", shard)
		}
	}
	for _, layer := range []string{"short", "integration", "full"} {
		got, err := (testShard{Count: 1}).commands(layer, selected, nil)
		want, oldErr := testCommands(layer, selected, nil)
		if err != nil || oldErr != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("default %s execution changed: %v %v, %v", layer, got, err, oldErr)
		}
	}
	if _, err := (testShard{Count: 3}).commands("integration", []suite{{Package: "example/empty"}}, nil); err == nil {
		t.Fatal("an integration scope with no eligible tests must fail")
	}
	if _, err := (testShard{Count: 1}).commands("unknown", selected, nil); err == nil {
		t.Fatal("an unknown layer must fail before executing commands")
	}
}

func TestShardAssignmentKeepsStableBucketsAndDistributesNames(t *testing.T) {
	for _, tc := range []struct {
		pkg, name string
		index     int
	}{
		{"example/a", "TestAlpha", 1},
		{"example/b", "TestAlpha", 2},
		{"example/a", "TestBeta", 2},
		{"example/a", "TestShared", 0},
	} {
		if !(testShard{Index: tc.index, Count: 3}).contains(tc.pkg, tc.name) {
			t.Errorf("stable FNV-1a bucket changed for %s/%s", tc.pkg, tc.name)
		}
	}
	for index := range 3 {
		count := 0
		for n := range 300 {
			if (testShard{Index: index, Count: 3}).contains("example/a", fmt.Sprintf("TestCase%03d", n)) {
				count++
			}
		}
		if count < 80 || count > 120 {
			t.Errorf("shard %d received %d/300 names; workload partition is unexpectedly skewed", index, count)
		}
	}
}

func TestCIShardsCoverEveryScopeWithUniquePartitions(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Name     string
			Env      map[string]string
			Strategy struct {
				FailFast *bool `yaml:"fail-fast"`
				Matrix   struct {
					Include []struct {
						Scope          string
						Partition      string
						Index          *int   `yaml:"shard-index"`
						Count          *int   `yaml:"shard-count"`
						PackageTimeout string `yaml:"package-timeout"`
						JobTimeout     int    `yaml:"job-timeout"`
					}
				}
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["test"]
	if job.Strategy.FailFast == nil || *job.Strategy.FailFast || !strings.Contains(job.Name, "matrix.partition") || !strings.Contains(job.Env["GOCACHE"], "matrix.partition") {
		t.Fatal("CI must run all partitions with distinct job/cache identities")
	}
	counts := map[string]int{}
	partitions := map[string]bool{}
	assignments := map[string]bool{}
	for _, entry := range job.Strategy.Matrix.Include {
		if entry.Index == nil || entry.Count == nil || entry.Partition == "" || partitions[entry.Partition] {
			t.Fatalf("missing shard metadata or duplicate partition: %+v", entry)
		}
		partitions[entry.Partition] = true
		if err := (testShard{Index: *entry.Index, Count: *entry.Count}).validate(); err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("%s/%d", entry.Scope, *entry.Index)
		if assignments[key] {
			t.Fatalf("duplicate scope/shard assignment: %s", key)
		}
		assignments[key] = true
		counts[entry.Scope]++
		wantCount, wantTimeout, wantJobTimeout := 1, "20m", 30
		if entry.Scope == "team" {
			wantCount = 3
		}
		if entry.Scope == "team" || entry.Scope == "improver" {
			wantTimeout, wantJobTimeout = "30m", 40
		}
		if *entry.Count != wantCount || entry.PackageTimeout != wantTimeout || entry.JobTimeout != wantJobTimeout {
			t.Fatalf("partition changed coverage count or timeout contract: %+v", entry)
		}
	}
	if !reflect.DeepEqual(counts, map[string]int{"cli": 1, "team": 3, "improver": 1, "other": 1}) {
		t.Fatalf("CI scope/partition counts = %v", counts)
	}
}

func TestShardsExecuteExamplesAndFuzzSeedsExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"go.mod": "module example/shardfixture\n\ngo 1.27.2\n",
		"shard_test.go": `package fixture
import ("fmt"; "testing")
func TestAlpha(t *testing.T) {}
func TestBeta(t *testing.T) {}
func TestProcess(t *testing.T) { if testing.Short() { t.Skip("integration") } }
func FuzzInput(f *testing.F) { f.Add("seed"); f.Fuzz(func(t *testing.T, value string) {}) }
func Example() {
    fmt.Println("example")
    // Output: example
}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	selected := []suite{{Package: "example/shardfixture", Short: []string{"TestAlpha", "TestBeta"}, Integration: []string{"TestProcess"}}}
	for _, layer := range []string{"short", "integration", "full"} {
		t.Run(layer, func(t *testing.T) {
			seen := map[string]int{}
			for index := range 3 {
				commands, err := (testShard{Index: index, Count: 3}).commands(layer, selected, []string{"-json"})
				if err != nil {
					t.Fatal(err)
				}
				for _, args := range commands {
					// Reuse the parent mise/setup-go toolchain, as the runner does.
					command := exec.CommandContext(t.Context(), "go", append([]string{"-C", dir}, args...)...)
					command.Dir = repoRoot
					command.Env = append(os.Environ(), "GOWORK=off")
					output, err := command.CombinedOutput()
					if err != nil {
						t.Fatalf("execute shard %d: %v\n%s", index, err, output)
					}
					for _, line := range bytes.Split(output, []byte("\n")) {
						if len(line) == 0 {
							continue
						}
						var event struct{ Action, Test string }
						if err := json.Unmarshal(line, &event); err != nil {
							t.Fatalf("decode fixture result: %v\n%s", err, line)
						}
						if event.Action == "pass" && event.Test != "" && !strings.Contains(event.Test, "/") {
							seen[event.Test]++
						}
					}
				}
			}
			want := map[string]int{"TestProcess": 1}
			if layer != "integration" {
				want = map[string]int{"TestAlpha": 1, "TestBeta": 1, "FuzzInput": 1, "Example": 1}
				if layer == "full" {
					want["TestProcess"] = 1
				}
			}
			if !reflect.DeepEqual(seen, want) {
				t.Fatalf("executed %v, want %v", seen, want)
			}
		})
	}
}
