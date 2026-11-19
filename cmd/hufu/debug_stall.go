package main

import (
	"archive/tar"
	"bytes"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

const debugStallDumpMaxBytes = 8 << 20

// Only dumps referenced by the selected run are eligible. Never walk the stall
// directory: it can contain unrelated runs and sensitive diagnostic material.
func addRunStallDumps(tw *tar.Writer, workspace, runID string, rows []byte, inclusions map[string]string) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		inclusions["stall_dumps"] = fmt.Sprintf("error resolving workspace: %v", err)
		return
	}
	seen := make(map[string]bool)
	for line := range bytes.SplitSeq(rows, []byte{'\n'}) {
		var event team.RunEvent
		if json.Unmarshal(line, &event) != nil || event.RunID != runID || event.Type != "run_stall_detected" {
			continue
		}
		key := "stall_dump:" + event.ID
		var payload struct {
			DumpPath string `json:"dump_path"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.DumpPath == "" {
			inclusions[key] = "omitted (missing dump path)"
			continue
		}
		dumpPath := payload.DumpPath
		if !filepath.IsAbs(dumpPath) {
			dumpPath = filepath.Join(root, dumpPath)
		}
		rel, err := filepath.Rel(root, dumpPath)
		if err != nil || filepath.Dir(rel) != filepath.Join("logs", "stall") {
			inclusions[key] = "omitted (outside logs/stall)"
			continue
		}
		name := filepath.Base(rel)
		if !strings.HasPrefix(name, "stall-") || !strings.HasSuffix(name, ".txt") {
			inclusions[key] = "omitted (unexpected stall dump filename)"
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		archivePath := filepath.ToSlash(rel)
		status, err := addRedactedFileToTarWithLimit(tw, filepath.Join(root, rel), archivePath, debugStallDumpMaxBytes)
		if err != nil {
			status = fmt.Sprintf("error exporting: %v", err)
		}
		inclusions[archivePath] = status
	}
}
