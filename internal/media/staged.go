package media

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

// Staging has a separate logical-byte budget from request transport. Holding
// transport slots while publishing staged inputs would nest reservations and
// can deadlock a fair semaphore. Neither budget claims to bound process RSS.
const MaxConcurrentStagedBytes int64 = 64 << 20

var stagingSlots = semaphore.NewWeighted(MaxConcurrentStagedBytes)

// StagedImage owns validated process-local bytes, without publishing an
// artifact or granting a model access. Close clears the bytes and returns their
// quota after borrowers exit. It must not be copied after construction.
type StagedImage struct {
	mu     sync.RWMutex
	data   []byte
	ref    Ref
	pool   *semaphore.Weighted
	quota  int64
	closed bool
}

// StageImage reads and validates the selected file now. Later publication uses
// these bytes even if the original file is changed or removed.
func StageImage(ctx context.Context, path string, limits Limits) (*StagedImage, error) {
	return stageImage(ctx, path, limits, stagingSlots)
}

func stageImage(ctx context.Context, path string, limits Limits, pool *semaphore.Weighted) (*StagedImage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits, err := limits.Resolve()
	if err != nil {
		return nil, err
	}
	reserved := limits.MaxBytesPerImage
	if !pool.TryAcquire(reserved) {
		return nil, Reject(LimitExceeded, "image staging budget is exhausted")
	}
	var data []byte
	transferred := false
	defer func() {
		if !transferred {
			clear(data)
			pool.Release(reserved)
		}
	}()
	data, err = ReadFile(ctx, path, limits)
	if err != nil {
		return nil, err
	}
	ref, err := Validate(ctx, data, limits)
	if err != nil {
		return nil, err
	}
	if err := CheckBundle([]Ref{ref}, limits); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	actual := int64(len(data))
	pool.Release(reserved - actual)
	reserved = actual
	transferred = true
	return &StagedImage{data: data, ref: ref, pool: pool, quota: reserved}, nil
}

// Ref returns immutable content properties, never a byte-access grant.
func (s *StagedImage) Ref() Ref {
	if s == nil {
		return Ref{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ref
}

// WithBytes lends read-only bytes to trusted host ingress, synchronously. The
// callback must neither modify the slice nor retain it after returning. A
// provider must consume canonical CAS input under its own ordinary admission.
func (s *StagedImage) WithBytes(ctx context.Context, consume func([]byte, Ref) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || consume == nil {
		return Reject(InputMissing, "staged image is unavailable")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Reject(InputMissing, "staged image was released")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return consume(s.data, s.ref)
}

// Close is idempotent and waits for active borrowers before clearing bytes.
func (s *StagedImage) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	clear(s.data)
	s.data = nil
	s.pool.Release(s.quota)
	s.quota = 0
}
