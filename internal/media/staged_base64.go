package media

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"golang.org/x/sync/semaphore"
)

// StageBase64Image admits encoded tool data into the existing staging budget.
// It validates original bytes and MIME without publishing a CAS reference or
// granting any provider access. Close returns the quota and clears the bytes.
func StageBase64Image(ctx context.Context, encoded, mediaType string, limits Limits) (*StagedImage, error) {
	return stageBase64Image(ctx, encoded, mediaType, limits, stagingSlots)
}

func stageBase64Image(ctx context.Context, encoded, mediaType string, limits Limits, pool *semaphore.Weighted) (*StagedImage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits, err := limits.Resolve()
	if err != nil {
		return nil, err
	}
	if mediaType != "image/png" && mediaType != "image/jpeg" && mediaType != "image/webp" {
		return nil, Reject(FormatUnsupported, "unsupported tool image MIME")
	}
	if int64(len(encoded)) > ((limits.MaxBytesPerImage+2)/3)*4 {
		return nil, Reject(LimitExceeded, "encoded image exceeds byte budget")
	}
	if len(encoded) == 0 || len(encoded)%4 != 0 || strings.ContainsAny(encoded, "\r\n") {
		return nil, Reject(DecodeInvalid, "invalid image Base64 encoding")
	}
	size := base64.StdEncoding.DecodedLen(len(encoded))
	if strings.HasSuffix(encoded, "==") {
		size -= 2
	} else if strings.HasSuffix(encoded, "=") {
		size--
	}
	if size <= 0 {
		return nil, Reject(DecodeInvalid, "empty encoded image")
	}
	if int64(size) > limits.MaxBytesPerImage {
		return nil, Reject(LimitExceeded, "decoded image exceeds byte budget")
	}
	if !pool.TryAcquire(int64(size)) {
		return nil, Reject(LimitExceeded, "image staging budget is exhausted")
	}
	data := make([]byte, size)
	transferred := false
	defer func() {
		if !transferred {
			clear(data)
			pool.Release(int64(size))
		}
	}()
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(encoded))
	decoded, err := io.ReadFull(decoder, data)
	if err != nil || decoded != size {
		return nil, Reject(DecodeInvalid, "invalid image Base64 encoding")
	}
	var extra [1]byte
	if n, err := decoder.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return nil, Reject(DecodeInvalid, "invalid image Base64 encoding")
	}
	ref, err := Validate(ctx, data, limits)
	if err != nil {
		return nil, err
	}
	if ref.MediaType != mediaType {
		return nil, Reject(IntegrityMismatch, "tool image MIME differs from its content")
	}
	if err := CheckBundle([]Ref{ref}, limits); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	transferred = true
	return &StagedImage{data: data, ref: ref, pool: pool, quota: int64(size)}, nil
}
