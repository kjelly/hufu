package main

import "testing"

func TestResolveOutputAliasAcceptsEquivalentAndRejectsConflict(t *testing.T) {
	allowed := []string{"text", "json"}
	got, err := resolveOutputAlias("format", "json", true, "output", "json", true, "text", allowed)
	if err != nil || got != "json" {
		t.Fatalf("equivalent aliases = %q, %v", got, err)
	}
	if _, err := resolveOutputAlias("format", "text", true, "output", "json", true, "text", allowed); err == nil {
		t.Fatal("conflicting aliases were accepted")
	}
	if _, err := resolveOutputAlias("output", "mermaid", true, "json", "json", false, "text", []string{"text", "json", "mermaid"}); err != nil {
		t.Fatalf("domain-specific format was rejected: %v", err)
	}
}
