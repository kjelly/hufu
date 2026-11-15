package team

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/processutil"
)

const codexProjectionConsumerProcessEnv = "HUFU_TEST_CODEX_PROJECTION_CONSUMER"

func TestCodexProjectionConsumerProcess(t *testing.T) {
	path := os.Getenv(codexProjectionConsumerProcessEnv)
	if path == "" {
		t.Skip("subprocess-only consumer fixture")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	unlock, err := acquireCodexStateFileLock(t.Context(), filepath.Join(filepath.Dir(path), codexProjectionLockFile))
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := newCodexProjectionConsumer(t.Context(), root, media.MaxBranchBlobs)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = consumer.file.Close() }()
	fmt.Fprintln(os.Stdout, "consumer-ready")
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	// Leave the empty marker as an interrupted owner would. The parent owns
	// this entire synthetic workspace and validates the next-process sweep.
}

func startCodexProjectionConsumerProcess(t *testing.T, path string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCodexProjectionConsumerProcess$")
	command.Env = append(os.Environ(), codexProjectionConsumerProcessEnv+"="+path, fakeCodexServerScriptEnvVar+"=")
	command.Dir = path
	var stderr bytes.Buffer
	command.Stderr = &stderr
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		cancel()
		t.Fatal(err)
	}
	wait, err := processutil.StartAndWait(command)
	if err != nil {
		_ = input.Close()
		cancel()
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() {
		_ = command.Process.Kill()
		_ = input.Close()
		<-wait
		cancel()
	})
	t.Cleanup(stop)
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "consumer-ready\n" {
		stop()
		t.Fatalf("consumer did not acquire a real process lease: %q %v; stderr: %s", line, err, stderr.String())
	}
	return stop
}

func TestCodexImageProjectionConsumerCrashPreservesOtherProcess(t *testing.T) {
	c, ctx, request, hash := codexImageProjectionFixture(t)
	first, err := newCodexImageProjection(ctx, c, request, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ownerA := startCodexProjectionConsumerProcess(t, first.path)
	ownerB := startCodexProjectionConsumerProcess(t, first.path)
	ownerA() // A dies while the kernel lease of B remains live.
	first.Close()
	root, err := os.OpenRoot(codexImageProjectionPath(c.artifactStoreRootPath(), hash))
	if err != nil {
		t.Fatal("parent removed another process's bytes", err)
	}
	for _, ref := range request.Task.MediaBinding.Refs {
		if _, err := root.Stat(codexImageFilename(ref)); err != nil {
			_ = root.Close()
			t.Fatal(err)
		}
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	ownerB()
	// A new constructor must reclaim both orphaned markers, verify the retained
	// copies from canonical CAS, and remove copies only after its own lease ends.
	next, err := newCodexImageProjection(ctx, c, request, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if err := next.Verify(ctx, media.Limits{}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(next.path)
	if err != nil {
		t.Fatal(err)
	}
	consumers := 0
	for _, entry := range entries {
		if validCodexProjectionConsumerName(entry.Name()) {
			consumers++
		}
	}
	if consumers != 1 {
		t.Fatalf("orphaned consumer leases survived new-process reuse: %d", consumers)
	}
	next.Close()
	if _, err := os.Lstat(next.path); !os.IsNotExist(err) {
		t.Fatalf("orphan reuse left copies: %v", err)
	}
}
