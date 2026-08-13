package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

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
			workspacePath = opts.workspace
			if workspacePath == "" {
				workspacePath = "workspace"
			}
			runID = target

			// Verify run-id exists
			eventsPath := filepath.Join(workspacePath, "logs", "execution-events.jsonl")
			if err := verifyRunID(eventsPath, runID); err != nil {
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
		} else {
			// When filtering by a specific runID, we omit files that have no durable
			// run-scoped representation (e.g. session.json, stm.md, ltm.md,
			// logs/evidence_manifest.json) because they only reflect the current
			// or latest run of the workspace, not necessarily the requested run.

			// Filter events and collect task IDs
			// Collect exact attempts from events for transcript matching
			taskIDs := make(map[string]bool)
			attempts := make(map[string]bool) // "taskID:attempt"

			filterJSONL := func(relPath string, checkRunID bool, idField string) {
				fullPath := filepath.Join(workspacePath, relPath)
				fJSON, err := debugOpenFile(fullPath)
				if err != nil {
					if errors.Is(err, errDebugSymlinkPath) {
						inclusions[relPath] = "omitted (symlink path)"
						return
					}
					if os.IsNotExist(err) {
						inclusions[relPath] = "omitted (not found)"
					} else {
						inclusions[relPath] = fmt.Sprintf("error reading: %v", err)
					}
					return
				}
				defer func() { _ = fJSON.Close() }()
				var filtered []byte
				omittedLegacy := 0
				scanner := bufio.NewScanner(fJSON)
				buf := make([]byte, 0, 64*1024)
				scanner.Buffer(buf, 1024*1024)
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
					filtered = []byte(utils.RedactSecrets(string(filtered)))
					if err := addBytesToTar(tw, filtered, relPath); err != nil {
						inclusions[relPath] = fmt.Sprintf("error writing to tar: %v", err)
					} else {
						inclusions[relPath] = "included (filtered, redacted)"
					}
				} else {
					inclusions[relPath] = "omitted (empty after filter)"
				}
			}
			filterJSONL("logs/execution-events.jsonl", true, "")
			filterJSONL("logs/event_store.jsonl", true, "")
			filterJSONL("logs/task_journal.jsonl", false, "task_id")

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

func verifyRunID(eventsPath string, runID string) error {
	f, err := debugOpenFile(eventsPath)
	if errors.Is(err, errDebugSymlinkPath) {
		return fmt.Errorf("execution events path contains a symlink")
	}
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("execution events log not found (is %s a valid workspace?)", filepath.Dir(filepath.Dir(eventsPath)))
		}
		return err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// increase buffer size in case of large JSON
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	type miniEvent struct {
		RunID string `json:"run_id"`
	}

	for scanner.Scan() {
		var ev miniEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err == nil {
			if ev.RunID == runID {
				return nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading execution events: %w", err)
	}
	return fmt.Errorf("run ID %q not found in execution events", runID)
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
	redacted := []byte(utils.RedactSecrets(string(data)))
	if err := addBytesToTar(tw, redacted, filepath.ToSlash(name)); err != nil {
		return "", err
	}
	if !bytes.Equal(data, redacted) {
		return "included (redacted)", nil
	}
	return "included", nil
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
			name == "evidence_manifest.json" || name == "credentials" ||
			name == "secrets" || strings.Contains(name, "credential") ||
			strings.Contains(name, "secret") || strings.Contains(name, "private-key") ||
			strings.Contains(name, "private_key") {
			return true
		}
	}
	return false
}
