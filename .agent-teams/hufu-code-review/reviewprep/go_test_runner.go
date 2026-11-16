package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/processutil"
)

const goTestSummaryHalf = 4096

type goTestLimits struct {
	Package     time.Duration
	Command     time.Duration
	Reserve     time.Duration
	OutputBytes int
}

func defaultGoTestLimits() goTestLimits {
	// The team's action timeout is 40m. Leave time for snapshot preparation,
	// command teardown and durable artifact publication; never inherit Go's 10m.
	return goTestLimits{Package: 30 * time.Minute, Command: 35 * time.Minute, Reserve: 30 * time.Second, OutputBytes: 64 * 1024 * 1024}
}

// goTestOutput retains the complete merged stream on disk and only a bounded
// head/tail in memory. Hitting the disk budget is an error, never valid evidence.
type goTestOutput struct {
	file        *os.File
	digest      hash.Hash
	cancel      context.CancelFunc
	limit       int
	count       int
	head        []byte
	tail        []byte
	probe       string
	testTimeout bool
	err         error
}

func (o *goTestOutput) Write(data []byte) (int, error) {
	if o.err != nil {
		return 0, o.err
	}
	if len(data) > o.limit-o.count {
		o.err = errors.New("go test diagnostic exceeds durable output budget")
		o.cancel()
		return 0, o.err
	}
	n, err := o.file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	_, _ = o.digest.Write(data[:n])
	o.count += n
	remaining := goTestSummaryHalf - len(o.head)
	if remaining > len(data[:n]) {
		remaining = n
	}
	o.head = append(o.head, data[:remaining]...)
	o.tail = append(o.tail, data[:n]...)
	if len(o.tail) > goTestSummaryHalf {
		o.tail = append([]byte(nil), o.tail[len(o.tail)-goTestSummaryHalf:]...)
	}
	// This is the Go test runner's alarm diagnostic, not task-prose parsing.
	// Keep overlap so a pipe write splitting the marker cannot hide the alarm.
	probe := o.probe + string(data[:n])
	if strings.HasPrefix(probe, "panic: test timed out after ") || strings.Contains(probe, "\npanic: test timed out after ") {
		o.testTimeout = true
	}
	if len(probe) > 64 {
		probe = probe[len(probe)-64:]
	}
	o.probe = probe
	if err != nil {
		o.err = err
		o.cancel()
	}
	return n, err
}

func (o *goTestOutput) summary() string {
	if o.count <= len(o.head) {
		return string(o.head)
	}
	if o.count <= 2*goTestSummaryHalf {
		return string(o.head) + string(o.tail[len(o.tail)-(o.count-len(o.head)):])
	}
	return string(o.head) + "\n[summary shortened; complete output is in output_artifact_id]\n" + string(o.tail)
}

func runSnapshotGoTests(ctx context.Context, snapshot *reviewSourceSnapshot, packages, environment []string, outputPath string, limits goTestLimits) (goTestVerification, error) {
	if limits.Package <= 0 || limits.Command <= 0 || limits.OutputBytes <= 0 || limits.Reserve < 0 {
		return goTestVerification{}, errors.New("invalid Go test execution limits")
	}
	budget := limits.Command
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline)-limits.Reserve < budget {
		budget = time.Until(deadline) - limits.Reserve
	}
	if budget <= 0 {
		return goTestVerification{}, errors.New("no time remains to run and seal Go test evidence")
	}
	packageTimeout := limits.Package
	// Reserve teardown time even when a caller supplies a shorter deadline.
	if budget/2 < packageTimeout && budget < limits.Package+5*time.Second {
		packageTimeout = budget / 2
	}
	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return goTestVerification{}, fmt.Errorf("create complete Go test output: %w", err)
	}
	output := &goTestOutput{file: file, digest: sha256.New(), cancel: cancel, limit: limits.OutputBytes}
	arguments := append([]string{"test", "-p", "2", "-mod=readonly", "-count=1", "-timeout", packageTimeout.String()}, packages...)
	command := exec.CommandContext(runCtx, "go", arguments...)
	command.Dir = snapshot.root
	command.Env = environment
	// A shared writer makes os/exec serialize stdout/stderr into one stream.
	command.Stdout = output
	command.Stderr = command.Stdout
	runErr := processutil.RunContextCommand(command, 5*time.Second)
	syncErr := file.Sync()
	closeErr := file.Close()
	if output.err != nil || syncErr != nil || closeErr != nil {
		return goTestVerification{}, fmt.Errorf("preserve complete Go test output: %w", errors.Join(output.err, syncErr, closeErr))
	}
	exitCode := 0
	if runErr != nil {
		exitCode = -1
		// The trusted-static interpreter does not expose errors.AsType yet.
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	kind := "none"
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		kind = "canceled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		kind = "caller_deadline"
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		kind = "command"
	case output.testTimeout && runErr != nil:
		kind = "test"
	}
	diagnostic := output.summary()
	if runErr != nil {
		diagnostic = fmt.Sprintf("Go test exit=%d; timeout_kind=%s\n", exitCode, kind) + diagnostic
		if output.count == 0 {
			diagnostic += runErr.Error()
		}
	}
	return goTestVerification{
		Completed:        kind == "none" && (exitCode == 0 || exitCode == 1),
		Passed:           runErr == nil && kind == "none",
		ReviewedRevision: snapshot.revision, BaseRevision: snapshot.baseRevision,
		Packages: packages, Command: append([]string{"go"}, arguments...), ExitCode: exitCode,
		TimedOut: kind == "test" || kind == "command" || kind == "caller_deadline", TimeoutKind: kind,
		OutputSHA256: hex.EncodeToString(output.digest.Sum(nil)), OutputBytes: output.count,
		SummaryTruncated: output.count > 2*goTestSummaryHalf, Diagnostic: diagnostic,
	}, nil
}
