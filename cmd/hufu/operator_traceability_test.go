package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestOperatorRequirementTraceabilityIsComplete(t *testing.T) {
	t.Parallel()
	root := operatorTraceabilityRoot(t)
	architecture := readOperatorTraceabilityFile(t, filepath.Join(root, "docs", "architecture", "operator-experience.md"))
	tracePath := filepath.Join(root, "docs", "reference", "operator-requirement-traceability.md")
	trace := readOperatorTraceabilityFile(t, tracePath)

	for _, expression := range []string{`HF-UX-[0-9]{3}[A-Z]?`, `UX-T[0-9]{2}`} {
		pattern := regexp.MustCompile(expression)
		for _, id := range uniqueOperatorTraceabilityMatches(pattern, architecture) {
			line := operatorTraceabilityLine(trace, id)
			if line == "" {
				t.Errorf("%s is missing from %s", id, tracePath)
				continue
			}
			if !regexp.MustCompile("`Test[A-Za-z0-9_]+`").MatchString(line) {
				t.Errorf("%s has no executable test reference: %s", id, line)
			}
		}
	}
	if !strings.Contains(operatorTraceabilityLine(trace, "HF-UX-070"), "blocked-human") {
		t.Error("HF-UX-070 must remain explicitly blocked-human until participant evidence exists")
	}

	knownTests := collectOperatorTraceabilityTests(t, root)
	for _, match := range regexp.MustCompile("`(Test[A-Za-z0-9_]+)`").FindAllStringSubmatch(trace, -1) {
		if _, ok := knownTests[match[1]]; !ok {
			t.Errorf("traceability references missing Go test %s", match[1])
		}
	}
}

func operatorTraceabilityRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}

func readOperatorTraceabilityFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func uniqueOperatorTraceabilityMatches(pattern *regexp.Regexp, text string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, match := range pattern.FindAllString(text, -1) {
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		result = append(result, match)
	}
	return result
}

func operatorTraceabilityLine(text, id string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "| "+id+" |") {
			return line
		}
	}
	return ""
}

func collectOperatorTraceabilityTests(t *testing.T, root string) map[string]struct{} {
	t.Helper()
	tests := make(map[string]struct{})
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && strings.HasPrefix(function.Name.Name, "Test") {
				tests[function.Name.Name] = struct{}{}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tests
}
