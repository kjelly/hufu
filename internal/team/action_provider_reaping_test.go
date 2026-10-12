//go:build unix

package team

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestCommandActionCancellationStopsDescendantsIncludingPTY(t *testing.T) {
	for _, pty := range []bool{false, true} {
		name := "shell"
		if pty {
			name = "pty"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			ready, marker := filepath.Join(root, "ready"), filepath.Join(root, "late-write")
			t.Setenv("HUFU_REAP_READY", ready)
			t.Setenv("HUFU_REAP_MARKER", marker)
			body := `printf ready > "$HUFU_REAP_READY"; (sleep 0.6; printf orphan > "$HUFU_REAP_MARKER") & wait`
			command := []string{"/bin/sh", "-c", body}
			if pty {
				if runtime.GOOS != "linux" {
					t.Skip("PTY fixture requires util-linux script")
				}
				t.Setenv("SHELL", "/bin/sh")
				script, err := exec.LookPath("script")
				if err != nil {
					t.Skip("util-linux script is unavailable")
				}
				command = []string{script, "--quiet", "--return", "--command", body, "/dev/null"}
			}
			provider := &commandActionProvider{capability: "demo", command: command}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			finished := make(chan error, 1)
			var workers sync.WaitGroup
			workers.Go(func() {
				_, err := provider.Execute(ctx, Action{Capability: "demo", Type: "apply"})
				finished <- err
			})
			defer func() { cancel(); workers.Wait() }()
			deadline := time.Now().Add(2 * time.Second)
			for {
				select {
				case err := <-finished:
					t.Fatalf("provider exited before its child was ready: %v", err)
				default:
				}
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("child never became ready")
				}
				time.Sleep(5 * time.Millisecond)
			}
			started := time.Now()
			cancel()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) || time.Since(started) > 750*time.Millisecond {
					t.Fatalf("unbounded cancellation: elapsed=%s error=%v", time.Since(started), err)
				}
			case <-time.After(time.Second):
				t.Fatal("provider did not reap its command")
			}
			time.Sleep(700 * time.Millisecond)
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("descendant continued writing after cancellation: %v", err)
			}
		})
	}
}
