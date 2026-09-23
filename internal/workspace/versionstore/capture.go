package versionstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// CaptureRequest describes one capture of a subject root.
type CaptureRequest struct {
	WorkspaceID string
	BranchID    string
	Root        string

	// ExcludeSubtrees are root-relative subtrees that are unmanaged (the
	// control root when it lies inside the subject root).
	ExcludeSubtrees []string

	// Parent is the published node this snapshot descends from (usually the
	// branch head); empty for a baseline.
	Parent SnapshotID
	Reason SnapshotReason

	RunID         string
	TaskID        string
	Attempt       int
	AnchorEventID string

	Limits CaptureLimits

	// RequireHufuignore fails a non-Git capture without .hufuignore
	// (required mode, D7).
	RequireHufuignore bool
}

const (
	maxFileCaptureAttempts = 3
	maxCaptureAttempts     = 3
	// statCacheSettleWindow keeps files modified shortly before a capture out
	// of the stat cache, so coarse timestamps cannot hide a later edit.
	statCacheSettleWindow = 2 * time.Second
)

var errCaptureRaced = errors.New("workspace changed during capture")

// leaf is one captured regular file or symlink.
type leaf struct {
	path     string
	entry    TreeEntry
	fp       fingerprint
	escaping bool
}

type captureResult struct {
	leaves       []leaf
	rootTreeHash string
	stats        CaptureStats
}

type capturePlan struct {
	request   CaptureRequest
	root      string
	subjectID string
	policy    inclusionPolicy
	limits    CaptureLimits
	started   time.Time
	statCache map[string]statCacheRow
	// dryRun hashes without writing CAS objects (LiveRootTree).
	dryRun bool
}

// CanonicalRoot resolves root to the absolute, symlink-free directory path
// used for identity and containment checks.
func CanonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root %q is not a directory", root)
	}
	return resolved, nil
}

// SubjectKey is the stable identity of a canonical subject root.
func SubjectKey(canonicalRoot string) string {
	sum := sha256.Sum256([]byte(canonicalRoot))
	return hex.EncodeToString(sum[:])
}

// Capture records the current managed state of req.Root as a new pending
// snapshot. It never advances a branch head: publication requires a durable
// commit event (PublishSnapshot). Callers hold the project lock.
func (s *Store) Capture(ctx context.Context, req CaptureRequest) (Snapshot, CaptureStats, error) {
	snapshot, _, stats, err := s.capture(ctx, req, "")
	return snapshot, stats, err
}

// CaptureIfChanged captures req.Root but records a pending snapshot only when
// the root tree differs from unchangedRoot (typically the branch head's
// tree). Otherwise changed is false and no row is written; the stat cache is
// still refreshed. Drift checks use it so that "no drift" leaves no orphans.
func (s *Store) CaptureIfChanged(ctx context.Context, req CaptureRequest, unchangedRoot string) (Snapshot, bool, CaptureStats, error) {
	return s.capture(ctx, req, unchangedRoot)
}

func (s *Store) capture(ctx context.Context, req CaptureRequest, unchangedRoot string) (Snapshot, bool, CaptureStats, error) {
	if err := s.requireWritable(); err != nil {
		return Snapshot{}, false, CaptureStats{}, err
	}
	plan, err := s.planCapture(ctx, req)
	if err != nil {
		return Snapshot{}, false, CaptureStats{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var result captureResult
	for attempt := 1; ; attempt++ {
		result, err = s.captureOnce(ctx, plan)
		if err == nil {
			break
		}
		if !errors.Is(err, errCaptureRaced) {
			return Snapshot{}, false, CaptureStats{}, err
		}
		if attempt == maxCaptureAttempts {
			return Snapshot{}, false, CaptureStats{}, fmt.Errorf("%w: %v", ErrWorkspaceUnstable, err)
		}
	}
	if unchangedRoot != "" && result.rootTreeHash == unchangedRoot {
		err = s.withTx(ctx, func(tx *sql.Tx) error {
			return replaceStatCache(ctx, tx, plan.subjectID, plan.started, result.leaves)
		})
		result.stats.Duration = s.now().Sub(plan.started)
		return Snapshot{}, false, result.stats, err
	}
	snapshot, err := s.recordCapture(ctx, plan, result)
	if err != nil {
		return Snapshot{}, false, CaptureStats{}, err
	}
	result.stats.Duration = s.now().Sub(plan.started)
	return snapshot, true, result.stats, nil
}

func (s *Store) planCapture(ctx context.Context, req CaptureRequest) (capturePlan, error) {
	if req.WorkspaceID == "" || req.BranchID == "" {
		return capturePlan{}, fmt.Errorf("capture: workspace and branch are required")
	}
	if !req.Reason.Valid() {
		return capturePlan{}, fmt.Errorf("capture: invalid reason %q", req.Reason)
	}
	root, err := CanonicalRoot(req.Root)
	if err != nil {
		return capturePlan{}, fmt.Errorf("capture: %w", err)
	}
	subtrees, err := normalizeSubtrees(req.ExcludeSubtrees)
	if err != nil {
		return capturePlan{}, fmt.Errorf("capture: %w", err)
	}
	if err = requirePublishedParent(ctx, s.db, req.Parent); err != nil {
		return capturePlan{}, fmt.Errorf("capture: %w", err)
	}
	plan := capturePlan{
		request:   req,
		root:      root,
		subjectID: SubjectKey(root),
		policy:    inclusionPolicy{excludeSubtrees: subtrees, requireHufuignore: req.RequireHufuignore},
		limits:    req.Limits.WithDefaults(),
		started:   s.now(),
	}
	plan.statCache, err = s.loadStatCache(ctx, plan.subjectID)
	if err != nil {
		return capturePlan{}, err
	}
	return plan, nil
}

// captureOnce performs one full pass. errCaptureRaced asks for a retry.
func (s *Store) captureOnce(ctx context.Context, plan capturePlan) (captureResult, error) {
	candidates, err := discoverCandidates(ctx, plan.root, plan.policy)
	if err != nil {
		return captureResult{}, err
	}
	root, err := os.OpenRoot(plan.root)
	if err != nil {
		return captureResult{}, fmt.Errorf("open workspace root: %w", err)
	}
	defer func() { _ = root.Close() }()

	run := &captureRun{store: s, plan: plan, root: root}
	for _, rel := range candidates.paths {
		captured, skip, leafErr := run.captureLeaf(ctx, rel)
		if leafErr != nil {
			return captureResult{}, leafErr
		}
		if skip {
			continue
		}
		run.leaves = append(run.leaves, captured)
		if len(run.leaves) > plan.limits.MaxFiles {
			return captureResult{}, fmt.Errorf("%w: more than %d files", ErrCaptureLimitExceeded, plan.limits.MaxFiles)
		}
	}
	rootHash, err := run.buildTrees(ctx)
	if err != nil {
		return captureResult{}, err
	}
	if err = run.revalidate(); err != nil {
		return captureResult{}, err
	}
	return captureResult{leaves: run.leaves, rootTreeHash: rootHash, stats: run.stats}, nil
}

type captureRun struct {
	store        *Store
	plan         capturePlan
	root         *os.Root
	leaves       []leaf
	logicalBytes int64
	stats        CaptureStats
}

// captureLeaf classifies and stores one candidate, retrying a file that
// changes while it is read.
func (r *captureRun) captureLeaf(ctx context.Context, rel string) (leaf, bool, error) {
	if !utf8.ValidString(rel) {
		return leaf{}, false, fmt.Errorf("%w: %q", ErrUnsupportedPathName, rel)
	}
	for _, segment := range strings.Split(rel, "/") {
		if err := validateEntryName(segment); err != nil {
			return leaf{}, false, fmt.Errorf("capture %q: %w", rel, err)
		}
	}
	osRel := filepath.FromSlash(rel)
	for attempt := 0; attempt < maxFileCaptureAttempts; attempt++ {
		info, err := r.root.Lstat(osRel)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Deleted after discovery (or tracked but already deleted).
				return leaf{}, true, nil
			}
			return leaf{}, false, fmt.Errorf("stat %q: %w", rel, err)
		}
		var captured leaf
		switch mode := info.Mode(); {
		case mode.IsRegular():
			captured, err = r.captureRegular(ctx, rel, osRel, info)
		case mode&os.ModeSymlink != 0:
			captured, err = r.captureSymlink(ctx, rel, osRel, info)
		case mode.IsDir():
			// A Git candidate that has become a directory; its contents are
			// listed as separate candidates.
			return leaf{}, true, nil
		default:
			return leaf{}, false, fmt.Errorf("%w: %q is %s", ErrUnsupportedFileType, rel, mode.Type())
		}
		if errors.Is(err, errCaptureRaced) {
			continue
		}
		return captured, false, err
	}
	return leaf{}, false, fmt.Errorf("%w: %q kept changing", ErrWorkspaceUnstable, rel)
}

func (r *captureRun) captureRegular(ctx context.Context, rel, osRel string, info os.FileInfo) (leaf, error) {
	limits := r.plan.limits
	if limits.MaxFileBytes > 0 && info.Size() > limits.MaxFileBytes {
		return leaf{}, fmt.Errorf("%w: %q is %d bytes (limit %d)", ErrCaptureLimitExceeded, rel, info.Size(), limits.MaxFileBytes)
	}
	before := fingerprintOf(info)
	entry := TreeEntry{Name: path.Base(rel), Kind: EntryBlob, Mode: uint32(info.Mode().Perm()), Size: info.Size()}
	if cached, ok := r.plan.statCache[rel]; ok && cached.matches(before) {
		exists, err := r.store.hasObject(ObjectBlob, cached.blobHash)
		if err != nil {
			return leaf{}, err
		}
		if exists {
			if r.store.statCacheHits != nil {
				r.store.statCacheHits.Add(1)
			}
			entry.ObjectHash = cached.blobHash
			r.stats.ReusedCASBytes += info.Size()
			return r.accountBlob(rel, entry, before)
		}
	}
	file, err := openRegularNoFollow(r.root, osRel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return leaf{}, errCaptureRaced
		}
		return leaf{}, fmt.Errorf("open %q: %w", rel, err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return leaf{}, fmt.Errorf("stat %q: %w", rel, err)
	}
	if !opened.Mode().IsRegular() || fingerprintOf(opened) != before {
		return leaf{}, errCaptureRaced
	}
	written, err := r.putBlob(ctx, file, limits.MaxFileBytes)
	if err != nil {
		return leaf{}, fmt.Errorf("store %q: %w", rel, err)
	}
	after, err := r.root.Lstat(osRel)
	if err != nil || fingerprintOf(after) != before || written.Size != before.size {
		return leaf{}, errCaptureRaced
	}
	if written.New {
		r.stats.NewCASBytes += written.Size
	} else {
		r.stats.ReusedCASBytes += written.Size
	}
	entry.ObjectHash = written.Hash
	return r.accountBlob(rel, entry, before)
}

func (r *captureRun) accountBlob(rel string, entry TreeEntry, fp fingerprint) (leaf, error) {
	r.logicalBytes += entry.Size
	if limit := r.plan.limits.MaxLogicalBytes; limit > 0 && r.logicalBytes > limit {
		return leaf{}, fmt.Errorf("%w: more than %d logical bytes", ErrCaptureLimitExceeded, limit)
	}
	return leaf{path: rel, entry: entry, fp: fp}, nil
}

func (r *captureRun) captureSymlink(ctx context.Context, rel, osRel string, info os.FileInfo) (leaf, error) {
	before := fingerprintOf(info)
	target, err := r.root.Readlink(osRel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return leaf{}, errCaptureRaced
		}
		return leaf{}, fmt.Errorf("readlink %q: %w", rel, err)
	}
	written, err := r.putSmall(ctx, ObjectLink, []byte(target))
	if err != nil {
		return leaf{}, fmt.Errorf("store symlink %q: %w", rel, err)
	}
	after, err := r.root.Lstat(osRel)
	if err != nil || fingerprintOf(after) != before {
		return leaf{}, errCaptureRaced
	}
	if written.New {
		r.stats.NewCASBytes += written.Size
	}
	return leaf{
		path:     rel,
		entry:    TreeEntry{Name: path.Base(rel), Kind: EntrySymlink, ObjectHash: written.Hash, Size: int64(len(target))},
		fp:       before,
		escaping: symlinkEscapes(r.plan.root, rel, target),
	}, nil
}

// symlinkEscapes reports whether the symlink at rel (inside root) resolves
// outside root. A dangling link is judged by its lexical target.
func symlinkEscapes(root, rel, target string) bool {
	resolved := target
	if !filepath.IsAbs(target) {
		resolved = filepath.Join(root, filepath.Dir(filepath.FromSlash(rel)), target)
	}
	if evaluated, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = evaluated
	}
	resolved = filepath.Clean(resolved)
	return resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator))
}

// revalidate re-checks every captured entry once the trees are built; any
// metadata change restarts the whole capture.
func (r *captureRun) revalidate() error {
	for _, captured := range r.leaves {
		info, err := r.root.Lstat(filepath.FromSlash(captured.path))
		if err != nil || fingerprintOf(info) != captured.fp {
			return fmt.Errorf("%w: %q", errCaptureRaced, captured.path)
		}
	}
	return nil
}

type treeNode struct {
	children map[string]*treeNode
	entry    *TreeEntry
}

// buildTrees publishes the Merkle trees bottom-up and returns the root hash.
func (r *captureRun) buildTrees(ctx context.Context) (string, error) {
	root := &treeNode{children: map[string]*treeNode{}}
	for index := range r.leaves {
		if err := root.insert(strings.Split(r.leaves[index].path, "/"), &r.leaves[index].entry); err != nil {
			return "", err
		}
	}
	return r.storeNode(ctx, root)
}

func (n *treeNode) insert(segments []string, entry *TreeEntry) error {
	node := n
	for index, segment := range segments {
		child := node.children[segment]
		last := index == len(segments)-1
		switch {
		case child == nil && last:
			node.children[segment] = &treeNode{entry: entry}
			return nil
		case child == nil:
			child = &treeNode{children: map[string]*treeNode{}}
			node.children[segment] = child
		case last || child.entry != nil:
			// A path is both a file and a directory: it changed mid-capture.
			return fmt.Errorf("%w: %q", errCaptureRaced, strings.Join(segments[:index+1], "/"))
		}
		node = child
	}
	return nil
}

func (r *captureRun) storeNode(ctx context.Context, node *treeNode) (string, error) {
	names := make([]string, 0, len(node.children))
	for name := range node.children {
		names = append(names, name)
	}
	sort.Strings(names)
	tree := TreeObject{Entries: make([]TreeEntry, 0, len(names))}
	for _, name := range names {
		child := node.children[name]
		if child.entry != nil {
			tree.Entries = append(tree.Entries, *child.entry)
			continue
		}
		hash, err := r.storeNode(ctx, child)
		if err != nil {
			return "", err
		}
		tree.Entries = append(tree.Entries, TreeEntry{Name: name, Kind: EntryTree, ObjectHash: hash})
	}
	written, err := r.putTreeObject(ctx, tree)
	if err != nil {
		return "", err
	}
	if written.New {
		r.stats.NewCASBytes += written.Size
	}
	return written.Hash, nil
}

// manifestDigest identifies the flattened leaf manifest.
func manifestDigest(leaves []leaf) string {
	hasher := sha256.New()
	for _, captured := range leaves {
		_, _ = fmt.Fprintf(hasher, "%s\x00%s\x00%s\x00%d\x00%o\n", captured.path, captured.entry.Kind, captured.entry.ObjectHash, captured.entry.Size, captured.entry.Mode)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func (s *Store) recordCapture(ctx context.Context, plan capturePlan, result captureResult) (Snapshot, error) {
	id, err := s.newSnapshotID()
	if err != nil {
		return Snapshot{}, err
	}
	req := plan.request
	snapshot := Snapshot{
		ID: id, WorkspaceID: req.WorkspaceID, BranchID: req.BranchID, Parent: req.Parent,
		RootTreeHash: result.rootTreeHash, ManifestDigest: manifestDigest(result.leaves),
		FileCount: len(result.leaves), Materializable: true, Reason: req.Reason,
		RunID: req.RunID, TaskID: req.TaskID, Attempt: req.Attempt, AnchorEventID: req.AnchorEventID,
		State: StatePending, CreatedAt: s.now(),
	}
	for _, captured := range result.leaves {
		if captured.entry.Kind == EntryBlob {
			snapshot.LogicalBytes += captured.entry.Size
		}
		if captured.escaping {
			snapshot.Materializable = false
		}
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if insertErr := insertSnapshot(ctx, tx, snapshot); insertErr != nil {
			return insertErr
		}
		return replaceStatCache(ctx, tx, plan.subjectID, plan.started, result.leaves)
	})
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// putBlob, putSmall, and putTreeObject write to the CAS, or only hash in a
// dry run.
func (r *captureRun) putBlob(ctx context.Context, content io.Reader, maxBytes int64) (casWrite, error) {
	if r.plan.dryRun {
		return hashOnly(content, maxBytes)
	}
	return r.store.putObject(ctx, ObjectBlob, content, "", maxBytes)
}

func (r *captureRun) putSmall(ctx context.Context, kind ObjectKind, data []byte) (casWrite, error) {
	if r.plan.dryRun {
		return hashOnly(bytes.NewReader(data), 0)
	}
	return r.store.putBytes(ctx, kind, data)
}

func (r *captureRun) putTreeObject(ctx context.Context, tree TreeObject) (casWrite, error) {
	if r.plan.dryRun {
		data, hash, err := EncodeTree(tree)
		return casWrite{Hash: hash, Size: int64(len(data))}, err
	}
	return r.store.putTree(ctx, tree)
}

func hashOnly(content io.Reader, maxBytes int64) (casWrite, error) {
	hasher := sha256.New()
	counter := &limitedHashWriter{dst: hasher, limit: maxBytes}
	if _, err := io.Copy(counter, content); err != nil {
		return casWrite{}, err
	}
	return casWrite{Hash: hex.EncodeToString(hasher.Sum(nil)), Size: counter.n}, nil
}

// LiveRootTree computes the root tree hash req.Root would get from a
// capture, without writing CAS objects, rows, or the stat cache. It works on
// a read-only store and is how status and doctor detect live drift.
func (s *Store) LiveRootTree(ctx context.Context, req CaptureRequest) (string, error) {
	if req.Reason == "" {
		req.Reason = SnapshotManual
	}
	plan, err := s.planCapture(ctx, req)
	if err != nil {
		return "", err
	}
	plan.dryRun = true
	for attempt := 1; ; attempt++ {
		result, captureErr := s.captureOnce(ctx, plan)
		if captureErr == nil {
			return result.rootTreeHash, nil
		}
		if !errors.Is(captureErr, errCaptureRaced) {
			return "", captureErr
		}
		if attempt == maxCaptureAttempts {
			return "", fmt.Errorf("%w: %v", ErrWorkspaceUnstable, captureErr)
		}
	}
}
