package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func runCompositionProcess(t *testing.T, binary, root string, args ...string) (int, []byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = root
	cmd.Env = []string{"HOME=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, ".config"), "XDG_CACHE_HOME=" + filepath.Join(root, ".cache"), "XDG_DATA_HOME=" + filepath.Join(root, ".local/share"), "HUFU_AGENT_TEAM_SEARCH_PATH=" + filepath.Join(root, ".agent-teams"), "PATH=" + os.Getenv("PATH"), "NO_COLOR=1", "TERM=dumb"}
	var out, errout bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errout
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("command timed out: %s", errout.String())
	}
	if err == nil {
		return 0, out.Bytes(), errout.Bytes()
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode(), out.Bytes(), errout.Bytes()
	}
	t.Fatal(err)
	return -1, nil, nil
}

func TestTeamSingleFileProcessJourney(t *testing.T) {
	binary := buildProcessContractBinary(t)
	root := t.TempDir()
	run := func(args ...string) []byte {
		t.Helper()
		code, out, stderr := runCompositionProcess(t, binary, root, args...)
		if code != 0 {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, out, stderr)
		}
		return out
	}
	run("team", "create", "dev", "--preset", "coding-reviewed", "--single-file", "--model", "ollama/composition-model")
	dir := filepath.Join(root, ".agent-teams/dev")
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 || files[0].Name() != "team.yaml" {
		t.Fatalf("files=%v err=%v", files, err)
	}
	original, err := os.ReadFile(filepath.Join(dir, "team.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	run("team", "validate", dir)
	lint := run("team", "lint", dir, "--format", "json", "--fail-on", "error")
	var lintDoc teamLintJSONDocument
	if err := json.Unmarshal(lint, &lintDoc); err != nil || !lintDoc.Complete {
		t.Fatalf("lint=%s err=%v", lint, err)
	}
	explain := run("team", "explain", dir, "--format", "json")
	if !bytes.Contains(explain, []byte("team-preset:coding-reviewed/developer")) {
		t.Fatalf("missing provenance: %s", explain)
	}
	show := run("team", "show", dir)
	if !bytes.Contains(show, []byte("preset: coding-reviewed")) || bytes.Contains(show, []byte("developer.md")) {
		t.Fatalf("show=%s", show)
	}
	for _, output := range []string{"text", "table", "json"} {
		listed := run("list", "dev", "--output", output)
		if output == "json" {
			var teams []listedTeam
			if err := json.Unmarshal(listed, &teams); err != nil || len(teams) != 1 || len(teams[0].Agents) != 2 {
				t.Fatalf("list=%s err=%v", listed, err)
			}
			if teams[0].Agents[0].Name != "developer" || !slices.Contains(teams[0].Agents[0].Tools, "write") {
				t.Fatalf("agents=%#v", teams[0].Agents)
			}
		} else if !bytes.Contains(listed, []byte("@developer")) || !bytes.Contains(listed, []byte("@reviewer")) {
			t.Fatalf("list=%s", listed)
		}
	}
	for _, extra := range [][]string{nil, {"--force"}} {
		args := append([]string{"team", "add", "dev", "developer", "--preset", "readonly"}, extra...)
		code, _, _ := runCompositionProcess(t, binary, root, args...)
		if code == 0 {
			t.Fatal("team add replaced manifest member")
		}
		if _, err := os.Stat(filepath.Join(dir, "developer.md")); !os.IsNotExist(err) {
			t.Fatal("team add wrote colliding file")
		}
	}
	run("team", "add", "dev", "security", "--preset", "review")
	run("team", "validate", dir)
	run("team", "create", "minimal", "--single-file")
	minimal, err := os.ReadFile(filepath.Join(root, ".agent-teams/minimal/team.yaml"))
	if err != nil || string(minimal) != "preset: coding-single\n" {
		t.Fatalf("default=%s err=%v", minimal, err)
	}
	for _, flags := range [][]string{{"--force"}, {"--force=false"}, {"--wizard=false"}, {"--expanded=false"}, {"--from="}, {"--category=general"}, {"--preset=coding"}} {
		args := append([]string{"team", "create", "rejected", "--single-file"}, flags...)
		if code, _, _ := runCompositionProcess(t, binary, root, args...); code == 0 {
			t.Fatalf("accepted %v", flags)
		}
		if _, err := os.Stat(filepath.Join(root, ".agent-teams/rejected")); !os.IsNotExist(err) {
			t.Fatalf("rejected %v wrote directory", flags)
		}
	}
	if code, _, _ := runCompositionProcess(t, binary, root, "team", "create", "dev", "--single-file"); code == 0 {
		t.Fatal("overwrote existing directory")
	}
	after, err := os.ReadFile(filepath.Join(dir, "team.yaml"))
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("existing manifest changed")
	}
	bad := filepath.Join(root, ".agent-teams/bad")
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "team.yaml"), []byte("preset: [broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCompositionProcess(t, binary, root, "list", "--output", "json")
	if code == 0 || len(out) != 0 {
		t.Fatalf("invalid manifest produced partial JSON: code=%d out=%s", code, out)
	}
}

type failingCompositionWriter struct{ *os.File }

func (w failingCompositionWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected write failure")
}

func TestTeamSingleFileOwnershipAndRollback(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "new")
	failure := errors.New("injected validation failure")
	err := writeSingleFileTeam(dir, []byte("preset: coding-single\n"), func(string) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("validation failure=%v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("failed validation left generated directory")
	}
	if err := writeSingleFileTeamWithOpen(dir, []byte("preset: coding-single\n"), func(string) error { return nil }, func(path string) (singleFileManifest, error) {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		return failingCompositionWriter{file}, err
	}); err == nil {
		t.Fatal("write failure lost")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("failed write left generated directory")
	}
	err = writeSingleFileTeam(dir, []byte("preset: coding-single\n"), func(path string) error {
		if err := os.WriteFile(filepath.Join(path, "concurrent.txt"), []byte("preserve"), 0o644); err != nil {
			t.Fatal(err)
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "team.yaml")); !os.IsNotExist(err) {
		t.Fatal("owned manifest remained")
	}
	foreign, err := os.ReadFile(filepath.Join(dir, "concurrent.txt"))
	if err != nil || string(foreign) != "preserve" {
		t.Fatal("cleanup removed concurrent file")
	}
	for _, path := range []string{dir, filepath.Join(root, "empty"), filepath.Join(root, "link")} {
		if strings.HasSuffix(path, "empty") {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if strings.HasSuffix(path, "link") {
			if err := os.Symlink(filepath.Join(root, "absent"), path); err != nil {
				t.Fatal(err)
			}
		}
		if err := writeSingleFileTeam(path, []byte("preset: coding-single\n"), func(string) error { return nil }); err == nil {
			t.Fatalf("accepted preexisting path %s", path)
		}
	}
}

func TestTeamSingleFileConcurrentCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "race")
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if err := writeSingleFileTeam(dir, []byte("preset: coding-single\n"), validateSingleFileTeam); err == nil {
				successes.Add(1)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful writers=%d", successes.Load())
	}
	if err := validateSingleFileTeam(dir); err != nil {
		t.Fatal(err)
	}
}

func TestTeamSingleFileDirectCreateResetsLegacyFlags(t *testing.T) {
	resetTeamCreateFlags()
	t.Cleanup(resetTeamCreateFlags)
	root := chdirTemp(t)
	teamCreateSingleFile = true
	if err := runTeamCreate(&cobra.Command{}, []string{"dev"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agent-teams/dev/team.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestTeamSingleFileCleanupPreservesReplacedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	err := writeSingleFileTeam(dir, []byte("preset: coding-single\n"), func(path string) error {
		if err := os.Rename(path, path+"-moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "team.yaml"), []byte("foreign"), 0o644); err != nil {
			t.Fatal(err)
		}
		return errors.New("injected validation failure after directory replacement")
	})
	if err == nil {
		t.Fatal("failure lost")
	}
	data, err := os.ReadFile(filepath.Join(dir, "team.yaml"))
	if err != nil || string(data) != "foreign" {
		t.Fatal("cleanup deleted foreign replacement")
	}
}
