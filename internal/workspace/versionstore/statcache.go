package versionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// statCacheRow is a hint that a file with this exact metadata was already
// hashed; correctness never depends on it (a missing row only costs a read).
type statCacheRow struct {
	size     int64
	mtimeNs  int64
	ctimeNs  int64
	inode    uint64
	mode     uint32
	blobHash string
}

func (r statCacheRow) matches(fp fingerprint) bool {
	return r.size == fp.size && r.mtimeNs == fp.mtimeNs && r.ctimeNs == fp.ctimeNs &&
		r.inode == fp.inode && r.mode == uint32(fp.mode)
}

func (s *Store) loadStatCache(ctx context.Context, subjectKey string) (map[string]statCacheRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path, size, mtime_ns, ctime_ns, inode, mode, blob_hash
FROM stat_cache WHERE subject_key=?`, subjectKey)
	if err != nil {
		return nil, fmt.Errorf("read stat cache: %w", err)
	}
	defer func() { _ = rows.Close() }()
	cache := make(map[string]statCacheRow)
	for rows.Next() {
		var rel string
		var row statCacheRow
		var inode int64
		if err = rows.Scan(&rel, &row.size, &row.mtimeNs, &row.ctimeNs, &inode, &row.mode, &row.blobHash); err != nil {
			return nil, fmt.Errorf("scan stat cache: %w", err)
		}
		row.inode = uint64(inode)
		cache[rel] = row
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("read stat cache: %w", err)
	}
	return cache, nil
}

// replaceStatCache stores the regular files of one capture. Files modified
// within statCacheSettleWindow of the capture start are left out so a later
// edit with an identical coarse timestamp can never be mistaken for a hit.
func replaceStatCache(ctx context.Context, tx *sql.Tx, subjectKey string, started time.Time, leaves []leaf) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM stat_cache WHERE subject_key=?", subjectKey); err != nil {
		return fmt.Errorf("clear stat cache: %w", err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO stat_cache(subject_key, path, size, mtime_ns, ctime_ns,
inode, mode, blob_hash) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare stat cache insert: %w", err)
	}
	defer func() { _ = statement.Close() }()
	settledBefore := started.Add(-statCacheSettleWindow).UnixNano()
	for _, captured := range leaves {
		if captured.entry.Kind != EntryBlob || captured.fp.mtimeNs >= settledBefore || captured.fp.ctimeNs >= settledBefore {
			continue
		}
		if _, err = statement.ExecContext(ctx, subjectKey, captured.path, captured.fp.size, captured.fp.mtimeNs,
			captured.fp.ctimeNs, int64(captured.fp.inode), uint32(captured.fp.mode), captured.entry.ObjectHash); err != nil {
			return fmt.Errorf("write stat cache: %w", err)
		}
	}
	return nil
}
