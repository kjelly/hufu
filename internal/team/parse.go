package team

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/skill"
)

type TeamSession struct {
	Config     agent.TeamConfig
	Dir        string
	Workspace  string
	Agents     map[string]*agent.AgentDef
	MCPServers map[string]mcp.MCPServerConfig
	Skills     []*skill.SkillDef
}

func parseSimpleYAML(data string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, ":")
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		if len(val) >= 2 &&
			((val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		result[key] = val
	}
	return result
}

func parseAgentFile(path string) *agent.AgentDef {
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to read agent file %s: %v\n", path, err)
		return nil
	}
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") {
		return nil
	}
	rest := text[4:]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		fmt.Fprintf(os.Stderr, "warning: malformed frontmatter in %s (missing closing ---)\n", path)
		return nil
	}
	fm := parseSimpleYAML(rest[:idx])
	body := strings.TrimSpace(rest[idx+5:])

	if fm["name"] == "" {
		fmt.Fprintf(os.Stderr, "warning: agent file %s has no 'name' in frontmatter\n", path)
		return nil
	}

	role := fm["role"]
	if role == "" {
		role = "worker"
	}

	def := &agent.AgentDef{
		Name:        fm["name"],
		Description: fm["description"],
		Tools:       fm["tools"],
		Role:        role,
		System:      body,
		Skills:      fm["skills"],
		MaxRetries:  -1,
		Generation: agent.GenerationParams{
			Model:       fm["model"],
			Temperature: fm["temperature"],
			MaxTokens:   fm["max-tokens"],
			TopP:        fm["top-p"],
			TopK:        fm["top-k"],
		},
		ProviderURL: fm["provider-url"],
	}
	if v := fm["timeout"]; v != "" {
		var seconds int64
		if _, err := fmt.Sscanf(v, "%d", &seconds); err == nil && seconds > 0 {
			def.Timeout = seconds
		}
	}
	if v := fm["max-retries"]; v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			def.MaxRetries = n
		}
	}
	return def
}

func parseTeamYML(teamDir string) (agent.TeamConfig, error) {
	cfg := agent.TeamConfig{
		MaxRounds:    10,
		WorkspaceDir: "workspace",
		Timeout:      600,
		MaxRetries:   2,
	}

	var data []byte
	var found bool
	for _, name := range []string{"team.yml", "team.yaml"} {
		d, err := os.ReadFile(filepath.Join(teamDir, name))
		if err == nil {
			data = d
			found = true
			break
		}
	}
	if !found {
		return cfg, fmt.Errorf("team.yml or team.yaml not found in %s", teamDir)
	}

	fm := parseSimpleYAML(string(data))
	if v := fm["name"]; v != "" {
		cfg.Name = v
	}
	if v := fm["description"]; v != "" {
		cfg.Description = v
	}
	if v := fm["max-rounds"]; v != "" {
		fmt.Sscanf(v, "%d", &cfg.MaxRounds)
	}
	if v := fm["workspace"]; v != "" {
		cfg.WorkspaceDir = v
	}
	if v := fm["timeout"]; v != "" {
		var seconds int64
		if _, err := fmt.Sscanf(v, "%d", &seconds); err == nil && seconds > 0 {
			cfg.Timeout = seconds
		}
	}
	if v := fm["max-retries"]; v != "" {
		fmt.Sscanf(v, "%d", &cfg.MaxRetries)
	}
	cfg.Generation = agent.GenerationParams{
		Model:       fm["model"],
		Temperature: fm["temperature"],
		MaxTokens:   fm["max-tokens"],
		TopP:        fm["top-p"],
		TopK:        fm["top-k"],
	}
	if v := fm["skills"]; v != "" {
		cfg.Skills = v
	}
	if v := fm["skills-exclude"]; v != "" {
		cfg.SkillsExclude = v
	}
	if v := fm["provider-url"]; v != "" {
		cfg.ProviderURL = v
	}

	return cfg, nil
}

func LoadTeam(teamDir string) (*TeamSession, error) {
	absDir, err := filepath.Abs(teamDir)
	if err != nil {
		return nil, fmt.Errorf("invalid team directory: %w", err)
	}

	cfg, err := parseTeamYML(absDir)
	if err != nil {
		return nil, err
	}
	if cfg.Name == "" {
		cfg.Name = filepath.Base(absDir)
	}

	var workspace string
	if filepath.IsAbs(cfg.WorkspaceDir) {
		workspace = cfg.WorkspaceDir
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get working directory: %w", err)
		}
		workspace = filepath.Join(cwd, cfg.WorkspaceDir)
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create workspace: %w", err)
	}

	session := &TeamSession{
		Config:     cfg,
		Dir:        absDir,
		Workspace:  workspace,
		Agents:     make(map[string]*agent.AgentDef),
		MCPServers: make(map[string]mcp.MCPServerConfig),
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read team directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(absDir, entry.Name())
		def := parseAgentFile(path)
		if def == nil {
			continue
		}
		session.Agents[strings.ToLower(def.Name)] = def
	}

	if len(session.Agents) == 0 {
		return nil, fmt.Errorf("no valid agent .md files found in %s", absDir)
	}

	skillDirs := []string{
		filepath.Join(absDir, ".agents", "skills"),
		filepath.Join(os.Getenv("HOME"), ".agents", "skills"),
	}

	allSkills := skill.DiscoverSkills(skillDirs)

	includeSkills := skill.ParseSkillList(session.Config.Skills)
	excludeSkills := skill.ParseSkillList(session.Config.SkillsExclude)
	session.Skills = skill.FilterSkills(allSkills, includeSkills, excludeSkills)

	return session, nil
}
