package context

import (
	"os"
	"testing"
)

// TestCanonicalMaintenanceDiscovery records WP-11B storage pressure without
// executing optimize, checkpoint, vacuum, or any proposed maintenance path.
func TestCanonicalMaintenanceDiscovery(t *testing.T) {
	if os.Getenv("HUFU_SQLITE_DISCOVERY") != "1" {
		t.Skip("set HUFU_SQLITE_DISCOVERY=1 to run the maintenance review")
	}
	repo := openCanonicalIndexFixture(t, 10_000)
	active, err := repo.StorageDiagnostics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("state=active rows=%d fts_rows=%d page_count=%d freelist_count=%d page_size=%d journal_mode=%s wal_autocheckpoint=%d database_bytes=%d wal_bytes=%d", active.ContextRows, active.FTSRows, active.PageCount, active.FreelistCount, active.PageSize, active.JournalMode, active.WALAutoCheckpoint, storageFileSize(t, repo.path), optionalStorageFileSize(t, repo.path+"-wal"))
	path := repo.path
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	quiescent, err := readOnly.StorageDiagnostics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("state=quiescent rows=%d fts_rows=%d page_count=%d freelist_count=%d page_size=%d journal_mode=%s wal_autocheckpoint=%d database_bytes=%d wal_bytes=%d", quiescent.ContextRows, quiescent.FTSRows, quiescent.PageCount, quiescent.FreelistCount, quiescent.PageSize, quiescent.JournalMode, quiescent.WALAutoCheckpoint, storageFileSize(t, path), optionalStorageFileSize(t, path+"-wal"))
}

func storageFileSize(t testing.TB, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func optionalStorageFileSize(t testing.TB, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}
