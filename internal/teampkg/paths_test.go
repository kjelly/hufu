package teampkg

import (
	"strings"
	"testing"
)

func TestValidateEntryPathRejectsPortablePathHazards(t *testing.T) {
	tests := []string{
		"", string([]byte{0xff}), "bad\x00name", `dir\file`, "/absolute", "//server/share",
		"C:/drive", "../escape", "dir/../escape", "./file", "dir//file", "dir/./file", "dir/",
	}
	for _, value := range tests {
		t.Run(strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			if err := ValidateEntryPath(value); err == nil {
				t.Fatalf("ValidateEntryPath(%q) succeeded", value)
			}
		})
	}
	for _, value := range []string{"team.yaml", "skills/café/SKILL.md", "schemas/result.json"} {
		if err := ValidateEntryPath(value); err != nil {
			t.Fatalf("ValidateEntryPath(%q): %v", value, err)
		}
	}
}

func TestPathRegistryRejectsExactAndASCIIFoldedDuplicates(t *testing.T) {
	registry := newPathRegistry()
	if err := registry.add("Skills/Review/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	if err := registry.add("Skills/Review/SKILL.md"); err == nil || !strings.Contains(err.Error(), "path_duplicate") {
		t.Fatalf("exact duplicate error = %v", err)
	}
	registry = newPathRegistry()
	if err := registry.add("Skills/Review/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	if err := registry.add("skills/review/skill.md"); err == nil || !strings.Contains(err.Error(), "path_case_collision") {
		t.Fatalf("case collision error = %v", err)
	}
	registry = newPathRegistry()
	if err := registry.add("skills/é/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	if err := registry.add("skills/É/SKILL.md"); err != nil {
		t.Fatalf("non-ASCII case must not be folded: %v", err)
	}
}
