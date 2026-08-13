package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDebugCmd_WorkspaceValidation(t *testing.T) {
	tmpDir := t.TempDir()

	originalWD, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(originalWD)

	// 1. Create a fake workspace with an execution-events.jsonl containing a run_id
	workspaceDir := "workspace"
	if err := os.MkdirAll(filepath.Join(workspaceDir, "logs"), 0755); err != nil {
		t.Fatal(err)
	}

	eventsPath := filepath.Join(workspaceDir, "logs", "execution-events.jsonl")
	if err := os.WriteFile(eventsPath, []byte(`{"run_id":"run-12345"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Set the global opts.workspace to the tmp workspace
	originalWorkspace := opts.workspace
	opts.workspace = workspaceDir
	defer func() { opts.workspace = originalWorkspace }()

	// 3. Test missing run-id
	cmd := newRootCommand()
	cmd.SetArgs([]string{"debug", "run-missing"})
	err := cmd.Execute()
	if err == nil {
		t.Errorf("expected error for missing run-id, got nil")
	} else if !strings.Contains(err.Error(), "run ID \"run-missing\" not found") {
		t.Errorf("unexpected error message: %v", err)
	}

	// 4. Test existing run-id
	cmd = newRootCommand()
	cmd.SetArgs([]string{"debug", "run-12345"})
	err = cmd.Execute()
	if err != nil {
		t.Errorf("expected success for existing run-id, got: %v", err)
	}

	bundlePath := filepath.Join(tmpDir, "hufu-debug-run-12345.tar.gz")
	if _, err := os.Stat(bundlePath); os.IsNotExist(err) {
		t.Errorf("expected bundle to be created at %s, but not found", bundlePath)
	}

	// 5. Test output collision
	cmd = newRootCommand()
	cmd.SetArgs([]string{"debug", "run-12345"})
	err = cmd.Execute()
	if err == nil {
		t.Errorf("expected error when bundle already exists, got nil")
	} else if !strings.Contains(err.Error(), "refusing to overwrite existing bundle") {
		t.Errorf("unexpected error message for collision: %v", err)
	}
}

func TestDebugCmd_BundleContents(t *testing.T) {
	tmpDir := t.TempDir()

	// 1. Create a fake workspace with files
	workspaceDir := filepath.Join(tmpDir, "myworkspace")
	if err := os.MkdirAll(filepath.Join(workspaceDir, "logs", "task-output"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(workspaceDir, "session.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "event_store.jsonl"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", ".env"), []byte("API_TOKEN=top-secret-value"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "task-output", "transcript.txt"), []byte(`transcript`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceDir, "runtime", "artifacts"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "runtime", "artifacts", "binary-output"), []byte{0, 'P', 'R', 'I', 'V', 'A', 'T', 'E'}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "runtime", "artifacts", "invalid-binary-output"), []byte{0xff, 0xfe, 0xfd, 'P', 'R', 'I', 'V', 'A', 'T', 'E'}, 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Test directory argument
	originalWD, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(originalWD)

	cmd := newRootCommand()
	cmd.SetArgs([]string{"debug", workspaceDir})
	err := cmd.Execute()
	if err != nil {
		t.Errorf("expected success for workspace dir, got: %v", err)
	}

	bundlePath := filepath.Join(tmpDir, "hufu-debug-myworkspace.tar.gz")
	f, err := os.Open(bundlePath)
	if err != nil {
		t.Fatalf("failed to open bundle: %v", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("failed to open gzip: %v", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	foundFiles := make(map[string]bool)
	var manifest []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read error: %v", err)
		}
		foundFiles[hdr.Name] = true
		if hdr.Name == "bundle-manifest.json" {
			manifest, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("failed to read bundle manifest: %v", err)
			}
		}
	}

	expectedFiles := []string{
		"logs/event_store.jsonl",
		"logs/task-output/transcript.txt",
		"bundle-manifest.json",
	}

	for _, ef := range expectedFiles {
		if !foundFiles[ef] {
			t.Errorf("expected bundle to contain %s, but it didn't", ef)
		}
	}
	if foundFiles["session.json"] || foundFiles["logs/.env"] {
		t.Fatal("directory debug bundle included a sensitive or non-run-scoped file")
	}
	if foundFiles["runtime/artifacts/binary-output"] {
		t.Fatal("directory debug bundle included binary content")
	}
	if foundFiles["runtime/artifacts/invalid-binary-output"] {
		t.Fatal("directory debug bundle included invalid UTF-8 binary content")
	}
	if !strings.Contains(string(manifest), "runtime/artifacts/binary-output") || !strings.Contains(string(manifest), "runtime/artifacts/invalid-binary-output") || !strings.Contains(string(manifest), "binary/unknown content") {
		t.Fatalf("manifest did not record binary omission: %s", manifest)
	}
}

func TestDebugCmd_RunIDContents(t *testing.T) {
	tmpDir := t.TempDir()

	workspaceDir := filepath.Join(tmpDir, "myworkspace")
	if err := os.MkdirAll(filepath.Join(workspaceDir, "logs", "task-output"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "execution-events.jsonl"), []byte(`{"run_id":"run-42", "task_id":"task-7", "attempt": 1}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "task-output", "task-7-run-42-attempt-1.jsonl"), []byte(`transcript attempt 1`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceDir, "logs", "artifacts", "meta"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceDir, "logs", "artifacts", "data"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "artifacts", "meta", "artifact-7.json"), []byte(`{"run_id":"run-42","name":"artifact"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "artifacts", "data", "artifact-7"), []byte("API_TOKEN=top-secret-value\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "artifacts", "meta", "artifact-invalid.json"), []byte(`{"run_id":"run-42","name":"invalid"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "artifacts", "data", "artifact-invalid"), []byte{0xff, 0xfe, 0xfd, 'S', 'E', 'C', 'R', 'E', 'T'}, 0644); err != nil {
		t.Fatal(err)
	}
	externalArtifact := filepath.Join(tmpDir, "outside-artifact.txt")
	if err := os.WriteFile(externalArtifact, []byte("outside-secret-value"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "artifacts", "meta", "artifact-link.json"), []byte(`{"run_id":"run-42","name":"linked"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalArtifact, filepath.Join(workspaceDir, "logs", "artifacts", "data", "artifact-link")); err != nil {
		t.Skipf("symlink test unavailable: %v", err)
	}

	originalWD, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(originalWD)

	cmd := newRootCommand()
	cmd.SetArgs([]string{"debug", "run-42", "-w", workspaceDir})
	err := cmd.Execute()
	if err != nil {
		t.Errorf("expected success for run-id, got: %v", err)
	}

	bundlePath := filepath.Join(tmpDir, "hufu-debug-run-42.tar.gz")
	f, err := os.Open(bundlePath)
	if err != nil {
		t.Fatalf("failed to open bundle: %v", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("failed to open gzip: %v", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	foundFiles := make(map[string]bool)
	var artifactData []byte
	var manifest []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read error: %v", err)
		}
		foundFiles[hdr.Name] = true
		if hdr.Name == "logs/artifacts/data/artifact-7" {
			artifactData, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("failed to read redacted artifact: %v", err)
			}
		}
		if hdr.Name == "bundle-manifest.json" {
			manifest, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("failed to read bundle manifest: %v", err)
			}
		}
	}

	expectedFiles := []string{
		"logs/execution-events.jsonl",
		"logs/task-output/task-7-run-42-attempt-1.jsonl",
		"bundle-manifest.json",
	}

	for _, ef := range expectedFiles {
		if !foundFiles[ef] {
			t.Errorf("expected bundle to contain %s, but it didn't", ef)
		}
	}
	if !foundFiles["logs/artifacts/data/artifact-7"] {
		t.Fatal("expected run-scoped artifact data in bundle")
	}
	if foundFiles["logs/artifacts/data/artifact-invalid"] || !strings.Contains(string(manifest), "logs/artifacts/data/artifact-invalid") || !strings.Contains(string(manifest), "binary/unknown content") {
		t.Fatalf("run-scoped invalid binary was not safely omitted: files=%v manifest=%s", foundFiles, manifest)
	}
	if strings.Contains(string(artifactData), "top-secret-value") || !strings.Contains(string(artifactData), "[REDACTED]") {
		t.Fatalf("run-scoped artifact was not redacted: %q", artifactData)
	}
	if foundFiles["logs/artifacts/data/artifact-link"] || strings.Contains(string(manifest), "outside-secret-value") || !strings.Contains(string(manifest), "logs/artifacts/data/artifact-link") || !strings.Contains(string(manifest), "symlink path") {
		t.Fatalf("run-scoped symlink was not safely omitted: files=%v manifest=%s", foundFiles, manifest)
	}
}

func TestDebugCmd_RejectsJSONLSymlinks(t *testing.T) {
	tmpDir := t.TempDir()
	workspaceDir := filepath.Join(tmpDir, "workspace")
	logsDir := filepath.Join(workspaceDir, "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		t.Fatal(err)
	}

	externalEvents := filepath.Join(tmpDir, "external-events.jsonl")
	if err := os.WriteFile(externalEvents, []byte(`{"run_id":"run-events-link","secret":"outside-secret"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalEvents, filepath.Join(logsDir, "execution-events.jsonl")); err != nil {
		t.Skipf("symlink test unavailable: %v", err)
	}

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(originalWD) }()

	cmd := newRootCommand()
	cmd.SetArgs([]string{"debug", "run-events-link", "-w", workspaceDir})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "execution events path contains a symlink") {
		t.Fatalf("execution-events symlink was not rejected: %v", err)
	}

	if err := os.Remove(filepath.Join(logsDir, "execution-events.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logsDir, "execution-events.jsonl"), []byte(`{"run_id":"run-filter"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	externalStore := filepath.Join(tmpDir, "external-event-store.jsonl")
	if err := os.WriteFile(externalStore, []byte(`{"run_id":"run-filter","secret":"outside-secret"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalStore, filepath.Join(logsDir, "event_store.jsonl")); err != nil {
		t.Skipf("symlink test unavailable: %v", err)
	}

	cmd = newRootCommand()
	cmd.SetArgs([]string{"debug", "run-filter", "-w", workspaceDir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("run-id export with filtered JSONL symlink failed: %v", err)
	}
	f, err := os.Open(filepath.Join(tmpDir, "hufu-debug-run-filter.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	var manifest []byte
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if hdr.Name == "logs/event_store.jsonl" {
			t.Fatal("run-id bundle followed event_store JSONL symlink")
		}
		if hdr.Name == "bundle-manifest.json" {
			manifest, err = io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if strings.Contains(string(manifest), "outside-secret") || !strings.Contains(string(manifest), "logs/event_store.jsonl") || !strings.Contains(string(manifest), "symlink path") {
		t.Fatalf("filtered JSONL symlink was not safely omitted: %s", manifest)
	}
}

func TestDebugCmd_RejectsDanglingOutputSymlink(t *testing.T) {
	tmpDir := t.TempDir()
	workspaceDir := filepath.Join(tmpDir, "workspace")
	if err := os.MkdirAll(filepath.Join(workspaceDir, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "execution-events.jsonl"), []byte(`{"run_id":"run-dangling"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(originalWD) }()

	// A dangling symlink at the bundle output path points outside the cwd.
	externalTarget := filepath.Join(tmpDir, "external-bundle-target.tar.gz")
	bundlePath := filepath.Join(tmpDir, "hufu-debug-run-dangling.tar.gz")
	if err := os.Symlink(externalTarget, bundlePath); err != nil {
		t.Skipf("symlink test unavailable: %v", err)
	}

	cmd := newRootCommand()
	cmd.SetArgs([]string{"debug", "run-dangling", "-w", workspaceDir})
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite existing bundle") {
		t.Fatalf("dangling output symlink was not rejected: %v", err)
	}

	// The external target must not have been created by following the symlink.
	if _, statErr := os.Stat(externalTarget); !os.IsNotExist(statErr) {
		t.Fatalf("debug command followed dangling output symlink and created external target: %v", statErr)
	}
}

func TestDebugCmd_ConcurrentOutputSwapDoesNotWriteExternal(t *testing.T) {
	tmpDir := t.TempDir()
	workspaceDir := filepath.Join(tmpDir, "workspace")
	if err := os.MkdirAll(filepath.Join(workspaceDir, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "execution-events.jsonl"), []byte(`{"run_id":"run-swap"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(originalWD) }()

	// The external target is a real file with sentinel content. A concurrent
	// swap of the bundle path to a symlink pointing at it must never truncate
	// or overwrite it.
	externalTarget := filepath.Join(tmpDir, "external-swap-target.tar.gz")
	const sentinel = "EXTERNAL-SENTINEL-CONTENT"
	if err := os.WriteFile(externalTarget, []byte(sentinel), 0644); err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(tmpDir, "hufu-debug-run-swap.tar.gz")

	stop := make(chan struct{})
	var swaps sync.WaitGroup
	swaps.Add(1)
	go func() {
		defer swaps.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(bundlePath)
			if err := os.Symlink(externalTarget, bundlePath); err == nil {
				_ = os.Remove(bundlePath)
			}
		}
	}()

	cmd := newRootCommand()
	cmd.SetArgs([]string{"debug", "run-swap", "-w", workspaceDir})
	cmdErr := cmd.Execute()
	close(stop)
	swaps.Wait()

	// The command may succeed (creating its own regular file) or refuse to
	// overwrite, but it must never write through the symlink to the external
	// target.
	data, readErr := os.ReadFile(externalTarget)
	if readErr != nil {
		t.Fatalf("external target became unreadable: %v", readErr)
	}
	if string(data) != sentinel {
		t.Fatalf("concurrent output swap truncated/overwrote external target: %q", data)
	}
	if cmdErr != nil && !strings.Contains(cmdErr.Error(), "refusing to overwrite existing bundle") {
		t.Fatalf("unexpected error during concurrent output swap: %v", cmdErr)
	}
}

func TestDebugCmd_ConcurrentSymlinkSwapDoesNotLeak(t *testing.T) {
	tmpDir := t.TempDir()
	workspaceDir := filepath.Join(tmpDir, "workspace")
	dataDir := filepath.Join(workspaceDir, "logs", "artifacts", "data")
	metaDir := filepath.Join(workspaceDir, "logs", "artifacts", "meta")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(metaDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "execution-events.jsonl"), []byte(`{"run_id":"run-race"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dataDir, "race-artifact")
	backup := filepath.Join(dataDir, "race-artifact.safe")
	external := filepath.Join(tmpDir, "outside-secret.txt")
	if err := os.WriteFile(candidate, []byte("safe artifact"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(external, []byte("outside-race-secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, "race-artifact.json"), []byte(`{"run_id":"run-race"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dataDir, "race-artifact-link-probe")); err != nil {
		t.Skipf("symlink test unavailable: %v", err)
	}
	_ = os.Remove(filepath.Join(dataDir, "race-artifact-link-probe"))

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(originalWD) }()

	stop := make(chan struct{})
	var swaps sync.WaitGroup
	swaps.Add(1)
	go func() {
		defer swaps.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Rename(candidate, backup); err != nil {
				continue
			}
			if err := os.Symlink(external, candidate); err == nil {
				_ = os.Remove(candidate)
			}
			_ = os.Rename(backup, candidate)
		}
	}()

	cmd := newRootCommand()
	cmd.SetArgs([]string{"debug", "run-race", "-w", workspaceDir})
	cmdErr := cmd.Execute()
	close(stop)
	swaps.Wait()
	if cmdErr != nil {
		t.Fatalf("concurrent debug export failed: %v", cmdErr)
	}

	f, err := os.Open(filepath.Join(tmpDir, "hufu-debug-run-race.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		data, readErr := io.ReadAll(tr)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(data), "outside-race-secret") {
			t.Fatalf("concurrent symlink swap leaked external content in %s", hdr.Name)
		}
	}
}
