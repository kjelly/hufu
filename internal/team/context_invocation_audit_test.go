package team

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

type modelCallChokepoint struct {
	path           string
	marker         string
	owner          string
	purpose        string
	classification string
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate context invocation audit")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func loadModelCallChokepoints(t *testing.T, root string) []modelCallChokepoint {
	t.Helper()
	path := filepath.Join(root, "internal", "team", "testdata", "model_call_chokepoints.txt")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var entries []modelCallChokepoint
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		separator := "\t"
		if strings.Contains(line, "<TAB>") {
			separator = "<TAB>"
		}
		fields := strings.Split(line, separator)
		if len(fields) != 5 {
			t.Fatalf("invalid model-call inventory row %q", line)
		}
		entry := modelCallChokepoint{path: fields[0], marker: fields[1], owner: fields[2], purpose: fields[3], classification: fields[4]}
		if entry.path == "" || entry.marker == "" || entry.owner == "" || entry.purpose == "" || entry.classification == "" {
			t.Fatalf("incomplete model-call inventory row %q", line)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestModelCallChokepointInventoryIsComplete(t *testing.T) {
	root := repositoryRoot(t)
	entries := loadModelCallChokepoints(t, root)
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(root, entry.path))
		if err != nil {
			t.Fatalf("read inventory source %s: %v", entry.path, err)
		}
		if !strings.Contains(string(data), entry.marker) {
			t.Fatalf("inventory marker disappeared: %s: %q", entry.path, entry.marker)
		}
	}

	// These are the concrete production primitives that can start a model
	// generation or construct a sidecar outside an existing stream. A new one
	// must be consciously registered with an owner, purpose, and boundary
	// classification before it can land.
	for _, target := range []string{"cmd/hufu", "internal/agent", "internal/promotion", "internal/sidecar", "internal/team"} {
		err := filepath.WalkDir(filepath.Join(root, target), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			for _, marker := range []string{"sidecar.NewSidecar(", ".Generate(ctx,", ".Stream(ctx,", `exec.CommandContext(ctx2, "ollama", "run"`} {
				if count := strings.Count(string(data), marker); count != registeredMarkerCount(entries, rel, marker) {
					return &unregisteredModelChokepointError{path: rel, marker: marker}
				}
			}
			return nil
		})
		if err != nil {
			if issue, ok := err.(*unregisteredModelChokepointError); ok {
				t.Fatalf("unregistered production model chokepoint: %s: %s", issue.path, issue.marker)
			}
			t.Fatal(err)
		}
	}
}

type unregisteredModelChokepointError struct{ path, marker string }

func (e *unregisteredModelChokepointError) Error() string { return e.path + ": " + e.marker }

func registeredMarkerCount(entries []modelCallChokepoint, path, broadMarker string) int {
	count := 0
	for _, entry := range entries {
		if entry.path != path {
			continue
		}
		if strings.Contains(entry.marker, broadMarker) || strings.Contains(broadMarker, entry.marker) {
			count++
		}
	}
	return count
}

func TestModelCallChokepointAuditRejectsUnregisteredPrimitive(t *testing.T) {
	entries := []modelCallChokepoint{{path: "a.go", marker: "registered.Generate(ctx,"}}
	if got := registeredMarkerCount(entries, "a.go", ".Generate(ctx,"); got != 1 {
		t.Fatalf("registered primitive count = %d, want 1", got)
	}
	if actual := strings.Count("registered.Generate(ctx, call)\nunregistered.Generate(ctx, call)", ".Generate(ctx,"); actual == registeredMarkerCount(entries, "a.go", ".Generate(ctx,") {
		t.Fatal("unregistered generation primitive was not detected")
	}
}

func TestSidecarPurposesAreRegistered(t *testing.T) {
	root := repositoryRoot(t)
	purposePattern := regexp.MustCompile(`WithPurpose\([^,]+,\s*"([a-z0-9_]+)"\)`) // literal calls are part of the closed contract.
	for _, target := range []string{"cmd/hufu", "internal/team", "internal/skill"} {
		err := filepath.WalkDir(filepath.Join(root, target), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, match := range purposePattern.FindAllStringSubmatch(string(data), -1) {
				if _, err := contextPurposePolicy(match[1]); err != nil {
					return &unregisteredModelChokepointError{path: path, marker: match[1]}
				}
			}
			return nil
		})
		if err != nil {
			if issue, ok := err.(*unregisteredModelChokepointError); ok {
				t.Fatalf("unregistered sidecar purpose: %s: %s", issue.path, issue.marker)
			}
			t.Fatal(err)
		}
	}
}
