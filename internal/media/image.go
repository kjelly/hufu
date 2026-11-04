package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	"golang.org/x/image/webp"
)

// Validate performs a bounded full decode of original bytes. It never rewrites
// metadata, normalizes pixels or retains the decoded image.
func Validate(ctx context.Context, data []byte, limits Limits) (Ref, error) {
	l, err := limits.Resolve()
	if err != nil {
		return Ref{}, err
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	if len(data) == 0 {
		return Ref{}, Reject(DecodeInvalid, "empty image")
	}
	if int64(len(data)) > l.MaxBytesPerImage {
		return Ref{}, Reject(LimitExceeded, "image exceeds byte budget")
	}
	mime, config, decode, err := codec(data)
	if err != nil {
		return Ref{}, err
	}
	if err := decodeSlots.Acquire(ctx, 1); err != nil {
		return Ref{}, err
	}
	defer decodeSlots.Release(1)
	cfg, err := config(bytes.NewReader(data))
	if err != nil {
		return Ref{}, Reject(DecodeInvalid, "invalid image header")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > l.MaxSide || cfg.Height > l.MaxSide || int64(cfg.Width)*int64(cfg.Height) > l.MaxPixels {
		return Ref{}, Reject(LimitExceeded, "image exceeds pixel budget")
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	img, err := decode(bytes.NewReader(data))
	if err != nil {
		return Ref{}, Reject(DecodeInvalid, "invalid or truncated image")
	}
	if img.Bounds().Dx() != cfg.Width || img.Bounds().Dy() != cfg.Height {
		return Ref{}, Reject(DecodeInvalid, "image dimensions changed during decode")
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])
	return Ref{ID: ContentID(digest, mime), SHA256: digest, MediaType: mime, ByteSize: int64(len(data)), Width: cfg.Width, Height: cfg.Height}, nil
}

func codec(data []byte) (string, func(io.Reader) (image.Config, error), func(io.Reader) (image.Image, error), error) {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		if err := checkPNG(data); err != nil {
			return "", nil, nil, err
		}
		return "image/png", png.DecodeConfig, png.Decode, nil
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg", jpeg.DecodeConfig, jpeg.Decode, nil
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		if err := checkWebP(data); err != nil {
			return "", nil, nil, err
		}
		return "image/webp", webp.DecodeConfig, webp.Decode, nil
	default:
		return "", nil, nil, Reject(FormatUnsupported, "expected PNG, JPEG or static WebP")
	}
}

func checkPNG(data []byte) error {
	for offset := 8; offset < len(data); {
		if len(data)-offset < 12 {
			return Reject(DecodeInvalid, "truncated PNG chunk")
		}
		n := int64(binary.BigEndian.Uint32(data[offset : offset+4]))
		if n > int64(len(data)-offset-12) {
			return Reject(DecodeInvalid, "truncated PNG chunk")
		}
		kind := string(data[offset+4 : offset+8])
		if kind == "acTL" || kind == "fcTL" || kind == "fdAT" {
			return Reject(FormatUnsupported, "animated PNG is unsupported")
		}
		offset += 12 + int(n)
		if kind == "IEND" {
			if n != 0 || offset != len(data) {
				return Reject(DecodeInvalid, "invalid PNG end")
			}
			return nil
		}
	}
	return Reject(DecodeInvalid, "missing PNG end")
}

func checkWebP(data []byte) error {
	if int64(binary.LittleEndian.Uint32(data[4:8]))+8 != int64(len(data)) {
		return Reject(DecodeInvalid, "invalid WebP container size")
	}
	for offset := 12; offset < len(data); {
		if len(data)-offset < 8 {
			return Reject(DecodeInvalid, "truncated WebP chunk")
		}
		n := int64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if n > int64(len(data)-offset-8) {
			return Reject(DecodeInvalid, "truncated WebP chunk")
		}
		kind := string(data[offset : offset+4])
		if kind == "ANIM" || kind == "ANMF" || (kind == "VP8X" && n > 0 && data[offset+8]&2 != 0) {
			return Reject(FormatUnsupported, "animated WebP is unsupported")
		}
		offset += 8 + int(n) + (int(n) & 1)
		if offset > len(data) {
			return Reject(DecodeInvalid, "missing WebP chunk padding")
		}
	}
	return nil
}

// ReadFile checks regular-file identity and size on the same nonblocking,
// no-symlink handle used for reading. All errors omit the supplied path.
func ReadFile(ctx context.Context, path string, limits Limits) ([]byte, error) {
	l, err := limits.Resolve()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := openRegularFile(path)
	if err != nil {
		if classified, ok := errors.AsType[*Error](err); ok {
			return nil, classified
		}
		return nil, Reject(InputMissing, "cannot open a regular image without symlinks")
	}
	defer func() { _ = f.Close() }()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, Reject(AccessDenied, "image source must be a regular file")
	}
	if stat.Size() <= 0 {
		return nil, Reject(DecodeInvalid, "empty image file")
	}
	if stat.Size() > l.MaxBytesPerImage {
		return nil, Reject(LimitExceeded, "image file exceeds byte budget")
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: f}, l.MaxBytesPerImage))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, Reject(DecodeInvalid, "image read failed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, Reject(DecodeInvalid, "image stat failed after read")
	}
	if after.Size() > l.MaxBytesPerImage {
		return nil, Reject(LimitExceeded, "image file grew beyond byte budget")
	}
	return data, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
