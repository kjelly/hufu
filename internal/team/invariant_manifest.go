package team

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/utils"
)

const (
	invariantManifestMaxBytes      = 256 * 1024
	invariantManifestMaxEntries    = 128
	invariantStatementMaxRunes     = 2048
	invariantStatementsMaxBytes    = 64 * 1024
	invariantAppliesToMaxEntries   = 32
	invariantAppliesToMaxPathBytes = 256
)

var invariantIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func loadInvariantCatalog(teamDir string) ([]InvariantDefinition, error) {
	manifestPath := filepath.Join(teamDir, "invariants.yaml")
	pathInfo, err := os.Lstat(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect invariant manifest: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("invariant manifest %q must be a regular file and not a symlink", manifestPath)
	}
	if pathInfo.Size() > invariantManifestMaxBytes {
		return nil, fmt.Errorf("invariant manifest %q exceeds %d bytes", manifestPath, invariantManifestMaxBytes)
	}

	file, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("open invariant manifest: %w", err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat open invariant manifest: %w", err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return nil, fmt.Errorf("invariant manifest %q changed while opening", manifestPath)
	}
	data, err := io.ReadAll(io.LimitReader(file, invariantManifestMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read invariant manifest: %w", err)
	}
	if len(data) > invariantManifestMaxBytes {
		return nil, fmt.Errorf("invariant manifest %q exceeds %d bytes", manifestPath, invariantManifestMaxBytes)
	}

	manifest, err := decodeInvariantManifest(data)
	if err != nil {
		return nil, fmt.Errorf("parse invariant manifest %q: %w", manifestPath, err)
	}
	return normalizeInvariantManifest(manifest)
}

func decodeInvariantManifest(data []byte) (InvariantManifest, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var manifest InvariantManifest
	if err := decoder.Decode(&manifest); err != nil {
		return InvariantManifest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return InvariantManifest{}, errors.New("multiple YAML documents are not allowed")
		}
		return InvariantManifest{}, fmt.Errorf("decode trailing YAML document: %w", err)
	}
	return manifest, nil
}

func normalizeInvariantManifest(manifest InvariantManifest) ([]InvariantDefinition, error) {
	if manifest.SchemaVersion != InvariantManifestSchemaVersion {
		return nil, fmt.Errorf("schema-version must be %d", InvariantManifestSchemaVersion)
	}
	if len(manifest.Invariants) == 0 {
		return nil, errors.New("invariants must contain at least one entry")
	}
	if len(manifest.Invariants) > invariantManifestMaxEntries {
		return nil, fmt.Errorf("invariants must contain at most %d entries", invariantManifestMaxEntries)
	}

	seenIDs := make(map[string]struct{}, len(manifest.Invariants))
	totalStatementBytes := 0
	for index := range manifest.Invariants {
		definition := &manifest.Invariants[index]
		definition.ID = strings.TrimSpace(definition.ID)
		if !invariantIDPattern.MatchString(definition.ID) {
			return nil, fmt.Errorf("invariants[%d].id %q must match %s", index, definition.ID, invariantIDPattern)
		}
		if _, duplicate := seenIDs[definition.ID]; duplicate {
			return nil, fmt.Errorf("invariants[%d].id %q is duplicated", index, definition.ID)
		}
		seenIDs[definition.ID] = struct{}{}

		definition.Statement = strings.TrimSpace(strings.ReplaceAll(definition.Statement, "\r\n", "\n"))
		definition.Statement = strings.TrimSpace(utils.RedactSecrets(definition.Statement))
		statementRunes := utf8.RuneCountInString(definition.Statement)
		if statementRunes == 0 || statementRunes > invariantStatementMaxRunes {
			return nil, fmt.Errorf("invariants[%d].statement must contain 1-%d runes", index, invariantStatementMaxRunes)
		}
		totalStatementBytes += len(definition.Statement)
		if totalStatementBytes > invariantStatementsMaxBytes {
			return nil, fmt.Errorf("normalized invariant statements exceed %d bytes", invariantStatementsMaxBytes)
		}

		switch definition.Severity {
		case InvariantSeverityError, InvariantSeverityWarning, InvariantSeverityInfo:
		default:
			return nil, fmt.Errorf("invariants[%d].severity must be error, warning, or info", index)
		}
		if len(definition.AppliesTo) == 0 || len(definition.AppliesTo) > invariantAppliesToMaxEntries {
			return nil, fmt.Errorf("invariants[%d].applies-to must contain 1-%d entries", index, invariantAppliesToMaxEntries)
		}
		normalizedPaths := make(map[string]struct{}, len(definition.AppliesTo))
		for pathIndex, rawPath := range definition.AppliesTo {
			normalizedPath, err := normalizeInvariantPath(rawPath)
			if err != nil {
				return nil, fmt.Errorf("invariants[%d].applies-to[%d]: %w", index, pathIndex, err)
			}
			normalizedPaths[normalizedPath] = struct{}{}
		}
		definition.AppliesTo = slices.Sorted(maps.Keys(normalizedPaths))
	}

	slices.SortFunc(manifest.Invariants, func(left, right InvariantDefinition) int {
		return strings.Compare(left.ID, right.ID)
	})
	return cloneInvariantCatalog(manifest.Invariants), nil
}

func normalizeInvariantPath(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "*" {
		return value, nil
	}
	if value == "" {
		return "", errors.New("path must not be empty")
	}
	if len(value) > invariantAppliesToMaxPathBytes {
		return "", fmt.Errorf("path exceeds %d bytes", invariantAppliesToMaxPathBytes)
	}
	if strings.ContainsRune(value, 0) || strings.Contains(value, `\`) {
		return "", errors.New("path must use repository-relative slash-separated syntax")
	}
	if path.IsAbs(value) || strings.ContainsAny(value, "*?[]{}") {
		return "", errors.New("path must be an exact repository-relative path or a directory prefix ending in /")
	}
	directoryPrefix := strings.HasSuffix(value, "/")
	core := strings.TrimSuffix(value, "/")
	if core == "" || path.Clean(core) != core {
		return "", errors.New("path must not contain empty, dot, or parent segments")
	}
	for segment := range strings.SplitSeq(core, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("path must not contain empty, dot, or parent segments")
		}
	}
	if directoryPrefix {
		return core + "/", nil
	}
	return core, nil
}
