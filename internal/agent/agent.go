package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/tools"
)

const DefaultProviderURL = "http://localhost:11434/v1"

const DefaultMaxSteps = 30
const DefaultCoordinatorMaxSteps = 20

type GenerationParams struct {
	Model       string
	Temperature string
	MaxTokens   string
	TopP        string
	TopK        string
}

type AgentDef struct {
	Name        string
	Description string
	Tools       string
	Role        string
	System      string
	Skills      string
	Timeout     int64
	MaxRetries  int
	MaxSteps    int
	Generation  GenerationParams
	ProviderURL string
}

type TeamConfig struct {
	Name          string
	Description   string
	MaxRounds     int
	MaxSteps      int
	WorkspaceDir  string
	Timeout       int64
	MaxRetries    int
	Generation    GenerationParams
	Skills        string
	SkillsExclude string
	ProviderURL   string
	ModelList     []config.ModelEntry
}

type OllamaProvider struct {
	provider fantasy.Provider
	baseURL  string
}

func NewOllamaProvider(baseURL string) (*OllamaProvider, error) {
	if baseURL == "" {
		baseURL = DefaultProviderURL
	}
	provider, err := openaicompat.New(
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey("ollama"),
		openaicompat.WithName("ollama"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create Ollama provider: %w", err)
	}
	return &OllamaProvider{provider: provider, baseURL: baseURL}, nil
}

func (p *OllamaProvider) LanguageModel(ctx context.Context, modelID string) (fantasy.LanguageModel, error) {
	model := modelID
	if strings.HasPrefix(model, "ollama/") {
		model = strings.TrimPrefix(model, "ollama/")
	}
	return p.provider.LanguageModel(ctx, model)
}

type AgentConfig struct {
	Def        *AgentDef
	TeamConfig *TeamConfig
	WorkDir    string
	MaxSteps   int
}

func resolveMaxSteps(agentSteps, teamSteps int) int {
	if agentSteps > 0 {
		return agentSteps
	}
	if teamSteps > 0 {
		return teamSteps
	}
	return DefaultMaxSteps
}

func CreateAgent(ctx context.Context, ollama *OllamaProvider, cfg AgentConfig, agentTools []fantasy.AgentTool) (fantasy.Agent, error) {
	modelStr := cfg.Def.Generation.Model
	if modelStr == "" {
		modelStr = cfg.TeamConfig.Generation.Model
	}
	if modelStr == "" {
		return nil, fmt.Errorf("no model specified for agent %q", cfg.Def.Name)
	}

	lm, err := ollama.LanguageModel(ctx, modelStr)
	if err != nil {
		return nil, fmt.Errorf("failed to create language model for %q: %w", cfg.Def.Name, err)
	}

	opts := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(cfg.Def.System),
		fantasy.WithTools(agentTools...),
	}

	if maxTokens := parseModelInt(cfg.Def.Generation.MaxTokens, cfg.TeamConfig.Generation.MaxTokens); maxTokens > 0 {
		opts = append(opts, fantasy.WithMaxOutputTokens(int64(maxTokens)))
	}
	if temp := parseModelFloat(cfg.Def.Generation.Temperature, cfg.TeamConfig.Generation.Temperature); temp >= 0 {
		opts = append(opts, fantasy.WithTemperature(temp))
	}
	if topP := parseModelFloat(cfg.Def.Generation.TopP, cfg.TeamConfig.Generation.TopP); topP >= 0 {
		opts = append(opts, fantasy.WithTopP(topP))
	}
	if topK := parseModelInt(cfg.Def.Generation.TopK, cfg.TeamConfig.Generation.TopK); topK > 0 {
		opts = append(opts, fantasy.WithTopK(int64(topK)))
	}

	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = resolveMaxSteps(cfg.Def.MaxSteps, cfg.TeamConfig.MaxSteps)
	}
	if maxSteps > 0 {
		opts = append(opts, fantasy.WithStopConditions(fantasy.StepCountIs(maxSteps)))
	}

	return fantasy.NewAgent(lm, opts...), nil
}

func RunAgent(ctx context.Context, agent fantasy.Agent, prompt string) (string, error) {
	result, err := agent.Generate(ctx, fantasy.AgentCall{
		Prompt: prompt,
	})
	if err != nil {
		return "", err
	}
	return result.Response.Content.Text(), nil
}

var alwaysIncludeTools = map[string]bool{
	"agent":        true,
	"todo":         true,
	"memory_save":  true,
	"memory_query": true,
}

func SelectTools(allTools []fantasy.AgentTool, toolNames string) []fantasy.AgentTool {
	if toolNames == "" || toolNames == "all" {
		return allTools
	}
	requested := map[string]bool{}
	for _, name := range strings.Split(toolNames, ",") {
		n := strings.TrimSpace(name)
		requested[n] = true
	}

	var selected []fantasy.AgentTool
	for _, t := range allTools {
		if requested[t.Info().Name] || alwaysIncludeTools[t.Info().Name] {
			selected = append(selected, t)
		} else if t.Info().Name == "view" && requested["read"] {
			selected = append(selected, t)
		} else if t.Info().Name == "glob" && requested["find"] {
			selected = append(selected, t)
		}
	}
	return selected
}

func BuildAllAgentTools(workDir string) []fantasy.AgentTool {
	return tools.AllTools(tools.WithWorkDir(workDir))
}

func parseModelInt(primary, fallback string) int {
	if primary != "" {
		if v, err := strconv.Atoi(primary); err == nil {
			return v
		}
	}
	if fallback != "" {
		if v, err := strconv.Atoi(fallback); err == nil {
			return v
		}
	}
	return -1
}

func parseModelFloat(primary, fallback string) float64 {
	if primary != "" {
		if v, err := strconv.ParseFloat(primary, 64); err == nil {
			return v
		}
	}
	if fallback != "" {
		if v, err := strconv.ParseFloat(fallback, 64); err == nil {
			return v
		}
	}
	return -1
}
