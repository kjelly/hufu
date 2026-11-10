package media

import (
	"context"
	"errors"
	"testing"
)

func TestTransportFramesReserveEachOccurrencePadding(t *testing.T) {
	reservation, err := ReserveTransportFrames(t.Context(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reservation.Release)
	// Two one-byte frames each need four encoded bytes, in three copies.
	const remainder = 12*MaxConcurrentRawBytes - 24
	var held int64
	t.Cleanup(func() {
		if held != 0 {
			encodedTransportSlots.Release(held)
		}
	})
	if !encodedTransportSlots.TryAcquire(remainder) {
		t.Fatal("valid multi-image reservation exceeded its exact encoding budget")
	}
	held = remainder
	if encodedTransportSlots.TryAcquire(1) {
		held++
		t.Fatal("aggregate rounding undercounted per-image Base64 padding")
	}
	reservation.Release()
	reservation.Release()
	if !encodedTransportSlots.TryAcquire(24) {
		t.Fatal("released bundle leaked its encoding reservation")
	}
	held += 24
}

func TestTransportFramesRejectInvalidBundlesAndCancellation(t *testing.T) {
	for _, frames := range [][]int64{nil, {0}, {-1}, {MaxConcurrentRawBytes, 1}, make([]int64, MaxCount+1)} {
		_, err := ReserveTransportFrames(t.Context(), frames...)
		requireCode(t, err, LimitExceeded)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cancelled, err := ReserveTransportFrames(ctx, 1, 1)
	if cancelled != nil {
		t.Cleanup(cancelled.Release)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled bundle acquired transport credit")
	}
	reservation, err := ReserveTransportFrames(t.Context(), MaxConcurrentRawBytes)
	if err != nil {
		t.Fatal("invalid or cancelled bundle leaked raw credit")
	}
	t.Cleanup(reservation.Release)
}
