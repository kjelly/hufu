package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
	"github.com/spf13/cobra"
)

var debugCmd = &cobra.Command{
	Use:   "debug <run-id-or-workspace>",
	Short: "Export a debug bundle for observability and troubleshooting",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]
		var workspacePath string
		var runID string

		var isDir bool

		// First, check if target is a direct directory path
		if info, err := os.Lstat(target); err == nil && info.IsDir() {
			isDir = true
			workspacePath = target
			runID = filepath.Base(target)
		} else {
			// Otherwise treat target as a run ID in the default workspace
			workspacePath = getWorkspace()
			if workspacePath == "" {
				return fmt.Errorf("managed workspace not found; run a team first or pass --workspace")
			}
			runID = target

			// Verify run-id exists
			if err := verifyRunID(cmd.Context(), workspacePath, runID); err != nil {
				return fmt.Errorf("failed to verify run ID %q: %w", runID, err)
			}
		}

		bundleName := fmt.Sprintf("hufu-debug-%s.tar.gz", strings.ReplaceAll(runID, "/", "-"))
		// Create the bundle atomically with O_EXCL so the existence check and
		// creation are a single operation. O_EXCL|O_CREAT fails with EEXIST when
		// the path already exists as a regular file OR as a symlink (even a
		// dangling one), so a pre-existing or concurrently swapped symlink can
		// never redirect the write to an external target.
		f, err := os.OpenFile(bundleName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("refusing to overwrite existing bundle %s", bundleName)
			}
			return err
		}

		gw := gzip.NewWriter(f)
		tw := tar.NewWriter(gw)

		var finalErr error
		defer func() {
			_ = tw.Close()
			_ = gw.Close()
			_ = f.Close()
		}()

		fmt.Printf("Creating debug bundle %s from workspace %s for run %s...\n", bundleName, workspacePath, runID)
		manifest := make(map[string]interface{})
		manifest["run_id"] = runID
		manifest["workspace"] = workspacePath
		inclusions := make(map[string]string)

		if isDir {
			// Directory exports are intentionally scoped to durable runtime data.
			// A workspace also contains session state, project files, and credentials;
			// exporting all of it would turn a troubleshooting command into a secret
			// exfiltration path.
			for _, root := range []string{"logs", "runtime"} {
				rootPath := filepath.Join(workspacePath, root)
				err := filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if info.IsDir() {
						return nil
					}
					rel, relErr := filepath.Rel(workspacePath, path)
					if relErr != nil {
						return relErr
					}
					rel = filepath.ToSlash(rel)
					if isDebugSensitivePath(rel) {
						inclusions[rel] = "omitted (sensitive)"
						return nil
					}
					if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
						inclusions[rel] = "omitted (unsupported file type)"
						return nil
					}
					status, writeErr := addRedactedFileToTar(tw, path, rel)
					if writeErr != nil {
						inclusions[rel] = fmt.Sprintf("error exporting: %v", writeErr)
					} else {
						inclusions[rel] = status
					}
					return nil
				})
				if err != nil && finalErr == nil && !os.IsNotExist(err) {
					finalErr = err
				}
			}
			// The event and task journals live in runtime.sqlite, which is
			// never copied raw; the bundle carries their JSONL exports under
			// the historical names.
			for _, journal := range []struct {
				name   string
				export func(context.Context, string, io.Writer) error
			}{
				{name: debugEventLogName, export: team.ExportEventLog},
				{name: "logs/task_journal.jsonl", export: team.ExportTaskJournal},
			} {
				var exported bytes.Buffer
				if err := journal.export(cmd.Context(), workspacePath, &exported); err != nil {
					inclusions[journal.name] = fmt.Sprintf("error exporting: %v", err)
				} else if exported.Len() == 0 {
					inclusions[journal.name] = "omitted (no records)"
				} else if err := addBytesToTar(tw, utils.RedactJSONLData(exported.Bytes()), journal.name); err != nil {
					inclusions[journal.name] = fmt.Sprintf("error writing to tar: %v", err)
				} else {
					inclusions[journal.name] = "included (exported from runtime.sqlite, redacted)"
				}
			}
		} else {
			// When filtering by a specific runID, we omit files that have no durable
			// run-scoped representation (e.g. session.json, stm.md, ltm.md,
			// logs/evidence_manifest.json) because they only reflect the current
			// or latest run of the workspace, not necessarily the requested run.

			// Filter events and collect task IDs
			// Collect exact attempts from events for transcript matching
			taskIDs := make(map[string]bool)
			attempts := make(map[string]bool) // "taskID:attempt"

			// filterJSONL adds source's JSONL rows for runID to the bundle under
			// relPath and returns them unredacted for derived projections.
			filterJSONL := func(relPath string, source io.Reader) []byte {
				var filtered []byte
				omittedLegacy := 0
				scanner := bufio.NewScanner(source)
				buf := make([]byte, 0, 64*1024)
				// Journal records can reach 4 MiB.
				scanner.Buffer(buf, 4*1024*1024)
				for scanner.Scan() {
					var ev map[string]interface{}
					if json.Unmarshal(scanner.Bytes(), &ev) == nil {
						match := false
						if rid, ok := ev["run_id"].(string); ok && rid != "" {
							if rid == runID {
								match = true
							}
						} else {
							omittedLegacy++
						}
						if match {
							filtered = append(filtered, scanner.Bytes()...)
							filtered = append(filtered, '\n')
							if tid, _ := ev["task_id"].(string); tid != "" {
								taskIDs[tid] = true
								if att, ok := ev["attempt"].(float64); ok {
									attempts[fmt.Sprintf("%s:%d", tid, int(att))] = true
								}
							}
						}
					}
				}
				if omittedLegacy > 0 {
					inclusions[relPath+"_omitted_legacy_rows"] = fmt.Sprintf("%d unscoped legacy rows omitted", omittedLegacy)
				}
				if err := scanner.Err(); err != nil {
					inclusions[relPath] = fmt.Sprintf("error parsing: %v", err)
				} else if len(filtered) > 0 {
					if err := addBytesToTar(tw, utils.RedactJSONLData(filtered), relPath); err != nil {
						inclusions[relPath] = fmt.Sprintf("error writing to tar: %v", err)
					} else {
						inclusions[relPath] = "included (filtered, redacted)"
					}
				} else {
					inclusions[relPath] = "omitted (empty after filter)"
				}
				return filtered
			}

			// Execution-event telemetry is derived from the run's canonical
			// events; the bundle carries it under its historical name, and
			// its attempts select the run's task transcripts.
			addDerivedExecutionEvents := func(storeRows []byte) {
				const relPath = "logs/execution-events.jsonl"
				projector := team.NewExecutionEventProjector()
				for _, line := range bytes.Split(storeRows, []byte{'\n'}) {
					var event team.RunEvent
					if json.Unmarshal(line, &event) == nil {
						projector.Add(event)
					}
				}
				var derived []byte
				for _, row := range projector.Rows() {
					if row.RunID != runID {
						continue
					}
					data, err := json.Marshal(row)
					if err != nil {
						inclusions[relPath] = fmt.Sprintf("error encoding: %v", err)
						return
					}
					derived = append(append(derived, data...), '\n')
					if row.TaskID != "" {
						taskIDs[row.TaskID] = true
						attempts[fmt.Sprintf("%s:%d", row.TaskID, row.Attempt)] = true
					}
				}
				if len(derived) == 0 {
					inclusions[relPath] = "omitted (no execution events for run)"
					return
				}
				if err := addBytesToTar(tw, utils.RedactJSONLData(derived), relPath); err != nil {
					inclusions[relPath] = fmt.Sprintf("error writing to tar: %v", err)
				} else {
					inclusions[relPath] = "included (derived from event store, redacted)"
				}
			}
			// addRunEvents adds the run's journal rows, exported as JSONL, and
			// returns them unredacted for derived projections.
			addRunEvents := func() []byte {
				events, unscoped, err := team.ReadUnvalidatedRunEvents(cmd.Context(), workspacePath, runID)
				if err != nil {
					inclusions[debugEventLogName] = fmt.Sprintf("error reading: %v", err)
					return nil
				}
				if unscoped > 0 {
					inclusions[debugEventLogName+"_omitted_legacy_rows"] = fmt.Sprintf("%d unscoped legacy rows omitted", unscoped)
				}
				var rows []byte
				for _, event := range events {
					data, err := json.Marshal(event)
					if err != nil {
						inclusions[debugEventLogName] = fmt.Sprintf("error encoding: %v", err)
						return nil
					}
					rows = append(append(rows, data...), '\n')
					if event.TaskID != "" {
						taskIDs[event.TaskID] = true
						if event.Attempt != 0 {
							attempts[fmt.Sprintf("%s:%d", event.TaskID, event.Attempt)] = true
						}
					}
				}
				if len(rows) == 0 {
					inclusions[debugEventLogName] = "omitted (empty after filter)"
				} else if err := addBytesToTar(tw, utils.RedactJSONLData(rows), debugEventLogName); err != nil {
					inclusions[debugEventLogName] = fmt.Sprintf("error writing to tar: %v", err)
				} else {
					inclusions[debugEventLogName] = "included (filtered, redacted)"
				}
				return rows
			}
			addDerivedExecutionEvents(addRunEvents())
			// The task journal lives in runtime.sqlite; the bundle carries its
			// run's records as JSONL under the historical name.
			var taskJournal bytes.Buffer
			if err := team.ExportTaskJournal(cmd.Context(), workspacePath, &taskJournal); err != nil {
				inclusions["logs/task_journal.jsonl"] = fmt.Sprintf("error reading: %v", err)
			} else {
				filterJSONL("logs/task_journal.jsonl", &taskJournal)
			}

			// task-output
			outDir := filepath.Join(workspacePath, "logs", "task-output")
			if entries, err := debugReadDir(outDir); err == nil {
				for _, e := range entries {
					if e.IsDir() {
						continue
					}
					name := e.Name()
					matched := false
					for taskAndAtt := range attempts {
						parts := strings.SplitN(taskAndAtt, ":", 2)
						if len(parts) == 2 {
							expectedPrefix := fmt.Sprintf("%s-%s-attempt-%s", parts[0], runID, parts[1])
							if strings.HasPrefix(name, expectedPrefix+".jsonl") || strings.HasPrefix(name, expectedPrefix+"-raw_transcript.txt") {
								matched = true
								break
							}
						}
					}
					if matched {
						rel := "logs/task-output/" + name
						if status, terr := addRedactedPathToTar(tw, filepath.Join(outDir, name), rel); terr == nil {
							inclusions[rel] = status
						} else {
							inclusions[rel] = fmt.Sprintf("error: %v", terr)
						}
					}
				}
			}

			// artifacts meta and data
			metaDir := filepath.Join(workspacePath, "logs", "artifacts", "meta")
			if entries, err := debugReadDir(metaDir); err == nil {
				for _, e := range entries {
					if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
						continue
					}
					metaPath := filepath.Join(metaDir, e.Name())
					metaFile, err := debugOpenFile(metaPath)
					if errors.Is(err, errDebugSymlinkPath) {
						inclusions["logs/artifacts/meta/"+e.Name()] = "omitted (symlink path)"
						continue
					}
					if err != nil {
						continue
					}
					data, err := io.ReadAll(metaFile)
					_ = metaFile.Close()
					if err != nil {
						continue
					}
					var m map[string]interface{}
					if json.Unmarshal(data, &m) == nil {
						if rid, _ := m["run_id"].(string); rid == runID {
							rel := "logs/artifacts/meta/" + e.Name()
							if status, terr := addRedactedPathToTar(tw, metaPath, rel); terr == nil {
								inclusions[rel] = status
							} else {
								inclusions[rel] = fmt.Sprintf("error: %v", terr)
							}
							id := strings.TrimSuffix(e.Name(), ".json")
							dataPath := filepath.Join(workspacePath, "logs", "artifacts", "data", id)
							dataRel := "logs/artifacts/data/" + id
							if status, terr := addRedactedPathToTar(tw, dataPath, dataRel); terr == nil {
								inclusions[dataRel] = status
							} else {
								inclusions[dataRel] = fmt.Sprintf("error: %v", terr)
							}
						}
					}
				}
			}

			// runtime receipts
			receiptsDir := filepath.Join(workspacePath, "runtime", "receipts")
			if entries, err := debugReadDir(receiptsDir); err == nil {
				for _, e := range entries {
					if e.IsDir() {
						continue
					}
					path := filepath.Join(receiptsDir, e.Name())
					receiptFile, err := debugOpenFile(path)
					if errors.Is(err, errDebugSymlinkPath) {
						inclusions["runtime/receipts/"+e.Name()] = "omitted (symlink path)"
						continue
					}
					if err != nil {
						continue
					}
					data, err := io.ReadAll(receiptFile)
					_ = receiptFile.Close()
					if err != nil {
						continue
					}
					var m map[string]interface{}
					if json.Unmarshal(data, &m) == nil {
						if rid, _ := m["run_id"].(string); rid == runID {
							rel := "runtime/receipts/" + e.Name()
							if status, terr := addRedactedPathToTar(tw, path, rel); terr == nil {
								inclusions[rel] = status
							} else {
								inclusions[rel] = fmt.Sprintf("error: %v", terr)
							}
						}
					}
				}
			}
		}

		manifest["files"] = inclusions
		manifestData, _ := json.MarshalIndent(manifest, "", "  ")
		if err := addBytesToTar(tw, manifestData, "bundle-manifest.json"); err != nil {
			if finalErr == nil {
				finalErr = fmt.Errorf("failed to write manifest: %w", err)
			}
		}

		if err := tw.Close(); err != nil && finalErr == nil {
			finalErr = err
		}
		if err := gw.Close(); err != nil && finalErr == nil {
			finalErr = err
		}
		if err := f.Close(); err != nil && finalErr == nil {
			finalErr = err
		}

		if finalErr == nil {
			fmt.Println("Debug bundle created successfully.")
		} else {
			_ = os.Remove(bundleName)
		}
		return finalErr
	},
}

// debugEventLogName is where a bundle carries the event journal's JSONL
// export, the path the retired file log used.
const debugEventLogName = "logs/event_store.jsonl"

// verifyRunID tolerates a damaged journal: a debug bundle must still find a
// run whose chain no longer validates.
func verifyRunID(ctx context.Context, workspacePath, runID string) error {
	events, _, err := team.ReadUnvalidatedRunEvents(ctx, workspacePath, runID)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("event store not found (is %s a valid workspace?)", workspacePath)
	}
	if err != nil {
		return fmt.Errorf("error reading event store: %w", err)
	}
	if len(events) == 0 {
		return fmt.Errorf("run ID %q not found in event store", runID)
	}
	return nil
}

func addBytesToTar(tw *tar.Writer, data []byte, name string) error {
	header := &tar.Header{
		Name: name,
		Size: int64(len(data)),
		Mode: 0644,
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func debugReadDir(path string) ([]os.DirEntry, error) {
	dir, err := debugOpenFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("debug path is not a directory: %s", path)
	}
	return dir.ReadDir(-1)
}

// addRedactedPathToTar is the only file exporter used by run-id bundles.
// Text is redacted before it enters the tar; binary/unknown content is omitted
// because it cannot be safely inspected for credentials or private keys.
func addRedactedPathToTar(tw *tar.Writer, path, name string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		included, redacted, omitted := 0, 0, 0
		err = filepath.Walk(path, func(filePath string, fileInfo os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if fileInfo.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(path, filePath)
			if relErr != nil {
				return relErr
			}
			status, writeErr := addRedactedFileToTar(tw, filePath, filepath.Join(name, rel))
			if writeErr != nil {
				return writeErr
			}
			switch status {
			case "included":
				included++
			case "included (redacted)":
				included++
				redacted++
			default:
				omitted++
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if included == 0 {
			return fmt.Sprintf("omitted (%d unsafe files)", omitted), nil
		}
		if redacted > 0 || omitted > 0 {
			return fmt.Sprintf("included (redacted; %d omitted)", omitted), nil
		}
		return "included", nil
	}
	return addRedactedFileToTar(tw, path, name)
}

func addRedactedFileToTar(tw *tar.Writer, path, name string) (string, error) {
	if isDebugSensitivePath(name) {
		return "omitted (sensitive path)", nil
	}
	file, err := debugOpenFile(path)
	if errors.Is(err, errDebugSymlinkPath) {
		return "omitted (symlink path)", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "omitted (unsupported file type)", nil
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	if !isDebugText(data) {
		return "omitted (binary/unknown content)", nil
	}
	redacted := redactDebugFileData(data, filepath.Ext(name))
	if err := addBytesToTar(tw, redacted, filepath.ToSlash(name)); err != nil {
		return "", err
	}
	if !bytes.Equal(data, redacted) {
		return "included (redacted)", nil
	}
	return "included", nil
}

// redactDebugFileData redacts file content being added to a debug export tar.
// JSON and JSONL content uses the JSON-aware redactor so escaped string
// values are unescaped before redaction; other content uses the text redactor.
func redactDebugFileData(data []byte, ext string) []byte {
	switch strings.ToLower(ext) {
	case ".json":
		return utils.RedactJSONFileData(data)
	case ".jsonl":
		return utils.RedactJSONLData(data)
	default:
		return []byte(utils.RedactSecrets(string(data)))
	}
}

// isDebugText is deliberately conservative. Valid UTF-8 is not sufficient to
// identify text because arbitrary binary data can avoid NUL bytes; reject
// control characters other than the whitespace used by ordinary logs/files.
func isDebugText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for _, r := range string(data) {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func isDebugSensitivePath(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == ".env" || strings.HasPrefix(name, ".env.") ||
			name == "session.json" || name == "stm.md" || name == "ltm.md" ||
			strings.HasPrefix(name, "runtime.sqlite") ||
			name == "acceptance_audit.jsonl" ||
			name == "evidence_manifest.json" || name == "credentials" ||
			name == "secrets" || strings.Contains(name, "credential") ||
			strings.Contains(name, "secret") || strings.Contains(name, "private-key") ||
			strings.Contains(name, "private_key") {
			return true
		}
	}
	return false
}
