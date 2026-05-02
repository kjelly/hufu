package team

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type TeamRegistry struct {
	searchPaths []string
	teams       map[string]string
}

func NewTeamRegistry(searchPaths []string) *TeamRegistry {
	expanded := make([]string, 0, len(searchPaths))
	for _, p := range searchPaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "~") {
			home, err := os.UserHomeDir()
			if err != nil {
				continue
			}
			p = filepath.Join(home, p[1:])
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		expanded = append(expanded, abs)
	}
	return &TeamRegistry{
		searchPaths: expanded,
		teams:       make(map[string]string),
	}
}

func DefaultSearchPaths() []string {
	home, _ := os.UserHomeDir()
	paths := []string{}
	cwd, err := os.Getwd()
	if err == nil {
		paths = append(paths, filepath.Join(cwd, ".agent-teams"))
	}
	if home != "" {
		paths = append(paths, filepath.Join(home, ".agent-teams"))
	}
	return paths
}

func (r *TeamRegistry) Discover() error {
	for _, dir := range r.searchPaths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("failed to read search path %s: %w", dir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := strings.ToLower(entry.Name())
			subDir := filepath.Join(dir, entry.Name())
			if r.hasTeamFile(subDir) {
				if _, exists := r.teams[name]; !exists {
					r.teams[name] = subDir
				}
			}
		}
	}
	return nil
}

func (r *TeamRegistry) hasTeamFile(dir string) bool {
	for _, name := range []string{"team.yml", "team.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

func (r *TeamRegistry) Resolve(name string) (string, error) {
	nameLower := strings.ToLower(name)
	if dir, ok := r.teams[nameLower]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("team %q not found", name)
}

func (r *TeamRegistry) HasTeam(name string) bool {
	_, ok := r.teams[strings.ToLower(name)]
	return ok
}

func (r *TeamRegistry) ListTeams() []string {
	var names []string
	for name := range r.teams {
		names = append(names, name)
	}
	return names
}

func (r *TeamRegistry) TeamCount() int {
	return len(r.teams)
}

func (r *TeamRegistry) SearchPaths() []string {
	return r.searchPaths
}

func (r *TeamRegistry) TeamDirs() []string {
	dirs := make([]string, 0, len(r.teams))
	for _, dir := range r.teams {
		dirs = append(dirs, dir)
	}
	return dirs
}
