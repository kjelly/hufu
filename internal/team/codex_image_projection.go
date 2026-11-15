package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/kjelly/hufu/internal/log"
	"github.com/kjelly/hufu/internal/media"
)

// A projection owns only copies of the current task's canonical bytes. The
// logical directory is stable across process restarts; it is never a CAS path.
type codexImageProjection struct {
	root        *os.Root
	path        string
	refs        []media.Ref
	reservation *media.Reservation
	release     func()
}

func codexImageProjectionHash(binding *media.TaskBinding, target string) (string, error) {
	if binding == nil {
		return "", media.Reject(media.BindingConflict, "Codex projection requires a canonical binding")
	}
	return media.Digest(struct {
		Binding string `json:"binding"`
		Target  string `json:"target"`
	}{binding.Hash, target})
}

func newCodexImageProjection(ctx context.Context, c *Coordinator, request AttemptRequest, hash string) (*codexImageProjection, error) {
	item := c.todoItemByID(request.TaskID)
	if item == nil || item.MediaBinding == nil {
		return nil, media.Reject(media.AccessDenied, "Codex projection has no canonical input")
	}
	expected, err := codexImageProjectionHash(item.MediaBinding, request.ExecutionTarget.String())
	if err != nil || expected != hash {
		return nil, media.Reject(media.BindingConflict, "Codex projection path is outside canonical binding")
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return nil, err
	}
	frames := make([]int64, len(item.MediaBinding.Refs))
	for index, ref := range item.MediaBinding.Refs {
		frames[index] = 2 * ref.ByteSize
	}
	// Count every occurrence, including duplicates, before loading or copying.
	reservation, err := media.ReserveTransportFrames(ctx, frames...)
	if err != nil {
		return nil, err
	}
	projection := &codexImageProjection{refs: slices.Clone(item.MediaBinding.Refs), reservation: reservation}
	success := false
	defer func() {
		if !success {
			projection.Close()
		}
	}()
	if err := c.validateCodexImageLease(ctx, request); err != nil {
		return nil, err
	}
	store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.projectDir)
	if err != nil {
		return nil, err
	}
	parentPath := filepath.Join(c.artifactStoreRootPath(), logsDir)
	parent, _, err := imageExportDirectory(filepath.Join(parentPath, "unused"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	base, err := openCodexProjectionDirectory(parent, "media-projections")
	if err != nil {
		return nil, err
	}
	defer func() {
		if projection.root == nil {
			_ = base.Close()
		}
	}()
	unlock, err := acquireCodexStateFileLock(ctx, filepath.Join(parentPath, "media-projections", codexProjectionLockFile))
	if err != nil {
		return nil, err
	}
	// Publication and quota admission finish before any app-server consumer runs.
	defer unlock()
	root, err := openCodexProjectionDirectory(base, "input-"+strings.TrimPrefix(hash, "sha256:"))
	if err != nil {
		return nil, err
	}
	projection.root = root
	directoryInfo, err := root.Stat(".")
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	projection.path = codexImageProjectionPath(c.artifactStoreRootPath(), hash)
	files := make(map[string]os.FileInfo)
	lease := &codexProjectionLease{base: base, root: root, directory: "input-" + strings.TrimPrefix(hash, "sha256:"), directoryInfo: directoryInfo,
		lockPath: filepath.Join(parentPath, "media-projections", codexProjectionLockFile), files: files, maxImages: policy.Policy.Image.MaxBranchBlobs}
	projection.release = sync.OnceFunc(func() {
		if err := lease.close(); err != nil {
			log.Printf("Codex projection cleanup retained copies: %v\n", err)
		}
		reservation.Release()
	})
	lease.consumer, err = newCodexProjectionConsumer(ctx, root, policy.Policy.Image.MaxBranchBlobs)
	if err != nil {
		return nil, err
	}

	if err := checkCodexProjectionCopies(ctx, base, root, projection.refs, policy.Policy.Image); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, ref := range projection.refs {
		if seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true
		if err := c.validateCodexImageLease(ctx, request); err != nil {
			return nil, err
		}
		// A surviving projection must match before any repair/replacement occurs.
		if _, err := root.Lstat(codexImageFilename(ref)); err == nil {
			if err := projection.verifyFile(ctx, ref, policy.Policy.Image); err != nil {
				return nil, err
			}
			files[codexImageFilename(ref)], err = root.Lstat(codexImageFilename(ref))
			if err != nil {
				return nil, err
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		data, err := store.readReservedMedia(ctx, ref, policy.Policy.Image)
		if err != nil {
			return nil, err
		}
		file, err := root.OpenFile(codexImageFilename(ref), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
		if err == nil {
			info, statErr := file.Stat()
			err = statErr
			if err == nil {
				files[codexImageFilename(ref)] = info
				_, err = file.Write(data)
			}
			if err == nil {
				err = file.Sync()
			}
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
		clear(data)
		if err != nil {
			return nil, err
		}
	}
	if err := projection.Verify(ctx, policy.Policy.Image); err != nil {
		return nil, err
	}
	success = true
	return projection, nil
}

func openCodexProjectionDirectory(parent *os.Root, name string) (*os.Root, error) {
	if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := parent.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, media.Reject(media.AccessDenied, "unsafe Codex image projection directory")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, media.Reject(media.AccessDenied, "Codex projection directory changed")
	}
	return root, nil
}

func (p *codexImageProjection) verifyFile(ctx context.Context, ref media.Ref, limits media.Limits) error {
	info, err := p.root.Lstat(codexImageFilename(ref))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 || info.Size() != ref.ByteSize {
		return media.Reject(media.IntegrityMismatch, "Codex projection is missing or unsafe")
	}
	data, err := media.ReadFile(ctx, filepath.Join(p.path, codexImageFilename(ref)), limits)
	if err != nil {
		return err
	}
	defer clear(data)
	actual, err := media.Validate(ctx, data, limits)
	if err != nil || actual != ref {
		return media.Reject(media.IntegrityMismatch, "Codex projection differs from canonical image")
	}
	return nil
}

func (p *codexImageProjection) Verify(ctx context.Context, limits media.Limits) error {
	for _, ref := range p.refs {
		if err := p.verifyFile(ctx, ref, limits); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func codexImageProjectionPath(workspace, hash string) string {
	return filepath.Join(workspace, logsDir, "media-projections", "input-"+strings.TrimPrefix(hash, "sha256:"))
}

func codexImageInputs(path, prompt string, refs []media.Ref) []codexUserInput {
	input := []codexUserInput{{Type: "text", Text: prompt}}
	for _, ref := range refs {
		input = append(input, codexUserInput{Type: "localImage", Path: filepath.Join(path, codexImageFilename(ref))})
	}
	return input
}

func (p *codexImageProjection) Inputs(prompt string) []codexUserInput {
	return codexImageInputs(p.path, prompt, p.refs)
}

func (p *codexImageProjection) Close() {
	if p == nil {
		return
	}
	if p.release != nil {
		p.release()
		return
	}
	if p.root != nil {
		_ = p.root.Close()
	}
	p.reservation.Release()
}

func codexImageFilename(ref media.Ref) string {
	extension := ".png"
	switch ref.MediaType {
	case "image/jpeg":
		extension = ".jpg"
	case "image/webp":
		extension = ".webp"
	}
	return ref.ID + extension
}
