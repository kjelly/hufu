package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/tools"
)

type evidenceSpan struct {
	Start       int64 `json:"start"`
	End         int64 `json:"end"`
	BundleStart int64 `json:"bundle_start"`
	BundleEnd   int64 `json:"bundle_end"`
	StartLine   int   `json:"start_line"`
	EndLine     int   `json:"end_line"`
}

type sourceEvidence struct {
	Path           string                    `json:"path"`
	Role           string                    `json:"role"`
	Status         string                    `json:"status"`
	SHA256         string                    `json:"sha256"`
	ByteSize       int64                     `json:"byte_size"`
	AvailableSpans []evidenceSpan            `json:"available_spans"`
	RequiredScopes []tools.ArtifactByteRange `json:"required_scopes"`
	Truncated      bool                      `json:"truncated"`
	Omitted        bool                      `json:"omitted"`
	OmittedBytes   int64                     `json:"omitted_bytes"`
}

type evidenceInventory struct {
	SchemaVersion int              `json:"schema_version"`
	Revision      string           `json:"review_revision"`
	BaseRevision  string           `json:"review_base_revision"`
	SnapshotID    string           `json:"snapshot_id,omitempty"`
	ContentSHA256 string           `json:"content_sha256,omitempty"`
	Sources       []sourceEvidence `json:"sources"`
}

func requiredArtifactRead(input artifact) tools.ArtifactReadCoverage {
	ranges := []tools.ArtifactByteRange{}
	if input.Bytes > 0 {
		ranges = append(ranges, tools.ArtifactByteRange{Start: 0, End: input.Bytes})
	}
	coverage := tools.ArtifactReadCoverage{ArtifactID: input.ID, SHA256: input.SHA256, ByteSize: input.Bytes, Ranges: ranges}
	return coverage
}

// Both coordinates are explicit: required_scopes address frozen source bytes;
// available_spans map those bytes to the actual assigned artifact. Neither a
// missing file nor an unlisted path is an assertion of repository absence.
func (s *reviewSourceSnapshot) bundleDataWithScopes(partition string, primaryPaths []string, scopes map[string][]tools.ArtifactByteRange, relatedBudget int) ([]byte, []byte, error) {
	if s == nil || s.root == "" {
		return nil, nil, errors.New("reviewed source snapshot is unavailable")
	}
	related, err := s.relatedGoPaths(primaryPaths)
	if err != nil {
		return nil, nil, err
	}
	inventory := evidenceInventory{SchemaVersion: 1, Revision: s.revision, BaseRevision: s.baseRevision, Sources: []sourceEvidence{}}
	var bundle strings.Builder
	fmt.Fprintf(&bundle, "# Immutable reviewed source snapshot\nreview_revision: %s\nreview_base_revision: %s\nworkset_partition: %s\n", s.revision, s.baseRevision, filepath.ToSlash(partition))
	if s.identity != nil {
		inventory.SnapshotID, inventory.ContentSHA256 = s.identity.ID, s.identity.ContentSHA256
		fmt.Fprintf(&bundle, "review_mode: repository_snapshot\nsnapshot_id: %s\ncontent_sha256: %s\n", s.identity.ID, s.identity.ContentSHA256)
	}
	bundle.WriteString("Read this artifact as the source/caller/test authority for the assigned workset. Do not substitute the live checkout.\ncoverage_scope: workset_only\nUnlisted or unavailable paths are not evidence of repository absence. Source byte ranges are half-open; source line coordinates retain their original numbering.\n")
	fmt.Fprintf(&bundle, "primary_paths: %d\nrelated_paths: %d\nrelated_content_budget_bytes: %d\n", len(primaryPaths), len(related), relatedBudget)
	seen := make(map[string]bool)
	for _, current := range primaryPaths {
		if seen[current] {
			continue
		}
		seen[current] = true
		row, _, err := s.writeEvidenceSection(&bundle, current, "primary", scopes[current], -1)
		if err != nil {
			return nil, nil, err
		}
		inventory.Sources = append(inventory.Sources, row)
	}
	remaining := relatedBudget
	for index, current := range related {
		limit := remaining
		if index >= maxSnapshotRelatedPaths {
			limit = 0
		}
		row, used, err := s.writeEvidenceSection(&bundle, current, "related", nil, limit)
		if err != nil {
			return nil, nil, err
		}
		remaining -= used
		inventory.Sources = append(inventory.Sources, row)
	}
	encoded, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	bundleBytes := []byte(bundle.String())
	encoded = append(encoded, '\n')
	return bundleBytes, encoded, nil
}

func (s *reviewSourceSnapshot) writeEvidenceSection(bundle *strings.Builder, repositoryPath, role string, requested []tools.ArtifactByteRange, limit int) (sourceEvidence, int, error) {
	row := sourceEvidence{Path: filepath.ToSlash(repositoryPath), Role: role, AvailableSpans: []evidenceSpan{}, RequiredScopes: []tools.ArtifactByteRange{}}
	relative, err := cleanSnapshotPath(repositoryPath)
	if err != nil {
		return row, 0, err
	}
	bundle.WriteString("\n## " + filepath.ToSlash(relative) + "\nrole: " + role + "\n")
	fullPath := filepath.Join(s.root, relative)
	info, err := os.Lstat(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		row.Status, row.Omitted = "unavailable_in_frozen_snapshot", true
		if s.identity != nil {
			for _, file := range s.identity.Files {
				if file.Path == row.Path {
					row.SHA256, row.ByteSize, row.OmittedBytes = file.SHA256, file.Bytes, file.Bytes
					break
				}
			}
		}
		row.RequiredScopes = append(row.RequiredScopes, requested...)
		if requested == nil && row.ByteSize > 0 {
			row.RequiredScopes = append(row.RequiredScopes, tools.ArtifactByteRange{Start: 0, End: row.ByteSize})
		}
		fmt.Fprintf(bundle, "status: %s\n", row.Status)
		return row, 0, nil
	}
	if err != nil {
		return row, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(fullPath)
		if err != nil {
			return row, 0, err
		}
		row.Status, row.SHA256, row.ByteSize, row.Omitted = "symlink", sha256Hex([]byte(target)), int64(len(target)), true
		row.OmittedBytes = row.ByteSize
		fmt.Fprintf(bundle, "status: symlink\ntarget: %s\n", filepath.ToSlash(target))
		return row, 0, nil
	}
	if !info.Mode().IsRegular() {
		row.Status, row.Omitted = "unsupported_mode", true
		fmt.Fprintf(bundle, "status: unsupported_mode\nmode: %s\n", info.Mode())
		return row, 0, nil
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return row, 0, err
	}
	row.Status, row.SHA256, row.ByteSize = "present", sha256Hex(data), int64(len(data))
	fmt.Fprintf(bundle, "status: present\nbytes: %d\nsha256: %s\n", len(data), row.SHA256)
	if requested == nil {
		requested = []tools.ArtifactByteRange{}
		if len(data) > 0 {
			requested = append(requested, tools.ArtifactByteRange{Start: 0, End: int64(len(data))})
		}
	}
	for index, span := range requested {
		if span.Start < 0 || span.End <= span.Start || span.End > int64(len(data)) || index > 0 && span.Start < requested[index-1].End {
			return row, 0, fmt.Errorf("invalid frozen source byte range for %q: %+v", repositoryPath, span)
		}
	}
	row.RequiredScopes = append(row.RequiredScopes, requested...)
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		row.Omitted, row.OmittedBytes = true, row.ByteSize
		bundle.WriteString("content: binary_omitted\n")
		return row, 0, nil
	}
	for _, span := range requested {
		if !utf8.Valid(data[span.Start:span.End]) {
			return row, 0, fmt.Errorf("source range for %q is not UTF-8 aligned", repositoryPath)
		}
	}
	used := 0
	for _, span := range requested {
		end := span.End
		if limit >= 0 && end-span.Start > int64(limit-used) {
			end = span.Start + int64(limit-used)
			for end > span.Start && !utf8.RuneStart(data[end]) {
				end--
			}
		}
		if end <= span.Start {
			continue
		}
		startLine := bytes.Count(data[:span.Start], []byte("\n")) + 1
		endLine := startLine + bytes.Count(data[span.Start:end], []byte("\n"))
		if data[end-1] == '\n' {
			endLine--
		}
		fmt.Fprintf(bundle, "source_byte_range: [%d,%d)\nsource_lines: %d-%d\n```text\n", span.Start, end, startLine, endLine)
		bundleStart := bundle.Len()
		bundle.Write(data[span.Start:end])
		row.AvailableSpans = append(row.AvailableSpans, evidenceSpan{Start: span.Start, End: end, BundleStart: int64(bundleStart), BundleEnd: int64(bundle.Len()), StartLine: startLine, EndLine: endLine})
		if data[end-1] != '\n' {
			bundle.WriteByte('\n')
		}
		bundle.WriteString("```\n")
		used += int(end - span.Start)
	}
	requestedBytes := int64(0)
	for _, span := range requested {
		requestedBytes += span.End - span.Start
	}
	row.OmittedBytes = row.ByteSize - int64(used)
	row.Truncated = int64(used) < requestedBytes
	row.Omitted = len(data) > 0 && used == 0
	if row.Truncated {
		fmt.Fprintf(bundle, "content_truncated: true\nomitted_bytes: %d\n", row.OmittedBytes)
	}
	if len(data) == 0 {
		bundle.WriteString("content: empty\n```text\n```\n")
	}
	return row, used, nil
}

type frozenSourceManifest struct {
	Revision      string         `json:"review_revision"`
	BaseRevision  string         `json:"review_base_revision"`
	SnapshotID    string         `json:"snapshot_id"`
	ContentSHA256 string         `json:"content_sha256"`
	Files         []snapshotFile `json:"files"`
}

// Persist the exact prepared bytes, including dirty/untracked worktree changes.
// The supplement action never re-archives HEAD or falls back to live files.
func (s *reviewSourceSnapshot) persistFrozenSource(artifactRoot, outputDir string) (artifact, error) {
	if s.frozenArtifact.ID != "" {
		return s.frozenArtifact, nil
	}
	root := filepath.Join(outputDir, "frozen-source", "files")
	manifest := frozenSourceManifest{Revision: s.revision, BaseRevision: s.baseRevision, Files: []snapshotFile{}}
	if s.identity != nil {
		manifest.SnapshotID, manifest.ContentSHA256 = s.identity.ID, s.identity.ContentSHA256
	}
	buffer := make([]byte, 64*1024)
	err := filepath.Walk(s.root, func(current string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(s.root, current)
		if err != nil {
			return err
		}
		destination := filepath.Join(root, relative)
		if info.IsDir() {
			return os.MkdirAll(destination, 0755)
		}
		file := snapshotFile{Path: filepath.ToSlash(relative), Disposition: "available"}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil {
				return err
			}
			file.Mode, file.SHA256, file.Bytes = "120000", sha256Hex([]byte(target)), int64(len(target))
			file.Disposition, file.Reason = "excluded", "symlink"
			// The link identity is retained but no filesystem link is published.
		} else if info.Mode().IsRegular() {
			file.Mode = "100644"
			if info.Mode().Perm()&0o111 != 0 {
				file.Mode = "100755"
			}
			source, err := os.Open(current)
			if err != nil {
				return err
			}
			target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
			if err != nil {
				_ = source.Close()
				return err
			}
			digest := sha256.New()
			last := byte('\n')
			var copyErr error
			for {
				count, readErr := source.Read(buffer)
				if count > 0 {
					data := buffer[:count]
					if _, copyErr = target.Write(data); copyErr != nil {
						break
					}
					if _, copyErr = digest.Write(data); copyErr != nil {
						break
					}
					file.Bytes += int64(count)
					file.Lines += bytes.Count(data, []byte{'\n'})
					last = data[len(data)-1]
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					copyErr = readErr
					break
				}
			}
			if file.Bytes > 0 && last != '\n' {
				file.Lines++
			}
			file.SHA256 = hex.EncodeToString(digest.Sum(nil))
			err = errors.Join(copyErr, source.Close(), target.Close())
			if err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unsupported frozen source mode: %s", info.Mode())
		}
		manifest.Files = append(manifest.Files, file)
		return nil
	})
	if err != nil {
		return artifact{}, fmt.Errorf("persist frozen source: %w", err)
	}
	if s.identity != nil {
		seen := make(map[string]bool)
		for _, file := range manifest.Files {
			seen[file.Path] = true
		}
		for _, file := range s.identity.Files {
			if !seen[file.Path] {
				manifest.Files = append(manifest.Files, file)
			}
		}
	}
	if manifest.ContentSHA256 == "" {
		identity, err := json.Marshal(manifest.Files)
		if err != nil {
			return artifact{}, err
		}
		manifest.ContentSHA256 = sha256Hex(identity)
		manifest.SnapshotID = "sha256:" + manifest.ContentSHA256
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return artifact{}, err
	}
	manifestPath := filepath.Join(outputDir, "frozen-source", "manifest.json")
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		return artifact{}, err
	}
	result, err := fileArtifact(artifactRoot, manifestPath, "review_frozen_source")
	if err != nil {
		return artifact{}, err
	}
	relative, err := filepath.Rel(artifactRoot, root)
	if err != nil {
		return artifact{}, err
	}
	s.frozenRoot = filepath.ToSlash(relative)
	s.frozenArtifact = result
	return result, nil
}
