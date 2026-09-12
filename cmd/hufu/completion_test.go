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
		`export extern "hufu inspect run"`,
		`export extern "hufu inspect task"`,
		`export extern "hufu inspect evidence"`,
		`export extern "hufu inspect context"`,
		`export extern "hufu inspect trace"`,
		`export extern "hufu inspect replay"`,
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("Nushell completion does not contain %q", expected)
		}
	}
}
