package team

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"

	"github.com/kjelly/hufu/internal/fsutil"
	"github.com/kjelly/hufu/internal/runtimestore"
)

const fanoutDocumentMaxBytes = 2 * resultCollectionAcceptedMaxBytes

func fanoutStagePath(workspace, key string) string {
	return filepath.Join(runtimestore.ArtifactRoot(workspace), "fanout-stage", sha256HexBytes([]byte(key)))
}

// Stage completed outputs separately from SQLite and the public artifact
// namespace. Those publications can then be retried without executing work.
func stageFanoutBytes(ctx context.Context, workspace, key string, body []byte, limit int) error {
	if len(body) > limit {
		return rejectLocalResult(RepairFailureSource, ErrReadLimit)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path := fanoutStagePath(workspace, key)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := fsutil.SyncDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := fsutil.AtomicCreateFile(path, body, 0o600); !errors.Is(err, os.ErrExist) {
		return err
	}
	prior, err := readFanoutStage(ctx, workspace, key, limit)
	if err != nil {
		return err
	}
	if !bytes.Equal(prior, body) {
		return errResultCollectionStale
	}
	return fsutil.SyncDir(dir)
}

func readFanoutStage(ctx context.Context, workspace, key string, limit int) ([]byte, error) {
	file, err := openBoundedArtifact(fanoutStagePath(workspace, key))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return readBoundedRegularFile(ctx, file, limit)
}

func removeFanoutStage(workspace, key string) {
	if err := os.Remove(fanoutStagePath(workspace, key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("warning: release published fanout handoff: %v", err)
	}
}

func (c *Coordinator) readFanoutDocument(ctx context.Context, name string) (body []byte, pending bool, err error) {
	workspace := c.artifactStoreRootPath()
	body, found, err := workspaceDocs(workspace).getDocument(name)
	if err != nil {
		return nil, false, err
	}
	if found {
		if len(body) == 0 || len(body) > fanoutDocumentMaxBytes {
			return nil, false, errResultCollectionStale
		}
		return body, false, nil
	}
	body, err = readFanoutStage(ctx, workspace, name, fanoutDocumentMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err == nil && len(body) == 0 {
		err = errResultCollectionStale
	}
	return body, err == nil, err
}

func (c *Coordinator) stageFanoutDocument(ctx context.Context, name string, body []byte) error {
	return rejectLocalResultPersistence(stageFanoutBytes(ctx, c.artifactStoreRootPath(), name, body, fanoutDocumentMaxBytes))
}

func (c *Coordinator) publishFanoutDocument(ctx context.Context, name string, body []byte) error {
	if err := c.sealCollectionDocument(ctx, name, body); err != nil {
		return rejectLocalResultPersistence(err)
	}
	removeFanoutStage(c.artifactStoreRootPath(), name)
	return nil
}
