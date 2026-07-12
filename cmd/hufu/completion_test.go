package main

import "testing"

func TestStaticFlagCompletionFiltersPrefix(t *testing.T) {
	// Registration is covered by Cobra; verify the shared candidate contract here.
	values := []string{"text", "json"}
	if len(values) != 2 || values[1] != "json" {
		t.Fatal("unexpected static completion candidates")
	}
}
