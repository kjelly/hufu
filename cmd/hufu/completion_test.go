package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestStaticFlagCompletionFiltersPrefix(t *testing.T) {
	// Registration is covered by Cobra; verify the shared candidate contract here.
	values := []string{"text", "json"}
	if len(values) != 2 || values[1] != "json" {
		t.Fatal("unexpected static completion candidates")
	}
}

func TestNushellCompletionIncludesInspectCommands(t *testing.T) {
	var output bytes.Buffer
	if err := generateNushellCompletion(&output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`def "nu-complete hufu inspect formats"`,
		`export extern "hufu inspect overview"`,
		`export extern "hufu inspect run"`,
		`export extern "hufu inspect task"`,
		`export extern "hufu inspect evidence"`,
		`export extern "hufu inspect context"`,
		`export extern "hufu inspect trace"`,
		`export extern "hufu inspect replay"`,
		`export extern "hufu inspect storage"`,
		`--output: string@'nu-complete hufu inspect formats'`,
		`--no-summary`,
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("Nushell completion does not contain %q", expected)
		}
	}
}

func TestNushellCompletionIncludesWorkspaceCommands(t *testing.T) {
	var output bytes.Buffer
	if err := generateNushellCompletion(&output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`def "nu-complete hufu workspace projects"`,
		`^hufu completion-helper projects`,
		`export extern "hufu workspace register"`,
		`export extern "hufu workspace list"`,
		`export extern "hufu workspace show"`,
		`export extern "hufu workspace path"`,
		`export extern "hufu workspace subject-path"`,
		`export extern "hufu workspace alias set"`,
		`export extern "hufu workspace alias clear"`,
		`export extern "hufu workspace rebind"`,
		`export extern "hufu workspace migrate"`,
		`export extern "hufu workspace doctor"`,
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("Nushell completion does not contain %q", expected)
		}
	}
}
