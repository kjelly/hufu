package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"golang.org/x/sync/semaphore"
)

func TestStageBase64ImagePreservesBytesAndReleasesQuota(t *testing.T) {
	original := testPNG(t)
	encoded := base64.StdEncoding.EncodeToString(original)
	budget := int64(len(original))
	pool := semaphore.NewWeighted(budget)
	staged, err := stageBase64Image(t.Context(), encoded, "image/png", Limits{MaxBytesPerImage: budget}, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(staged.Close)
	if err := staged.WithBytes(t.Context(), func(data []byte, ref Ref) error {
		if !bytes.Equal(data, original) || ref.MediaType != "image/png" || ref.ByteSize != budget {
			t.Fatal("tool image bytes or immutable properties changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if extra, err := stageBase64Image(t.Context(), encoded, "image/png", Limits{}, pool); err == nil {
		extra.Close()
		t.Fatal("tool image staging exceeded its budget")
	} else {
		requireCode(t, err, LimitExceeded)
	}
	owned := staged.data
	staged.Close()
	if !bytes.Equal(owned, make([]byte, len(owned))) || !pool.TryAcquire(budget) {
		t.Fatal("closing tool staging retained bytes or quota")
	}
	pool.Release(budget)
}

func TestStageBase64ImageRejectsInvalidContentWithoutRetainingQuota(t *testing.T) {
	data := testPNG(t)
	encoded := base64.StdEncoding.EncodeToString(data)
	for _, test := range []struct {
		name, encoded, mime string
		limit               int64
		code                Code
	}{
		{"empty", "", "image/png", 0, DecodeInvalid},
		{"invalid alphabet", "!!!!", "image/png", 0, DecodeInvalid},
		{"noncanonical padding", "Zh==", "image/png", 0, DecodeInvalid},
		{"trailing newline", encoded + "\n", "image/png", 0, DecodeInvalid},
		{"embedded newline", encoded[:4] + "\n" + encoded[4:] + "\n\n\n", "image/png", 0, DecodeInvalid},
		{"unpadded", "aGk", "image/png", 0, DecodeInvalid},
		{"invalid format", "aGk=", "image/png", 0, FormatUnsupported},
		{"unsupported MIME", encoded, "image/gif", 0, FormatUnsupported},
		{"MIME mismatch", encoded, "image/jpeg", 0, IntegrityMismatch},
		{"encoded size", encoded, "image/png", 1, LimitExceeded},
		{"decoded size", encoded, "image/png", int64(len(data)) - 1, LimitExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			const budget = 1 << 20
			pool := semaphore.NewWeighted(budget)
			staged, err := stageBase64Image(t.Context(), test.encoded, test.mime, Limits{MaxBytesPerImage: test.limit}, pool)
			if staged != nil {
				staged.Close()
				t.Fatal("invalid tool data became staged input")
			}
			requireCode(t, err, test.code)
			if !pool.TryAcquire(budget) {
				t.Fatal("rejected tool data retained staging quota")
			}
			pool.Release(budget)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := StageBase64Image(ctx, encoded, "image/png", Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatal("tool image staging ignored cancellation", err)
	}
}
