package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/processutil"
)

const codexImageProtocolVersion = "0.166.0"

// Probe the configured executable, never the app-server's userAgent. Only the
// schema version checked into this repository grants native image support.
func codexImageBinaryIdentity(ctx context.Context, argv, environment []string) (string, error) {
	if len(argv) == 0 {
		return "", media.Reject(media.Unsupported, "Codex image executable is unavailable")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	before, err := codexImageBinaryHash(path)
	if err != nil {
		return "", err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, path, "--version")
	command.Env = environment
	output := newBoundedBuffer(1024)
	command.Stdout = output
	command.Stderr = newBoundedBuffer(1024)
	if err := processutil.RunContextCommand(command, time.Second); err != nil {
		return "", media.Reject(media.Unsupported, "Codex image version probe failed")
	}
	if output.Truncated() || strings.TrimSpace(output.String()) != "codex-cli "+codexImageProtocolVersion {
		return "", media.Reject(media.Unsupported, "Codex native images require verified codex-cli "+codexImageProtocolVersion)
	}
	after, err := codexImageBinaryHash(path)
	if err != nil {
		return "", err
	}
	if before != after {
		return "", media.Reject(media.BindingConflict, "Codex executable changed during version verification")
	}
	return before, nil
}

func codexImageBinaryHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 512<<20 {
		return "", fmt.Errorf("invalid Codex image executable")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, io.LimitReader(file, 512<<20+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func codexImageBinaryHashForCommand(argv []string) (string, error) {
	if len(argv) == 0 {
		return "", media.Reject(media.Unsupported, "Codex image executable is unavailable")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return "", err
	}
	return codexImageBinaryHash(path)
}
