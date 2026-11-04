package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.RGBA{R: 128, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func requireCode(t *testing.T, err error, code Code) {
	t.Helper()
	mediaErr, ok := errors.AsType[*Error](err)
	if !ok || mediaErr.Code != code {
		t.Fatalf("error=%v, want code %s", err, code)
	}
}

func pngChunk(kind string, data []byte) []byte {
	chunk := make([]byte, 12+len(data))
	binary.BigEndian.PutUint32(chunk, uint32(len(data)))
	copy(chunk[4:8], kind)
	copy(chunk[8:], data)
	binary.BigEndian.PutUint32(chunk[8+len(data):], crc32.ChecksumIEEE(chunk[4:8+len(data)]))
	return chunk
}

func TestValidate(t *testing.T) {
	pngData := testPNG(t)
	var jpegBuffer bytes.Buffer
	if err := jpeg.Encode(&jpegBuffer, image.NewRGBA(image.Rect(0, 0, 2, 3)), nil); err != nil {
		t.Fatal(err)
	}
	// A small APP1 payload ensures the original JPEG metadata bytes participate
	// in content identity even though the pixel decoder ignores that segment.
	exifJPEG := append(bytes.Clone(jpegBuffer.Bytes()[:2]), []byte("\xff\xe1\x00\x08Exif\x00\x00")...)
	exifJPEG = append(exifJPEG, jpegBuffer.Bytes()[2:]...)
	// A single solid pixel, encoded as a static VP8 frame. No external files,
	// network access, decoder binary or third-party image is required.
	webp, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, mime    string
		data          []byte
		width, height int
	}{
		{"png", "image/png", pngData, 2, 3}, {"jpeg", "image/jpeg", jpegBuffer.Bytes(), 2, 3}, {"jpeg-exif", "image/jpeg", exifJPEG, 2, 3}, {"webp", "image/webp", webp, 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := Validate(t.Context(), tt.data, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(tt.data)
			if ref.SHA256 != hex.EncodeToString(digest[:]) || ref.MediaType != tt.mime || ref.Width != tt.width || ref.Height != tt.height || ref.ByteSize != int64(len(tt.data)) {
				t.Fatalf("unexpected metadata: %+v", ref)
			}
			if err := ref.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	animatedPNG := append(bytes.Clone(pngData[:33]), pngChunk("acTL", make([]byte, 8))...)
	animatedPNG = append(animatedPNG, pngData[33:]...)
	animatedWebP := append([]byte("RIFF\x10\x00\x00\x00WEBPANIM\x04\x00\x00\x00"), make([]byte, 4)...)
	hugePNG := bytes.Clone(pngData)
	binary.BigEndian.PutUint32(hugePNG[16:20], 8193)
	binary.BigEndian.PutUint32(hugePNG[29:33], crc32.ChecksumIEEE(hugePNG[12:29]))
	for _, tt := range []struct {
		name   string
		data   []byte
		limits Limits
		code   Code
	}{
		{"empty", nil, Limits{}, DecodeInvalid}, {"text", []byte("not an image"), Limits{}, FormatUnsupported},
		{"truncated", pngData[:len(pngData)-12], Limits{}, DecodeInvalid},
		{"truncated-jpeg", jpegBuffer.Bytes()[:len(jpegBuffer.Bytes())-5], Limits{}, DecodeInvalid},
		{"truncated-webp", webp[:len(webp)-1], Limits{}, DecodeInvalid},
		{"bad-pixels", append(bytes.Clone(pngData[:40]), pngData[41:]...), Limits{}, DecodeInvalid},
		{"apng", animatedPNG, Limits{}, FormatUnsupported}, {"animated-webp", animatedWebP, Limits{}, FormatUnsupported},
		{"over-side", hugePNG, Limits{}, LimitExceeded}, {"policy-pixels", pngData, Limits{MaxPixels: 5}, LimitExceeded},
		{"policy-bytes", pngData, Limits{MaxBytesPerImage: 1}, LimitExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) { _, err := Validate(t.Context(), tt.data, tt.limits); requireCode(t, err, tt.code) })
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "misleading.txt")
	data := testPNG(t)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(t.Context(), path, Limits{})
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read: %v", err)
	}
	if _, err := Validate(t.Context(), got, Limits{}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, path string
		setup      func() error
	}{
		{"file-symlink", filepath.Join(dir, "link"), func() error { return os.Symlink(path, filepath.Join(dir, "link")) }},
		{"parent-symlink", filepath.Join(dir, "parent-link", "misleading.txt"), func() error { return os.Symlink(dir, filepath.Join(dir, "parent-link")) }},
		{"directory", dir, func() error { return nil }},
		{"device", "/dev/null", func() error { return nil }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.setup(); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadFile(t.Context(), tt.path, Limits{}); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
	_, err = ReadFile(t.Context(), path, Limits{MaxBytesPerImage: 1})
	requireCode(t, err, LimitExceeded)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadFile(ctx, path, Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
}

func TestDecodeLimiterCancellation(t *testing.T) {
	if err := decodeSlots.Acquire(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() { decodeSlots.Release(1) })
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := make(chan struct{})
	waiting := &observedContext{Context: ctx, observe: sync.OnceFunc(func() { close(observed) })}
	result := make(chan error, 1)
	data := testPNG(t)
	go func() { _, err := Validate(waiting, data, Limits{}); result <- err }()
	<-observed
	cancel()
	err := <-result
	release()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled decode: %v", err)
	}
	if _, err := Validate(t.Context(), testPNG(t), Limits{}); err != nil {
		t.Fatal(err)
	}
}

type observedContext struct {
	context.Context
	observe func()
}

func (c *observedContext) Done() <-chan struct{} {
	c.observe()
	return c.Context.Done()
}

func TestTransportReservation(t *testing.T) {
	r, err := ReserveTransport(t.Context(), MaxConcurrentRawBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Release)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReserveTransport(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("reservation: %v", err)
	}
	r.Release()
	r.Release()
	next, err := ReserveTransport(t.Context(), MaxConcurrentRawBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Release)
	next.Release()
	_, err = ReserveTransport(t.Context(), MaxConcurrentRawBytes+1)
	requireCode(t, err, LimitExceeded)
	// Two frames can need more padding than one frame for the same total raw
	// bytes. Both reservations must fit without waiting on one another.
	fragment, err := ReserveTransport(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer fragment.Release()
	deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	rest, err := ReserveTransport(deadline, MaxConcurrentRawBytes-2)
	if err != nil {
		t.Fatalf("fragmented reservation deadlock: %v", err)
	}
	rest.Release()
}

func TestCheckBundle(t *testing.T) {
	ref, err := Validate(t.Context(), testPNG(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckBundle([]Ref{ref, ref}, Limits{}); err != nil {
		t.Fatal(err)
	}
	requireCode(t, CheckBundle([]Ref{ref, ref}, Limits{MaxCount: 1}), LimitExceeded)
	requireCode(t, CheckBundle([]Ref{ref, ref}, Limits{MaxBytesPerBundle: ref.ByteSize}), LimitExceeded)
	for _, l := range []Limits{{MaxCount: MaxCount + 1}, {MaxPixels: MaxPixels + 1}, {MaxBranchBytes: MaxBranchBytes + 1}, {MaxSide: -1}} {
		_, err := l.Resolve()
		requireCode(t, err, LimitExceeded)
	}
}
