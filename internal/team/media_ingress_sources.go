package team

import (
	"context"
	"path/filepath"

	"github.com/kjelly/hufu/internal/media"
)

func (c *Coordinator) admitMediaTurnImage(ctx context.Context, request MediaTurnRequest, index int, limits media.Limits) (media.Ref, error) {
	if len(request.StagedImages) > 0 {
		var actual media.Ref
		err := request.StagedImages[index].WithBytes(ctx, func(data []byte, expected media.Ref) error {
			var err error
			actual, err = c.publishMediaTurnImage(ctx, c.artifactStoreRootPath(), PutArtifactRequest{
				Kind: "image", Content: data, ID: expected.ID, MediaType: expected.MediaType, MediaLimits: limits,
			})
			if err == nil && actual != expected {
				return media.Reject(media.IntegrityMismatch, "staged image differs from its admitted content")
			}
			return err
		})
		return actual, err
	}
	path := request.Images[index]
	absolute, err := filepath.Abs(path)
	if err != nil || path == "" {
		return media.Ref{}, media.Reject(media.InputMissing, "image file is unavailable")
	}
	// Explicit file ingress retains the same narrow source root and no-symlink
	// reader. Staged ingress has no source path and uses the same private CAS.
	return c.publishMediaTurnImage(ctx, filepath.Dir(absolute), PutArtifactRequest{Kind: "image", SourcePath: absolute, MediaLimits: limits})
}

func (c *Coordinator) publishMediaTurnImage(ctx context.Context, sourceRoot string, request PutArtifactRequest) (media.Ref, error) {
	store, err := NewFileArtifactStore(c.artifactStoreRootPath(), sourceRoot)
	if err != nil {
		return media.Ref{}, media.Reject(media.DurabilityRequired, "private artifact store unavailable")
	}
	put, err := store.Put(ctx, request)
	if err != nil {
		return media.Ref{}, err
	}
	metadata, err := store.mediaMetadata(ctx, put.ID)
	if err != nil {
		return media.Ref{}, err
	}
	if metadata.Image == nil {
		return media.Ref{}, media.Reject(media.IntegrityMismatch, "image publication has no immutable metadata")
	}
	return *metadata.Image, nil
}
