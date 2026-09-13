package team

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadInvariantCatalogNormalizesDeterministically(t *testing.T) {
	dir := t.TempDir()
	writeInvariantTestFile(t, dir, `schema-version: 1
invariants:
  - id: z-last
    statement: |
      keep the final boundary
    severity: warning
    applies-to: [internal/team/z.go, internal/team/]
  - id: a-first
    statement: "preserve the first boundary"
    severity: error
    applies-to: ["*", "*"]
`)

	catalog, err := loadInvariantCatalog(dir)
	if err != nil {
		t.Fatalf("loadInvariantCatalog: %v", err)
	}
	if len(catalog) != 2 || catalog[0].ID != "a-first" || catalog[1].ID != "z-last" {
		t.Fatalf("catalog order = %#v", catalog)
	}
	if !slices.Equal(catalog[0].AppliesTo, []string{"*"}) {
		t.Fatalf("deduplicated applies-to = %#v", catalog[0].AppliesTo)
	}
	if !slices.Equal(catalog[1].AppliesTo, []string{"internal/team/", "internal/team/z.go"}) {
		t.Fatalf("sorted applies-to = %#v", catalog[1].AppliesTo)
	}

	clone := cloneInvariantCatalog(catalog)
	clone[0].AppliesTo[0] = "changed"
	if catalog[0].AppliesTo[0] != "*" {
		t.Fatal("cloneInvariantCatalog aliases applies-to storage")
	}
}

func TestLoadInvariantCatalogAbsent(t *testing.T) {
	catalog, err := loadInvariantCatalog(t.TempDir())
	if err != nil || catalog != nil {
		t.Fatalf("absent catalog = %#v, %v", catalog, err)
	}
}

func TestLoadTeamOwnsInvariantCatalogWithoutTemplating(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: catalog-team\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "coordinator.md"), []byte("Coordinate the team.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeInvariantTestFile(t, dir, strings.Replace(validInvariantManifest(""), "preserve behavior", "preserve ${TEAM_NAME}", 1))

	session, err := LoadTeam(dir, map[string]string{"TEAM_NAME": "expanded"}, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if len(session.InvariantCatalog) != 1 || session.InvariantCatalog[0].Statement != "preserve ${TEAM_NAME}" {
		t.Fatalf("catalog was not loaded as literal repository policy: %#v", session.InvariantCatalog)
	}
}

func TestLoadInvariantCatalogRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "empty", content: "", want: "EOF"},
		{name: "future schema", content: "schema-version: 2\ninvariants: []\n", want: "schema-version must be 1"},
		{name: "empty entries", content: "schema-version: 1\ninvariants: []\n", want: "at least one"},
		{name: "unknown field", content: validInvariantManifest("    mystery: true\n"), want: "field mystery not found"},
		{name: "duplicate mapping", content: "schema-version: 1\nschema-version: 1\ninvariants: []\n", want: "already defined"},
		{name: "trailing document", content: validInvariantManifest("") + "---\n{}\n", want: "multiple YAML documents"},
		{name: "bad id", content: strings.Replace(validInvariantManifest(""), "safe-id", "Bad.ID", 1), want: "must match"},
		{name: "duplicate id", content: validInvariantManifest("") + "  - id: safe-id\n    statement: second\n    severity: info\n    applies-to: ['*']\n", want: "is duplicated"},
		{name: "empty statement", content: strings.Replace(validInvariantManifest(""), "statement: preserve behavior", "statement: '   '", 1), want: "statement must contain"},
		{name: "bad severity", content: strings.Replace(validInvariantManifest(""), "severity: error", "severity: critical", 1), want: "severity must be"},
		{name: "absolute path", content: strings.Replace(validInvariantManifest(""), "internal/team/", "/internal/team/", 1), want: "repository-relative"},
		{name: "parent path", content: strings.Replace(validInvariantManifest(""), "internal/team/", "internal/../team/", 1), want: "dot, or parent"},
		{name: "glob path", content: strings.Replace(validInvariantManifest(""), "internal/team/", "internal/*/", 1), want: "exact repository-relative"},
		{name: "backslash path", content: strings.Replace(validInvariantManifest(""), "internal/team/", `internal\team\`, 1), want: "slash-separated"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeInvariantTestFile(t, dir, test.content)
			_, err := loadInvariantCatalog(dir)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestLoadInvariantCatalogRejectsSymlinkAndOversize(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "catalog-source.yaml")
		if err := os.WriteFile(target, []byte(validInvariantManifest("")), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "invariants.yaml")); err != nil {
			t.Fatal(err)
		}
		if _, err := loadInvariantCatalog(dir); err == nil || !strings.Contains(err.Error(), "not a symlink") {
			t.Fatalf("symlink error = %v", err)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		dir := t.TempDir()
		writeInvariantTestFile(t, dir, strings.Repeat("x", invariantManifestMaxBytes+1))
		if _, err := loadInvariantCatalog(dir); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversize error = %v", err)
		}
	})
}

func validInvariantManifest(extra string) string {
	return "schema-version: 1\ninvariants:\n" +
		"  - id: safe-id\n" +
		"    statement: preserve behavior\n" +
		"    severity: error\n" +
		"    applies-to: [internal/team/]\n" + extra
}

func writeInvariantTestFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "invariants.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
