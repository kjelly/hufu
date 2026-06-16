package skill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const usageFileName = ".skill-usage.json"

// UsageStats records per-skill usage data.
type UsageStats struct {
	Name      string    `json:"name"`
	UsedCount int       `json:"used_count"`
	FirstUsed time.Time `json:"first_used"`
	LastUsed  time.Time `json:"last_used"`
	Agents    []string  `json:"agents,omitempty"`
}

// CleanOpts controls which drafts to clean.
type CleanOpts struct {
	OlderThan  time.Duration
	UnusedOnly bool
	DryRun     bool
}

// CleanResult reports what was (or would have been) deleted.
type CleanResult struct {
	Deleted []string
	Kept    []string
}

// LoadUsageStats reads the per-workspace usage stats file.
// Returns an empty map if the file doesn't exist.
func LoadUsageStats(workspaceDir string) (map[string]UsageStats, error) {
	path := filepath.Join(workspaceDir, usageFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]UsageStats), nil
		}
		return nil, fmt.Errorf("read usage stats: %w", err)
	}
	stats := make(map[string]UsageStats)
	if err := json.Unmarshal(data, &stats); err != nil {
		return nil, fmt.Errorf("parse usage stats: %w", err)
	}
	return stats, nil
}

// SaveUsageStats writes the usage stats to disk atomically.
func SaveUsageStats(workspaceDir string, stats map[string]UsageStats) error {
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal usage stats: %w", err)
	}
	path := filepath.Join(workspaceDir, usageFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write usage stats: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename usage stats: %w", err)
	}
	return nil
}

// RecordUsage updates the usage stats for a skill.
func RecordUsage(workspaceDir, skillName, agentName string) error {
	stats, err := LoadUsageStats(workspaceDir)
	if err != nil {
		return err
	}
	now := time.Now()
	entry, ok := stats[skillName]
	if !ok {
		entry = UsageStats{
			Name:      skillName,
			FirstUsed: now,
			Agents:    []string{},
		}
	}
	entry.UsedCount++
	entry.LastUsed = now
	found := false
	for _, a := range entry.Agents {
		if a == agentName {
			found = true
			break
		}
	}
	if !found {
		entry.Agents = append(entry.Agents, agentName)
	}
	stats[skillName] = entry
	return SaveUsageStats(workspaceDir, stats)
}

// PromoteDraft moves a draft from <skillsDir>/drafts/<name>/ to
// <skillsDir>/<name>/, stripping the "draft-" prefix from the directory
// name. Returns the new SKILL.md path.
func PromoteDraft(skillsDir, draftName string) (string, error) {
	srcDir := filepath.Join(skillsDir, "drafts", draftName)
	if _, err := os.Stat(srcDir); err != nil {
		return "", fmt.Errorf("draft not found: %s", draftName)
	}
	newName := strings.TrimPrefix(draftName, "draft-")
	if newName == "" || newName == draftName {
		newName = draftName
	}
	dstDir := filepath.Join(skillsDir, newName)
	if _, err := os.Stat(dstDir); err == nil {
		return "", fmt.Errorf("destination already exists: %s", dstDir)
	}
	if err := os.Rename(srcDir, dstDir); err != nil {
		return "", fmt.Errorf("rename draft: %w", err)
	}
	return filepath.Join(dstDir, "SKILL.md"), nil
}

// CleanDrafts deletes draft skills matching the given options.
func CleanDrafts(skillsDir string, opts CleanOpts) (CleanResult, error) {
	draftsRoot := filepath.Join(skillsDir, "drafts")
	entries, err := os.ReadDir(draftsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return CleanResult{}, nil
		}
		return CleanResult{}, fmt.Errorf("read drafts dir: %w", err)
	}

	var usage map[string]UsageStats
	if opts.UnusedOnly {
		workspaceDir := filepath.Dir(skillsDir)
		usage, _ = LoadUsageStats(workspaceDir)
		if usage == nil {
			usage = make(map[string]UsageStats)
		}
	}

	now := time.Now()
	var result CleanResult
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		skillPath := filepath.Join(draftsRoot, name, "SKILL.md")
		def := parseSkillFile(skillPath)
		if def == nil {
			continue
		}
		if opts.OlderThan > 0 {
			age := now.Sub(def.CreatedAt)
			if age < opts.OlderThan {
				result.Kept = append(result.Kept, name)
				continue
			}
		}
		if opts.UnusedOnly {
			stats, used := usage[name]
			if used && stats.UsedCount > 0 {
				result.Kept = append(result.Kept, name)
				continue
			}
		}
		if !opts.DryRun {
			if err := os.RemoveAll(filepath.Join(draftsRoot, name)); err != nil {
				return result, fmt.Errorf("delete draft %s: %w", name, err)
			}
		}
		result.Deleted = append(result.Deleted, name)
	}
	return result, nil
}
