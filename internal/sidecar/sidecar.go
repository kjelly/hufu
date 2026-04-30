package sidecar

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
)

const (
	summarizeMaxChars   = 4000
	compactMaxChars     = 4000
	executeMaxChars     = 8000
	sidecarMaxSteps     = 1
	sidecarSystemPrompt = "You are a concise assistant. Follow the user's instruction exactly. Be brief and precise. Do not add unnecessary commentary."
)

type Sidecar struct {
	mu       sync.Mutex
	agent    fantasy.Agent
	provider *agent.OllamaProvider
	modelID  string
}

func NewSidecar(ctx context.Context, provider *agent.OllamaProvider, modelID string) (*Sidecar, error) {
	if modelID == "" {
		return nil, fmt.Errorf("sidecar model ID is empty")
	}
	s := &Sidecar{
		provider: provider,
		modelID:  modelID,
	}
	if err := s.init(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize sidecar: %w", err)
	}
	return s, nil
}

func (s *Sidecar) init(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agent != nil {
		return nil
	}
	lm, err := s.provider.LanguageModel(ctx, s.modelID)
	if err != nil {
		return fmt.Errorf("failed to create sidecar language model for %q: %w", s.modelID, err)
	}
	s.agent = fantasy.NewAgent(lm,
		fantasy.WithSystemPrompt(sidecarSystemPrompt),
		fantasy.WithStopConditions(fantasy.StepCountIs(sidecarMaxSteps)),
	)
	return nil
}

func (s *Sidecar) ModelID() string {
	return s.modelID
}

func (s *Sidecar) generate(ctx context.Context, prompt string) (string, error) {
	s.mu.Lock()
	a := s.agent
	s.mu.Unlock()
	if a == nil {
		return "", fmt.Errorf("sidecar agent not initialized")
	}
	result, err := a.Generate(ctx, fantasy.AgentCall{Prompt: prompt})
	if err != nil {
		return "", err
	}
	return result.Response.Content.Text(), nil
}

func (s *Sidecar) Summarize(ctx context.Context, text string, maxChars int) (string, error) {
	if s == nil || s.agent == nil {
		return text, nil
	}
	if maxChars <= 0 {
		maxChars = summarizeMaxChars
	}
	if utf8.RuneCountInString(text) <= maxChars/2 {
		return text, nil
	}
	prompt := fmt.Sprintf(`Summarize the following text in under %d characters. Preserve all key information, facts, and conclusions. Output ONLY the summary, no meta-commentary.

---
%s`, maxChars, text)
	summary, err := s.generate(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar summarize generate failed: %v\n", err)
		return text, nil
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return text, nil
	}
	return summary, nil
}

func (s *Sidecar) Compact(ctx context.Context, text string, instruction string) (string, error) {
	if s == nil || s.agent == nil {
		return text, nil
	}
	if instruction == "" {
		instruction = "Condense the following text while preserving all key information."
	}
	prompt := fmt.Sprintf(`%s Output ONLY the result, no meta-commentary.

---
%s`, instruction, text)
	result, err := s.generate(ctx, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar compact generate failed: %v\n", err)
		return text, nil
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return text, nil
	}
	return result, nil
}

func (s *Sidecar) Execute(ctx context.Context, task string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("sidecar not configured")
	}
	runes := []rune(task)
	if len(runes) > executeMaxChars {
		task = string(runes[:executeMaxChars]) + "\n...(truncated)"
	}
	result, err := s.generate(ctx, task)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result), nil
}
