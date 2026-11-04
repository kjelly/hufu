// Package media owns validated, reference-only image inputs. A Ref identifies
// content; possession of a Ref does not authorize a task or provider to read it.
package media

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const Version = 1

type Code string

const (
	Unsupported        Code = "IMAGE_INPUT_UNSUPPORTED"
	FormatUnsupported  Code = "MEDIA_UNSUPPORTED_FORMAT"
	LimitExceeded      Code = "MEDIA_LIMIT_EXCEEDED"
	DecodeInvalid      Code = "MEDIA_DECODE_INVALID"
	IntegrityMismatch  Code = "IMAGE_INTEGRITY_MISMATCH"
	InputMissing       Code = "IMAGE_INPUT_MISSING"
	AccessDenied       Code = "MEDIA_ACCESS_DENIED"
	EgressDenied       Code = "MEDIA_EGRESS_DENIED"
	BindingConflict    Code = "MEDIA_BINDING_CONFLICT"
	DurabilityRequired Code = "MEDIA_DURABILITY_REQUIRED"
)

// Error contains safe diagnostics only: never source paths, image bytes or EXIF.
type Error struct {
	Code   Code
	Reason string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Reason }

func Reject(code Code, reason string) error { return &Error{Code: code, Reason: reason} }

type Ref struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
	ByteSize  int64  `json:"byte_size"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// ContentID includes the format version and MIME, independent of publishers.
func ContentID(digest, mime string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("hufu-media-v%d\x00%s\x00%s", Version, digest, mime)))
	return "media-v1-" + hex.EncodeToString(h[:])
}

func IsID(id string) bool {
	if !strings.HasPrefix(id, "media-v1-") || len(id) != len("media-v1-")+64 {
		return false
	}
	return validDigest(strings.TrimPrefix(id, "media-v1-"))
}

func validDigest(digest string) bool {
	if len(digest) != 2*sha256.Size {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(digest) == digest
}

func (r Ref) Validate() error {
	if !validDigest(r.SHA256) || r.ID != ContentID(r.SHA256, r.MediaType) {
		return Reject(IntegrityMismatch, "invalid content identity")
	}
	if r.MediaType != "image/png" && r.MediaType != "image/jpeg" && r.MediaType != "image/webp" {
		return Reject(FormatUnsupported, "unsupported image MIME")
	}
	if r.ByteSize <= 0 || r.ByteSize > MaxBytesPerImage || r.Width <= 0 || r.Height <= 0 ||
		r.Width > MaxSide || r.Height > MaxSide || int64(r.Width)*int64(r.Height) > MaxPixels {
		return Reject(LimitExceeded, "invalid image dimensions or size")
	}
	return nil
}
