package team

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/mcp"
	"github.com/anomalyco/hufu/internal/notify"
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

type agentFrontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Role        string   `yaml:"role"`
	Tools       string   `yaml:"tools"`
	Skills      string   `yaml:"skills"`
	Guard       []string `yaml:"guard"`
	Model       string   `yaml:"model"`
	Temperature string   `yaml:"temperature"`
	MaxTokens   string   `yaml:"max-tokens"`
	TopP        string   `yaml:"top-p"`
	TopK        string   `yaml:"top-k"`
	Timeout     int64    `yaml:"timeout"`
	MaxRetries  int      `yaml:"max-retries"`
	MaxSteps    int      `yaml:"max-steps"`
	ProviderURL string   `yaml:"provider-url"`
}

type teamConfigYAML struct {
	Name          string              `yaml:"name"`
	Description   string              `yaml:"description"`
	MaxRounds     int                 `yaml:"max-rounds"`
	MaxSteps      int                 `yaml:"max-steps"`
	Workspace     string              `yaml:"workspace"`
	Timeout       int64               `yaml:"timeout"`
	MaxRetries    int                 `yaml:"max-retries"`
	Model         string              `yaml:"model"`
	Temperature   string              `yaml:"temperature"`
	MaxTokens     string              `yaml:"max-tokens"`
	TopP          string              `yaml:"top-p"`
	TopK          string              `yaml:"top-k"`
	Skills        string              `yaml:"skills"`
	SkillsExclude string              `yaml:"skills-exclude"`
	ProviderURL   string              `yaml:"provider-url"`
	ModelList     []config.ModelEntry `yaml:"model-list"`
	SidecarModel  string              `yaml:"sidecar-model"`
	GuardModel    string              `yaml:"guard-model"`
	Notify        notify.NotifyConfig `yaml:"notify"`
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

func agentFrontmatterFromSimple(m map[string]string) agentFrontmatter {
	var fm agentFrontmatter
	fm.Name = m["name"]
	fm.Description = m["description"]
	fm.Role = m["role"]
	fm.Tools = m["tools"]
	fm.Skills = m["skills"]
	fm.Model = m["model"]
	fm.Temperature = m["temperature"]
	fm.MaxTokens = m["max-tokens"]
	fm.TopP = m["top-p"]
	fm.TopK = m["top-k"]
	fm.ProviderURL = m["provider-url"]
	if v := m["timeout"]; v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			fm.Timeout = n
		}
	}
	if v := m["max-retries"]; v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			fm.MaxRetries = n
		}
	}
	if v := m["max-steps"]; v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			fm.MaxSteps = n
		}
	}
	return fm
}

func parseAgentFile(path string, vars map[string]string) (*agent.AgentDef, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read agent file %s: %w", path, err)
	}
	text := string(raw)

	templated, err := applyTemplate(text, filepath.Base(path), vars)
	if err != nil {
		return nil, fmt.Errorf("template error in agent file %s: %w", path, err)
	}
	text = templated

	if !strings.HasPrefix(text, "---\n") {
		return nil, nil
	}
	rest := text[4:]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		fmt.Fprintf(os.Stderr, "warning: malformed frontmatter in %s (missing closing ---)\n", path)
		return nil, nil
	}

	var fm agentFrontmatter
	if err := yaml.Unmarshal([]byte(rest[:idx]), &fm); err != nil {
		fmt.Fprintf(os.Stderr, "warning: YAML parse failed in %s, using fallback: %v\n", path, err)
		fm = agentFrontmatterFromSimple(parseSimpleYAML(rest[:idx]))
	}
	body := strings.TrimSpace(rest[idx+5:])

	if fm.Name == "" {
		fmt.Fprintf(os.Stderr, "warning: agent file %s has no 'name' in frontmatter\n", path)
		return nil, nil
	}

	role := fm.Role
	if role == "" {
		role = "worker"
	}

	def := &agent.AgentDef{
		Name:        fm.Name,
		Description: fm.Description,
		Tools:       fm.Tools,
		Role:        role,
		System:      body,
		Skills:      fm.Skills,
		Guard:       fm.Guard,
		MaxRetries:  -1,
		MaxSteps:    fm.MaxSteps,
		Generation: agent.GenerationParams{
			Model:       fm.Model,
			Temperature: fm.Temperature,
			MaxTokens:   fm.MaxTokens,
			TopP:        fm.TopP,
			TopK:        fm.TopK,
		},
		ProviderURL: fm.ProviderURL,
	}
	if fm.Timeout > 0 {
		def.Timeout = fm.Timeout
	}
	if fm.MaxRetries >= 0 {
		def.MaxRetries = fm.MaxRetries
	}
	return def, nil
}

func parseTeamYML(teamDir string, vars map[string]string) (agent.TeamConfig, error) {
	cfg := agent.TeamConfig{
		MaxRounds:    10,
		WorkspaceDir: "workspace",
		Timeout:      600,
		MaxRetries:   2,
	}

	var data []byte
	var dataFilename string
	var found bool
	for _, name := range []string{"team.yml", "team.yaml"} {
		d, err := os.ReadFile(filepath.Join(teamDir, name))
		if err == nil {
			data = d
			dataFilename = name
			found = true
			break
		}
	}
	if !found {
		return cfg, fmt.Errorf("team.yml or team.yaml not found in %s", teamDir)
	}

	text := string(data)
	templated, err := applyTemplate(text, dataFilename, vars)
	if err != nil {
		return cfg, fmt.Errorf("template error in team config: %w", err)
	}
	data = []byte(templated)

	var yc teamConfigYAML
	if err := yaml.Unmarshal(data, &yc); err != nil {
		return cfg, fmt.Errorf("failed to parse team config: %w", err)
	}

	if yc.Name != "" {
		cfg.Name = yc.Name
	}
	if yc.Description != "" {
		cfg.Description = yc.Description
	}
	if yc.MaxRounds > 0 {
		cfg.MaxRounds = yc.MaxRounds
	}
	if yc.Workspace != "" {
		cfg.WorkspaceDir = yc.Workspace
	}
	if yc.Timeout > 0 {
		cfg.Timeout = yc.Timeout
	}
	if yc.MaxRetries >= 0 {
		cfg.MaxRetries = yc.MaxRetries
	}
	if yc.MaxSteps > 0 {
		cfg.MaxSteps = yc.MaxSteps
	}
	cfg.Generation = agent.GenerationParams{
		Model:       yc.Model,
		Temperature: yc.Temperature,
		MaxTokens:   yc.MaxTokens,
		TopP:        yc.TopP,
		TopK:        yc.TopK,
	}
	if yc.Skills != "" {
		cfg.Skills = yc.Skills
	}
	if yc.SkillsExclude != "" {
		cfg.SkillsExclude = yc.SkillsExclude
	}
	if yc.ProviderURL != "" {
		cfg.ProviderURL = yc.ProviderURL
	}
	if len(yc.ModelList) > 0 {
		cfg.ModelList = yc.ModelList
	}
	if yc.SidecarModel != "" {
		cfg.SidecarModel = yc.SidecarModel
	}
	if yc.GuardModel != "" {
		cfg.GuardModel = yc.GuardModel
	}
	if yc.Notify.Enabled() {
		cfg.Notify = yc.Notify
	}

	return cfg, nil
}

func LoadTeam(teamDir string, vars map[string]string) (*TeamSession, error) {
	absDir, err := filepath.Abs(teamDir)
	if err != nil {
		return nil, fmt.Errorf("invalid team directory: %w", err)
	}

	cfg, err := parseTeamYML(absDir, vars)
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
		if entry.Type()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || resolved == path {
				continue
			}
		}
		def, err := parseAgentFile(path, vars)
		if err != nil {
			return nil, err
		}
		if def == nil {
			continue
		}
		fileAlias := strings.TrimSuffix(entry.Name(), ".md")
		def.FileAlias = fileAlias
		fileKey := strings.ToLower(fileAlias)
		nameKey := strings.ToLower(def.Name)
		if _, exists := session.Agents[fileKey]; !exists {
			session.Agents[fileKey] = def
		}
		if nameKey != fileKey {
			if _, exists := session.Agents[nameKey]; !exists {
				session.Agents[nameKey] = def
			}
		}
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
