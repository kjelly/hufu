package main

import (
	"archive/tar"
	"bytes"
	"encoding/json/v2"
	"fmt"
	"path/filepath"

	"github.com/kjelly/hufu/internal/team"
)

// Only dumps referenced by the selected run are eligible. Never walk the stall
// directory: it can contain unrelated runs and sensitive diagnostic material.
func addRunStallDumps(tw *tar.Writer, workspace string, rows []byte, inclusions map[string]string) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		inclusions["stall_dumps"] = fmt.Sprintf("error resolving workspace: %v", err)
		return
	}
	seen := make(map[string]bool)
	for line := range bytes.SplitSeq(rows, []byte{'\n'}) {
		var event team.RunEvent
		if json.Unmarshal(line, &event) != nil || event.Type != "run_stall_detected" {
			continue
		}
		var payload struct {
			DumpPath string `json:"dump_path"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.DumpPath == "" {
			continue
		}
		dumpPath, err := filepath.Abs(payload.DumpPath)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, dumpPath)
		if err != nil || filepath.Dir(rel) != filepath.Join("logs", "stall") {
			inclusions["stall_dump:"+event.ID] = "omitted (outside logs/stall)"
			continue
		}
		rel = filepath.ToSlash(rel)
		if seen[rel] {
			continue
		}
		seen[rel] = true
		status, err := addRedactedFileToTar(tw, filepath.Join(root, filepath.FromSlash(rel)), rel)
		if err != nil {
			status = fmt.Sprintf("error exporting: %v", err)
		}
		inclusions[rel] = status
	}
}
