package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
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

type SkillSummary struct {
	Name        string
	Description string
}

var jsonCodeBlockRe = regexp.MustCompile("(?s)```(?:json)?\\s*\\n?(.*?)\\n?```")

func (s *Sidecar) MatchSkills(ctx context.Context, prompt string, skills []SkillSummary) ([]string, error) {
	if s == nil || s.agent == nil {
		return nil, fmt.Errorf("sidecar not initialized")
	}
	if len(skills) == 0 {
		return nil, nil
	}

	var skillList strings.Builder
	for i, sk := range skills {
		desc := sk.Description
		if utf8.RuneCountInString(desc) > 200 {
			runes := []rune(desc)
			desc = string(runes[:200]) + "..."
		}
		fmt.Fprintf(&skillList, "%d. %s: %s\n", i+1, sk.Name, desc)
	}

	matchPrompt := fmt.Sprintf(`Given the user's task below, identify ALL skills from the list that are relevant or potentially helpful for completing the task. A task can require multiple skills — return every skill name that could assist with any part of the task.

Return ONLY a JSON array of skill name strings (e.g., ["skill-a", "skill-b"]). Return multiple names when multiple skills are relevant. If none are relevant, return [].

Available skills:
%s

User task: %s`, skillList.String(), prompt)

	result, err := s.generate(ctx, matchPrompt)
	if err != nil {
		return nil, fmt.Errorf("sidecar match skills generate failed: %w", err)
	}

	result = strings.TrimSpace(result)

	extracted := jsonCodeBlockRe.FindStringSubmatch(result)
	if len(extracted) >= 2 {
		result = strings.TrimSpace(extracted[1])
	}

	var names []string
	if err := json.Unmarshal([]byte(result), &names); err != nil {
		return nil, fmt.Errorf("sidecar match skills: failed to parse JSON response %q: %w", result, err)
	}

	validMap := map[string]bool{}
	for _, sk := range skills {
		validMap[strings.ToLower(sk.Name)] = true
	}
	var filtered []string
	for _, name := range names {
		if validMap[strings.ToLower(strings.TrimSpace(name))] {
			filtered = append(filtered, strings.TrimSpace(name))
		}
	}
	return filtered, nil
}

// SimilarTask checks whether newTask is semantically equivalent to any task in
// pastTasks. Returns the 0-based index of the matching past task, or -1 if none
// match. The caller is responsible for truncating long task strings if needed.
func (s *Sidecar) SimilarTask(ctx context.Context, newTask string, pastTasks []string) (int, error) {
	if s == nil || s.agent == nil {
		return -1, fmt.Errorf("sidecar not initialized")
	}
	if len(pastTasks) == 0 {
		return -1, nil
	}

	var list strings.Builder
	for i, t := range pastTasks {
		preview := t
		if utf8.RuneCountInString(preview) > 120 {
			preview = string([]rune(preview)[:120]) + "..."
		}
		fmt.Fprintf(&list, "%d. %s\n", i+1, preview)
	}

	prompt := fmt.Sprintf(`You are a task deduplication classifier. Determine whether the NEW TASK is semantically equivalent to any task in PAST TASKS — meaning it asks for essentially the same work and would produce the same result.

Return ONLY a JSON object: {"match": <1-based index of matching past task, or 0 if none>}

PAST TASKS:
%s
NEW TASK: %s`, list.String(), newTask)

	result, err := s.generate(ctx, prompt)
	if err != nil {
		return -1, fmt.Errorf("sidecar similar task generate failed: %w", err)
	}
	result = strings.TrimSpace(result)

	extracted := jsonCodeBlockRe.FindStringSubmatch(result)
	if len(extracted) >= 2 {
		result = strings.TrimSpace(extracted[1])
	}

	var resp struct {
		Match int `json:"match"`
	}
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		return -1, fmt.Errorf("sidecar similar task: failed to parse JSON response %q: %w", result, err)
	}
	if resp.Match < 1 || resp.Match > len(pastTasks) {
		return -1, nil
	}
	return resp.Match - 1, nil
}
