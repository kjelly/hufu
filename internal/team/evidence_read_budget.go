package team

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
)

type artifactReadBudgetKey struct{}

// ArtifactReadBudget accounts for actual artifact bytes across all stores in
// one inspection. Err is sticky because audit projections may turn a failed
// artifact check into a diagnostic; bounded callers must still fail the query.
type ArtifactReadBudget struct {
	mu        sync.Mutex
	remaining int64
	err       error
}

func WithArtifactReadBudget(ctx context.Context, maxBytes int64) (context.Context, *ArtifactReadBudget) {
	if ctx == nil {
		ctx = context.Background()
	}
	budget := &ArtifactReadBudget{remaining: maxBytes}
	if maxBytes <= 0 {
		budget.err = ErrReadLimit
	}
	return context.WithValue(ctx, artifactReadBudgetKey{}, budget), budget
}

func (b *ArtifactReadBudget) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func artifactReadContextError(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if budget, ok := ctx.Value(artifactReadBudgetKey{}).(*ArtifactReadBudget); ok {
		return budget.Err()
	}
	return nil
}

func readArtifactFile(ctx context.Context, path string) ([]byte, error) {
	return readArtifactFileWithin(ctx, path, 0)
}

// A positive limit caps this file in addition to any aggregate read budget.
func readArtifactFileWithin(ctx context.Context, path string, limit int) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	budget, bounded := ctx.Value(artifactReadBudgetKey{}).(*ArtifactReadBudget)
	if !bounded && limit == 0 {
		return os.ReadFile(path)
	}
	if bounded {
		budget.mu.Lock()
		defer budget.mu.Unlock()
		if budget.err != nil {
			return nil, budget.err
		}
	}
	file, err := openBoundedArtifact(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if !bounded {
		return readBoundedRegularFile(ctx, file, limit)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact must be a regular file")
	}
	if info.Size() > budget.remaining {
		budget.err = ErrReadLimit
		return nil, budget.err
	}
	if limit > 0 && info.Size() > int64(limit) {
		return nil, ErrReadLimit
	}
	// Do not use a limit+1 sentinel: even the probing byte would exceed the
	// declared aggregate budget. A second stat detects growth past the limit.
	remainingBeforeRead := budget.remaining
	admitted := remainingBeforeRead
	if limit > 0 && int64(limit) < admitted {
		admitted = int64(limit)
	}
	data, err := io.ReadAll(io.LimitReader(contextArtifactReader{ctx: ctx, reader: file}, admitted))
	budget.remaining -= int64(len(data))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if after.Size() > remainingBeforeRead {
		budget.err = ErrReadLimit
		return nil, budget.err
	}
	if after.Size() > admitted {
		return nil, ErrReadLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

type contextArtifactReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextArtifactReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func readBoundedRegularFile(ctx context.Context, file *os.File, limit int) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact must be a regular file")
	}
	if info.Size() > int64(limit) {
		return nil, ErrReadLimit
	}
	body, err := io.ReadAll(io.LimitReader(contextArtifactReader{ctx: ctx, reader: file}, int64(limit)+1))
	if err == nil && len(body) > limit {
		err = ErrReadLimit
	}
	return body, err
}
