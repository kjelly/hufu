package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/fsutil"
	"github.com/kjelly/hufu/internal/media"
)

func isMediaArtifactID(id string) bool { return strings.HasPrefix(id, "media-") }

func imageArtifactRef(image media.Ref) ArtifactRef {
	return ArtifactRef{ID: image.ID, Kind: "image", Role: "media", Path: "media/image/" + image.ID,
		SHA256: image.SHA256, Bytes: image.ByteSize, ByteSize: image.ByteSize, MediaType: image.MediaType}
}

// The shared artifact store owns both namespaces. Media metadata contains only
// immutable content properties. Producer/task ownership belongs to occurrences.
func (s *FileArtifactStore) putMedia(ctx context.Context, req PutArtifactRequest) (ArtifactPutResult, error) {
	limits, err := req.MediaLimits.Resolve()
	if err != nil {
		return ArtifactPutResult{}, err
	}
	reservedBytes := int64(len(req.Content))
	if req.SourcePath != "" {
		reservedBytes = limits.MaxBytesPerImage
	}
	if reservedBytes <= 0 {
		return ArtifactPutResult{}, media.Reject(media.DecodeInvalid, "empty image")
	}
	if reservedBytes > limits.MaxBytesPerImage {
		return ArtifactPutResult{}, media.Reject(media.LimitExceeded, "image exceeds byte budget")
	}
	// Reserve both the supplied/source buffer and the possible immutable
	// collision comparison buffer before either can be allocated here.
	reservation, err := media.ReserveTransport(ctx, 2*reservedBytes+2*maxMediaMetadataBytes)
	if err != nil {
		return ArtifactPutResult{}, err
	}
	defer reservation.Release()
	data := req.Content
	if req.SourcePath != "" {
		if len(data) > 0 {
			return ArtifactPutResult{}, media.Reject(media.BindingConflict, "multiple image sources")
		}
		root, err := filepath.Abs(s.sourceDir)
		if err != nil || s.sourceDir == "" {
			return ArtifactPutResult{}, media.Reject(media.AccessDenied, "image source root unavailable")
		}
		path := req.SourcePath
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || !filepath.IsLocal(rel) {
			return ArtifactPutResult{}, media.Reject(media.AccessDenied, "image source escapes root")
		}
		data, err = media.ReadFile(ctx, path, req.MediaLimits)
		if err != nil {
			return ArtifactPutResult{}, err
		}
	}
	image, err := media.Validate(ctx, data, req.MediaLimits)
	if err != nil {
		return ArtifactPutResult{}, err
	}
	if err := media.CheckBundle([]media.Ref{image}, req.MediaLimits); err != nil {
		return ArtifactPutResult{}, err
	}
	if (req.MediaType != "" && req.MediaType != image.MediaType) || (req.ID != "" && req.ID != image.ID) {
		return ArtifactPutResult{}, media.Reject(media.IntegrityMismatch, "image identity or MIME claim conflicts")
	}
	if err := s.ensureMediaNamespace(); err != nil {
		return ArtifactPutResult{}, err
	}
	ref := imageArtifactRef(image)
	metadata, err := json.Marshal(immutableArtifactMetadata{ArtifactRef: ref, Image: &image})
	if err != nil {
		return ArtifactPutResult{}, err
	}
	createdData, err := createOrVerifyPrivateMedia(ctx, s.mediaPath("data", image.ID), data, req.MediaLimits)
	if err != nil {
		return ArtifactPutResult{}, err
	}
	createdMeta, err := createOrVerifyPrivateMedia(ctx, s.mediaPath("meta", image.ID+".json"), metadata, media.Limits{MaxBytesPerImage: maxMediaMetadataBytes})
	if err != nil {
		return ArtifactPutResult{}, err
	}
	return ArtifactPutResult{ArtifactRef: ref, CreatedData: createdData, CreatedMetadata: createdMeta}, nil
}

func (s *FileArtifactStore) ensureMediaNamespace() error {
	for _, suffix := range []string{"media", "media/data", "media/meta"} {
		path := filepath.Join(s.root, filepath.FromSlash(suffix))
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return media.Reject(media.DurabilityRequired, "cannot create private media directory")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			return media.Reject(media.AccessDenied, "unsafe private media directory")
		}
	}
	return nil
}

func (s *FileArtifactStore) mediaPath(kind, id string) string {
	return filepath.Join(s.root, "media", kind, id)
}

func createOrVerifyPrivateMedia(ctx context.Context, path string, data []byte, limits media.Limits) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := fsutil.AtomicCreateFile(path, data, 0o600); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrExist) {
		return false, media.Reject(media.DurabilityRequired, "cannot persist private media")
	}
	old, err := readPrivateMedia(ctx, path, limits)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(old, data) {
		return false, media.Reject(media.IntegrityMismatch, "immutable media collision")
	}
	return false, nil
}

func readPrivateMedia(ctx context.Context, path string, limits media.Limits) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, media.Reject(media.InputMissing, "private media is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, media.Reject(media.AccessDenied, "unsafe private media file")
	}
	if budget, bounded := ctx.Value(artifactReadBudgetKey{}).(*ArtifactReadBudget); bounded {
		budget.mu.Lock()
		defer budget.mu.Unlock()
		if budget.err != nil {
			return nil, budget.err
		}
		if info.Size() > budget.remaining {
			budget.err = ErrReadLimit
			return nil, budget.err
		}
		resolved, err := limits.Resolve()
		if err != nil {
			return nil, err
		}
		// This package's legacy min(int,int) shadows the language built-in.
		if budget.remaining < resolved.MaxBytesPerImage {
			resolved.MaxBytesPerImage = budget.remaining
		}
		if resolved.MaxBytesPerImage <= 0 {
			budget.err = ErrReadLimit
			return nil, budget.err
		}
		data, err := media.ReadFile(ctx, path, resolved)
		budget.remaining -= int64(len(data))
		if err != nil {
			budget.err = ErrReadLimit
			return nil, err
		}
		return data, nil
	}
	return media.ReadFile(ctx, path, limits)
}

const maxMediaMetadataBytes int64 = 64 << 10

func (s *FileArtifactStore) mediaMetadata(ctx context.Context, id string) (immutableArtifactMetadata, error) {
	var metadata immutableArtifactMetadata
	if !media.IsID(id) {
		return metadata, media.Reject(media.IntegrityMismatch, "invalid media ID")
	}
	for _, suffix := range []string{"media", "media/data", "media/meta"} {
		info, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(suffix)))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			return metadata, media.Reject(media.AccessDenied, "unsafe private media directory")
		}
	}
	encoded, err := readPrivateMedia(ctx, s.mediaPath("meta", id+".json"), media.Limits{MaxBytesPerImage: maxMediaMetadataBytes})
	if err != nil {
		return metadata, err
	}
	if err := json.Unmarshal(encoded, &metadata, json.RejectUnknownMembers(true)); err != nil || metadata.Image == nil {
		return metadata, media.Reject(media.IntegrityMismatch, "invalid media metadata")
	}
	if err := metadata.Image.Validate(); err != nil {
		return metadata, err
	}
	if metadata.Image.ID != id || !sameArtifactRef(metadata.ArtifactRef, imageArtifactRef(*metadata.Image)) || metadata.RetrievalURL != "" || len(metadata.ParentSourceIDs) > 0 {
		return metadata, media.Reject(media.IntegrityMismatch, "media metadata conflicts with canonical content")
	}
	return metadata, nil
}

func checkMediaArtifactClaims(supplied, canonical ArtifactRef) error {
	if (supplied.Kind != "" && supplied.Kind != canonical.Kind) || (supplied.Role != "" && supplied.Role != canonical.Role) ||
		(supplied.Path != "" && supplied.Path != canonical.Path) || (supplied.MediaType != "" && supplied.MediaType != canonical.MediaType) ||
		(supplied.SHA256 != "" && supplied.SHA256 != canonical.SHA256) || (supplied.Bytes != 0 && supplied.Bytes != canonical.Bytes) ||
		(supplied.ByteSize != 0 && supplied.ByteSize != canonical.ByteSize) || supplied.RunID != "" || supplied.TaskID != "" || supplied.Attempt != 0 ||
		supplied.Agent != "" || supplied.Provider != "" || supplied.ToolCallID != "" || supplied.Description != "" || supplied.Type != "" || !supplied.CreatedAt.IsZero() {
		return media.Reject(media.IntegrityMismatch, "media reference claim conflicts")
	}
	return nil
}

func (s *FileArtifactStore) readVerifiedMedia(ctx context.Context, id string) ([]byte, ArtifactRef, *media.Reservation, error) {
	metadata, err := s.mediaMetadata(ctx, id)
	if err != nil {
		return nil, ArtifactRef{}, nil, err
	}
	reservation, err := media.ReserveTransport(ctx, metadata.Image.ByteSize)
	if err != nil {
		return nil, ArtifactRef{}, nil, err
	}
	success := false
	defer func() {
		if !success {
			reservation.Release()
		}
	}()
	data, err := s.readReservedMediaData(ctx, *metadata.Image, media.Limits{})
	if err != nil {
		return nil, ArtifactRef{}, nil, err
	}
	success = true
	return data, metadata.ArtifactRef, reservation, nil
}

// readReservedMedia verifies the exact bytes returned to the caller. The
// caller must already own transport credit for expected.ByteSize; this helper
// never nests a storage reservation inside a whole-request reservation.
// Possession of a Ref is not an authorization: native compilers must validate
// the canonical occurrence and current lease before calling this helper.
func (s *FileArtifactStore) readReservedMedia(ctx context.Context, expected media.Ref, limits media.Limits) ([]byte, error) {
	if err := expected.Validate(); err != nil {
		return nil, err
	}
	metadata, err := s.mediaMetadata(ctx, expected.ID)
	if err != nil {
		return nil, err
	}
	if metadata.Image == nil || *metadata.Image != expected {
		return nil, media.Reject(media.IntegrityMismatch, "private image metadata differs from the reserved reference")
	}
	return s.readReservedMediaData(ctx, expected, limits)
}

// readReservedMediaData shares the reservation contract above. Its caller has
// already validated private metadata, avoiding a second metadata budget debit.
func (s *FileArtifactStore) readReservedMediaData(ctx context.Context, expected media.Ref, limits media.Limits) ([]byte, error) {
	data, err := readPrivateMedia(ctx, s.mediaPath("data", expected.ID), media.Limits{MaxBytesPerImage: expected.ByteSize})
	if err != nil {
		clear(data)
		return nil, err
	}
	actual, err := media.Validate(ctx, data, limits)
	if err != nil || actual != expected {
		clear(data)
		if err != nil {
			return nil, err
		}
		return nil, media.Reject(media.IntegrityMismatch, "private image changed")
	}
	return data, nil
}

func (s *FileArtifactStore) verifyMedia(ctx context.Context, ref ArtifactRef) error {
	_, canonical, reservation, err := s.readVerifiedMedia(ctx, ref.ID)
	if err != nil {
		return err
	}
	defer reservation.Release()
	// Manifest-local ownership is verified by the evidence binding, not CAS.
	// Keep every immutable content claim exact while allowing that occurrence.
	ref.RunID, ref.TaskID, ref.Attempt, ref.Agent = "", "", 0, ""
	if !sameArtifactRef(ref, canonical) {
		return media.Reject(media.IntegrityMismatch, "media verification requires canonical content metadata")
	}
	return nil
}

func (s *FileArtifactStore) openMedia(ctx context.Context, id string) (io.ReadCloser, error) {
	data, _, reservation, err := s.readVerifiedMedia(ctx, id)
	if err != nil {
		return nil, err
	}
	return &privateMediaReader{Reader: bytes.NewReader(data), reservation: reservation}, nil
}

type privateMediaReader struct {
	*bytes.Reader
	reservation *media.Reservation
}

func (r *privateMediaReader) Close() error {
	r.Reset(nil)
	r.reservation.Release()
	return nil
}
