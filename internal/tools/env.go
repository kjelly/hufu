package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func parseDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vars := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eqIdx := strings.IndexByte(line, '=')
		if eqIdx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eqIdx])
		val := strings.TrimSpace(line[eqIdx+1:])
		val = unquote(val)
		vars[key] = val
	}
	return vars, scanner.Err()
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func loadDirenvEnv(workDir string) (map[string]string, error) {
	direnvBin, err := exec.LookPath("direnv")
	if err != nil {
		return nil, fmt.Errorf("direnv not found in PATH")
	}

	allowCmd := exec.Command(direnvBin, "allow", workDir)
	allowCmd.Dir = workDir
	out, err := allowCmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("direnv allow failed: %v\n%s", err, string(out))
	}

	exportCmd := exec.Command(direnvBin, "export", "json", workDir)
	exportCmd.Dir = workDir
	out, err = exportCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("direnv export failed: %w", err)
	}

	var envMap map[string]interface{}
	if err := json.Unmarshal(out, &envMap); err != nil {
		return nil, fmt.Errorf("failed to parse direnv JSON: %w", err)
	}

	vars := make(map[string]string, len(envMap))
	for k, v := range envMap {
		vars[k] = fmt.Sprint(v)
	}
	return vars, nil
}

func envMapToSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func LoadProjectEnv(workDir string, useDirenv bool) ([]string, error) {
	vars := make(map[string]string)

	dotEnvPath := filepath.Join(workDir, ".env")
	if dotEnvVars, err := parseDotEnv(dotEnvPath); err == nil {
		for k, v := range dotEnvVars {
			vars[k] = v
		}
	}

	if useDirenv {
		if direnvVars, err := loadDirenvEnv(workDir); err == nil {
			for k, v := range direnvVars {
				vars[k] = v
			}
		} else {
			if len(vars) == 0 {
				return nil, err
			}
			fmt.Fprintf(os.Stderr, "warning: direnv failed (%v); using .env fallback only\n", err)
		}
	}

	return envMapToSlice(vars), nil
}
