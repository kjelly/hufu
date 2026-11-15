package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/media"
)

// Real app-server subprocesses hold their turns open after verifying the
// canonical image bytes. Both must read while neither consumer has drained.
// Full provider admission/retry/resume remains covered by codex_images_test.
func TestCodexImageProjectionsReachIndependentAppServersConcurrently(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_binding=%t", same), func(t *testing.T) {
			c, ctx, request, hash := codexImageProjectionFixture(t)
			requests := []AttemptRequest{request, request}
			contexts := []context.Context{ctx, ctx}
			hashes := []string{hash, hash}
			if !same {
				_, contexts[1], requests[1], hashes[1] = codexImageProjectionRequest(t, c, ctx, admittedMediaTask(t, c, ctx, "parallel-turn"))
				if requests[0].TaskID == requests[1].TaskID || hashes[0] == hashes[1] {
					t.Fatal("fixture did not create independent task bindings")
				}
			}
			digest := sha256.Sum256(mediaStorePNG(t))
			imageHash := hex.EncodeToString(digest[:])
			var projections []*codexImageProjection
			var stops []func()
			var cancels []context.CancelFunc
			var results []<-chan error
			var ready []string
			started := time.Now()
			for i := range 2 {
				projection, err := newCodexImageProjection(contexts[i], c, requests[i], hashes[i])
				if err != nil {
					t.Fatal(err)
				}
				projections = append(projections, projection)
				t.Cleanup(projection.Close)
				home := t.TempDir()
				unlock, err := acquireCodexHomeStateLock(t.Context(), home)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(unlock)
				marker := filepath.Join(home, "images-read")
				script := fakeCodexScript{Steps: []fakeCodexStep{codexInitializeStep(t), fakeCodexThreadStartStep("thread-1", c.projectDir, "gpt-5-codex", codexSandboxReadOnly, t),
					{ImageHashes: []string{imageHash, imageHash}, NoResponse: true, WriteFile: marker, WriteFileContent: "read"}}}
				data, err := json.Marshal(script)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(home, "script.json")
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				supervisor := NewProcessSupervisor()
				process, err := StartCodexAppServer(t.Context(), supervisor, CodexProcessConfig{Argv: []string{os.Args[0]}, Dir: c.projectDir,
					Env: append(os.Environ(), "CODEX_HOME="+home, fakeCodexServerScriptEnvVar+"="+path), StartupTimeout: 5 * time.Second})
				if err != nil {
					t.Fatal(err)
				}
				stop := sync.OnceFunc(func() {
					_ = supervisor.KillTree(context.Background(), process.Handle())
					_ = process.Client.Close()
					_ = process.Wait()
					unlock()
				})
				stops = append(stops, stop)
				t.Cleanup(stop)
				_, err = codexStartOrResumeThread(t.Context(), process.Client, "", CodexThreadConfig{CWD: c.projectDir, Model: "gpt-5-codex", Sandbox: codexSandboxReadOnly}, nil)
				if err != nil {
					t.Fatal(err)
				}
				callCtx, cancel := context.WithCancel(t.Context())
				cancels = append(cancels, cancel)
				done := make(chan error, 1)
				var calls sync.WaitGroup
				calls.Go(func() {
					var result codexTurnStartResult
					done <- process.Client.Call(callCtx, codexMethodTurnStart, codexTurnStartParams{ThreadID: "thread-1", Input: projection.Inputs(requests[i].Prompt)}, &result)
				})
				t.Cleanup(func() { cancel(); stop(); calls.Wait() })
				results = append(results, done)
				ready = append(ready, marker)
			}
			waitCtx, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancelWait()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for _, path := range ready {
				for {
					if data, err := os.ReadFile(path); err == nil && string(data) == "read" {
						break
					}
					select {
					case <-waitCtx.Done():
						t.Fatal("both app-servers did not read while their turns were active", waitCtx.Err())
					case <-ticker.C:
					}
				}
			}
			t.Logf("two active app-servers read verified images in %s", time.Since(started))
			for i := range 2 {
				select {
				case err := <-results[i]:
					t.Fatalf("consumer exited before cancellation: %v", err)
				default:
				}
			}
			cancels[0]()
			if err := <-results[0]; err == nil {
				t.Fatal("cancelled turn succeeded")
			}
			stops[0]() // Drain/reap the first child before releasing its image lease.
			projections[0].Close()
			if err := projections[1].Verify(ctx, media.Limits{}); err != nil {
				t.Fatal("first drain removed live consumer's bytes", err)
			}
			cancels[1]()
			if err := <-results[1]; err == nil {
				t.Fatal("cancelled turn succeeded")
			}
			stops[1]()
			projections[1].Close()
			for _, projection := range projections {
				if _, err := os.Lstat(projection.path); !os.IsNotExist(err) {
					t.Fatalf("last consumer left image directory: %v", err)
				}
			}
		})
	}
}
