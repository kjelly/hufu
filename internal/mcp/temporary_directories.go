package mcp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/client"
)

func validateTemporaryDirectoryEnv(cfg MCPServerConfig) error {
	if len(cfg.TemporaryDirectoryEnv) == 0 {
		return nil
	}
	if cfg.Type != "" && cfg.Type != "local" {
		return fmt.Errorf("temporary-directory-env requires a local MCP server")
	}
	if len(cfg.TemporaryDirectoryEnv) > 16 {
		return fmt.Errorf("temporary-directory-env permits at most 16 variables")
	}
	seen := make(map[string]bool)
	for _, name := range cfg.TemporaryDirectoryEnv {
		if !validDirectoryEnvName(name) || blockedEnvVars[strings.ToUpper(name)] || seen[name] {
			return fmt.Errorf("invalid, blocked or duplicate temporary-directory-env name %q", name)
		}
		if _, exists := cfg.Environment[name]; exists {
			return fmt.Errorf("temporary-directory-env %q conflicts with environment", name)
		}
		seen[name] = true
	}
	return nil
}

func validDirectoryEnvName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, ch := range name {
		if ch != '_' && (ch < 'A' || ch > 'Z') && (ch < 'a' || ch > 'z') && (i == 0 || ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

// Each launched server owns one private root. Configuration and policy hashes
// retain the declaration, never these generated execution-time paths.
func localServerEnvironment(cfg MCPServerConfig) (env []string, root string, err error) {
	if err := validateTemporaryDirectoryEnv(cfg); err != nil {
		return nil, "", err
	}
	for name, value := range cfg.Environment {
		if !blockedEnvVars[strings.ToUpper(name)] {
			env = append(env, name+"="+value)
		}
	}
	if len(cfg.TemporaryDirectoryEnv) == 0 {
		return env, "", nil
	}
	root, err = os.MkdirTemp(os.TempDir(), "hufu-mcp-")
	if err != nil {
		return nil, "", err
	}
	for i, name := range cfg.TemporaryDirectoryEnv {
		path := filepath.Join(root, strconv.Itoa(i))
		if err := os.Mkdir(path, 0o700); err != nil {
			return nil, "", errors.Join(err, os.RemoveAll(root))
		}
		env = append(env, name+"="+path)
	}
	return env, root, nil
}

type localServerResources struct {
	root     string
	closed   bool
	closeErr error
}

// Caller holds m.mu. Close waits for the SDK-owned subprocess before deleting
// its outputs. An unconfirmed close retains them and reports their exact path.
func (m *MCPToolManager) closeClientLocked(cli *client.Client) error {
	resources := m.localServers[cli]
	if resources != nil && resources.closed {
		return resources.closeErr
	}
	err := cli.Close()
	if resources == nil {
		return err
	}
	resources.closed = true
	resources.closeErr = err
	root := resources.root
	_, exited := errors.AsType[*exec.ExitError](err)
	if err != nil && !exited {
		if root != "" {
			resources.closeErr = fmt.Errorf("MCP close unconfirmed; temporary directory retained at %s: %w", root, err)
		}
		return resources.closeErr
	}
	if cleanupErr := removeTemporaryRoot(root); cleanupErr != nil {
		resources.closeErr = errors.Join(err, fmt.Errorf("clean MCP temporary directory %s: %w", root, cleanupErr))
		return resources.closeErr
	}
	delete(m.localServers, cli)
	return err
}

func (m *MCPToolManager) closeRejectedClient(cli *client.Client) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeClientLocked(cli)
}

func removeTemporaryRoot(root string) error {
	if root == "" {
		return nil
	}
	return os.RemoveAll(root)
}
