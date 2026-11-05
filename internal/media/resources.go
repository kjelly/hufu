package media

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

// One process-wide decoder bounds peak decoded pixel memory across validators.
var decodeSlots = semaphore.NewWeighted(1)

const MaxConcurrentRawBytes int64 = 64 << 20

// rawTransportSlots reserves raw buffers. encodedTransportSlots separately
// reserves Base64, JSON escaping and one SDK serialization copy (three frames).
// Each one-byte reservation can expand to four Base64 bytes. Account for that
// worst case so fragmented reservations cannot deadlock on Base64 padding
// while already holding all raw slots.
var (
	rawTransportSlots     = semaphore.NewWeighted(MaxConcurrentRawBytes)
	encodedTransportSlots = semaphore.NewWeighted(12 * MaxConcurrentRawBytes)
)

type Reservation struct {
	release func()
}

func (r *Reservation) Release() {
	if r != nil && r.release != nil {
		r.release()
	}
}

// ReserveTransport must be called before loading any provider-bound bytes.
// The caller drains all consumers before Release. Waiting is cancelable.
// Reservations must not be nested: queue fairness can prevent progress when
// another waiter needs the bytes an outer reservation holds. Storage readers
// retain their own reservation until Close.
func ReserveTransport(ctx context.Context, rawBytes int64) (*Reservation, error) {
	return ReserveTransportFrames(ctx, rawBytes)
}

// ReserveTransportFrames reserves one whole request before loading any of its
// images. Each occurrence gets its own Base64 padding, including repetitions;
// the compiler must not acquire nested per-image storage reservations.
func ReserveTransportFrames(ctx context.Context, frames ...int64) (*Reservation, error) {
	if len(frames) == 0 || len(frames) > MaxCount {
		return nil, Reject(LimitExceeded, "invalid transport image count")
	}
	var rawBytes, encoded int64
	for _, size := range frames {
		if size <= 0 || size > MaxConcurrentRawBytes-rawBytes {
			return nil, Reject(LimitExceeded, "transport buffer exceeds process budget")
		}
		rawBytes += size
		encoded += 3 * ((size + 2) / 3 * 4)
	}
	if err := rawTransportSlots.Acquire(ctx, rawBytes); err != nil {
		return nil, err
	}
	if err := encodedTransportSlots.Acquire(ctx, encoded); err != nil {
		rawTransportSlots.Release(rawBytes)
		return nil, err
	}
	return &Reservation{release: sync.OnceFunc(func() { encodedTransportSlots.Release(encoded); rawTransportSlots.Release(rawBytes) })}, nil
}
