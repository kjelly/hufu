package improve

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigureAnalyticsSQLiteKeepsDefaultsAfterBenchmarkGate(t *testing.T) {
	if len(analyticsPragmaStatements) != 0 {
		t.Fatalf("retained analytics PRAGMAs = %v, want SQLite defaults", analyticsPragmaStatements)
	}
	session := newTestSession(t)
	if err := configureAnalyticsSQLite(t.Context(), session.conn); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureAnalyticsSQLiteLeavesCanonicalDatabaseUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	canonical, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := canonical.ExecContext(t.Context(), `CREATE TABLE marker (value TEXT); INSERT INTO marker VALUES ('canonical'); PRAGMA user_version = 7`); err != nil {
		_ = canonical.Close()
		t.Fatal(err)
	}
	if err := canonical.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	session, err := openSQLiteAnalyticsSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("opening configured analytics SQLite changed canonical context.sqlite bytes")
	}
}
