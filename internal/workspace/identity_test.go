package workspace

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestIDGeneratorProducesOpaque128BitIDs(t *testing.T) {
	for _, kind := range []IDKind{ProjectIDKind, WorkspaceIDKind, TrashIDKind, OperationIDKind} {
		got, err := NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0xab}, 16))).New(kind)
		if err != nil {
			t.Fatal(err)
		}
		want := string(kind) + "_" + strings.Repeat("ab", 16)
		if got != want {
			t.Fatalf("id = %q, want %q", got, want)
		}
	}
}

func TestIDGeneratorFailsClosedOnEntropyFailure(t *testing.T) {
	want := errors.New("entropy unavailable")
	_, err := NewIDGenerator(errorReader{err: want}).New(ProjectIDKind)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want wrapped %v", err, want)
	}
}

func TestNormalizeAliasAndTeamName(t *testing.T) {
	alias, err := NormalizeAlias("  My.Project_1  ")
	if err != nil || alias != "my.project_1" {
		t.Fatalf("NormalizeAlias = %q, %v", alias, err)
	}
	for _, invalid := range []string{"", "-bad", "has space", strings.Repeat("a", 65)} {
		if _, err = NormalizeAlias(invalid); err == nil {
			t.Fatalf("NormalizeAlias(%q) unexpectedly succeeded", invalid)
		}
	}
	team, err := NormalizeTeamName(" Dev ")
	if err != nil || team != "dev" {
		t.Fatalf("NormalizeTeamName = %q, %v", team, err)
	}
	for _, invalid := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err = NormalizeTeamName(invalid); err == nil {
			t.Fatalf("NormalizeTeamName(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestProjectSlug(t *testing.T) {
	for input, want := range map[string]string{
		"/work/My Project!!!": "my-project",
		"/work/...":           "project",
		"/work/A--B":          "a-b",
	} {
		if got := projectSlug(input); got != want {
			t.Fatalf("projectSlug(%q) = %q, want %q", input, got, want)
		}
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
