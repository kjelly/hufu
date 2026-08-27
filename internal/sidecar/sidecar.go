package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

const (
	summarizeMaxChars   = 4000
	compactMaxChars     = 4000
	executeMaxChars     = 8000
	sidecarMaxSteps     = 1
	sidecarSystemPrompt = "You are a concise assistant. Follow the user's instruction exactly. Be brief and precise. Do not add unnecessary commentary."
)

// Profile bounds the generation shape for a category of sidecar calls.
// Sidecar backs a wide range of very different jobs — from one-line JSON
// route classification to multi-paragraph structured summarization — through
// a single underlying agent that otherwise has no generation limits of its
// own. Without a profile, a reasoning-capable local model (MiniMax M2.7,
// Gemma 4) has no reason not to "think" at length before answering a trivial
// classification call whose entire output is a few words of JSON.
type Profile struct {
	MaxOutputTokens int64
	Temperature     float64
	ReasoningEffort string
}

type purposeContextKey struct{}

// WithPurpose labels a sidecar call with its narrow review/classification
// contract so the coordinator can build a purpose-specific request/manifest.
func WithPurpose(ctx context.Context, purpose string) context.Context {
	return context.WithValue(ctx, purposeContextKey{}, strings.TrimSpace(purpose))
}

var (
	// ClassifierProfile is for short structured-output decisions: route,
	// skill, and team matching, guard review, dedup, and other calls whose
	// entire output is a small JSON object or a single name/value.
	ClassifierProfile = Profile{MaxOutputTokens: 512, Temperature: 0, ReasoningEffort: "none"}
	// CompactorProfile is for summarization/compaction, which needs enough
	// headroom to reproduce a condensed version of its input.
	CompactorProfile = Profile{MaxOutputTokens: 4096, Temperature: 0.1, ReasoningEffort: "low"}
	// JudgeProfile is for picking the best of several full candidate
	// outputs, which needs more than classifier headroom for its reasoning
	// and any grafted content, but nowhere near a full worker's budget.
	JudgeProfile = Profile{MaxOutputTokens: 1024, Temperature: 0, ReasoningEffort: "medium"}
)

// apply layers profile onto call as per-call overrides, which take
// precedence over the sidecar agent's own (unset) construction-time
// defaults — see fantasy.Agent.prepareCall.
func (p Profile) apply(call *fantasy.AgentCall) {
	maxTokens := p.MaxOutputTokens
	call.MaxOutputTokens = &maxTokens
	temp := p.Temperature
	call.Temperature = &temp
	if agent.ValidReasoningEfforts[p.ReasoningEffort] {
		call.ProviderOptions = openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{
			ReasoningEffort: new(openai.ReasoningEffort(p.ReasoningEffort)),
		})
	}
}

type Sidecar struct {
	mu             sync.Mutex
	agent          fantasy.Agent
	provider       *agent.OpenAICompatibleProvider
	modelID        string
	usageObserver  func(*fantasy.AgentResult)
	promptPreparer func(context.Context, string, string) (string, error)
}

// SetPromptPreparer installs the coordinator's purpose-aware context
// compiler/manifest boundary. Every sidecar model call crosses this single
// chokepoint, including guard, judge, skill matching, and compaction APIs.
func (s *Sidecar) SetPromptPreparer(preparer func(context.Context, string, string) (string, error)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.promptPreparer = preparer
	s.mu.Unlock()
}

func profilePurpose(profile Profile) string {
	switch profile {
	case JudgeProfile:
		return "judge"
	case CompactorProfile:
		return "compactor"
	default:
		return "classifier"
	}
}

func NewSidecar(ctx context.Context, provider *agent.OpenAICompatibleProvider, modelID string) (*Sidecar, error) {
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
		// Coordinator-owned budgets and recovery policy account for every
		// sidecar call. Disable Fantasy's transport retries for the same reason
		// as worker agents: its default exponential backoff can outlive the
		// enclosing Hufu deadline and hides attempts from that accounting.
		fantasy.WithMaxRetries(0),
	)
	return nil
}

func (s *Sidecar) ModelID() string {
	return s.modelID
}

// SetUsageObserver installs an optional callback for the usage of every
// generated sidecar response. The coordinator uses this to include guard,
// judge, skill, and other sidecar calls in its no-progress token budget.
func (s *Sidecar) SetUsageObserver(observer func(*fantasy.AgentResult)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.usageObserver = observer
	s.mu.Unlock()
}

func (s *Sidecar) generate(ctx context.Context, prompt string, profile Profile) (string, error) {
	return s.generateWithOptions(ctx, prompt, profile, sidecarInvocationOptions{
		observeUsage:  true,
		preparePrompt: true,
	})
}

// sidecarInvocationOptions are scoped to one generation call. In particular,
// transient projections can bypass durable prompt preparation and usage
// observation without changing the shared Sidecar hooks used by concurrent
// durable calls.
type sidecarInvocationOptions struct {
	observeUsage  bool
	preparePrompt bool
}

func (s *Sidecar) generateWithOptions(ctx context.Context, prompt string, profile Profile, options sidecarInvocationOptions) (string, error) {
	s.mu.Lock()
	a := s.agent
	preparer := s.promptPreparer
	s.mu.Unlock()
	if a == nil {
		return "", fmt.Errorf("sidecar agent not initialized")
	}
	if options.preparePrompt && preparer != nil {
		var err error
		purpose, _ := ctx.Value(purposeContextKey{}).(string)
		if purpose == "" {
			purpose = profilePurpose(profile)
		}
		prompt, err = preparer(ctx, purpose, prompt)
		if err != nil {
			return "", fmt.Errorf("prepare sidecar context: %w", err)
		}
	}
	call := fantasy.AgentCall{Prompt: prompt}
	profile.apply(&call)
	result, err := a.Generate(ctx, call)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	observer := s.usageObserver
	s.mu.Unlock()
	if options.observeUsage && observer != nil && result != nil {
		observer(result)
	}
	if result == nil {
		return "", fmt.Errorf("sidecar agent returned no result")
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
	summary, err := s.generate(ctx, prompt, CompactorProfile)
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
	result, err := s.generate(ctx, prompt, CompactorProfile)
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

// Execute runs a generic sidecar task under ClassifierProfile. Every current
// caller's expected output is a short name/value or small JSON object; use
// ExecuteProfile directly for a call that genuinely needs a larger budget
// (e.g. judging full candidate outputs).
func (s *Sidecar) Execute(ctx context.Context, task string) (string, error) {
	return s.ExecuteProfile(ctx, task, ClassifierProfile)
}

// ExecuteProfile is Execute with an explicit generation profile.
func (s *Sidecar) ExecuteProfile(ctx context.Context, task string, profile Profile) (string, error) {
	if s == nil {
		return "", fmt.Errorf("sidecar not configured")
	}
	runes := []rune(task)
	if len(runes) > executeMaxChars {
		task = string(runes[:executeMaxChars]) + "\n...(truncated)"
	}
	result, err := s.generate(ctx, task, profile)
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

	result, err := s.generate(ctx, matchPrompt, ClassifierProfile)
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

// TeamSummary is a candidate team for auto-selection.
type TeamSummary struct {
	Name        string
	Description string
}

// MatchTeam asks the sidecar to pick the single most suitable team for the
// user's prompt from the candidates. It returns the chosen team name (matched
// case-insensitively against the candidates) or "" if the model declines or
// returns something unrecognized — callers should then fall back to a
// deterministic heuristic.
func (s *Sidecar) MatchTeam(ctx context.Context, prompt string, teams []TeamSummary) (string, error) {
	if s == nil || s.agent == nil {
		return "", fmt.Errorf("sidecar not initialized")
	}
	if len(teams) == 0 {
		return "", nil
	}

	var teamList strings.Builder
	for i, t := range teams {
		desc := t.Description
		if utf8.RuneCountInString(desc) > 300 {
			runes := []rune(desc)
			desc = string(runes[:300]) + "..."
		}
		fmt.Fprintf(&teamList, "%d. %s: %s\n", i+1, t.Name, desc)
	}

	matchPrompt := fmt.Sprintf(`Choose the single team best suited to accomplish the user's task from the list below.

Return ONLY the exact team name as a JSON string (e.g. "dev-team"). If none clearly fit, return "".

Available teams:
%s
User task: %s`, teamList.String(), prompt)

	result, err := s.generate(ctx, matchPrompt, ClassifierProfile)
	if err != nil {
		return "", fmt.Errorf("sidecar match team generate failed: %w", err)
	}
	result = strings.TrimSpace(result)

	if extracted := jsonCodeBlockRe.FindStringSubmatch(result); len(extracted) >= 2 {
		result = strings.TrimSpace(extracted[1])
	}

	var name string
	if err := json.Unmarshal([]byte(result), &name); err != nil {
		// Tolerate a bare, unquoted name on its own line.
		name = strings.Trim(strings.TrimSpace(result), `"`)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	for _, t := range teams {
		if strings.EqualFold(t.Name, name) {
			return t.Name, nil
		}
	}
	return "", nil
}

type RouteClassification struct {
	Route  string `json:"route"`
	Reason string `json:"reason"`
}

// ClassifyRoute asks the sidecar to determine whether a task should use a "fast" or "team" execution path.
func (s *Sidecar) ClassifyRoute(ctx context.Context, prompt string) (RouteClassification, error) {
	if s == nil || s.agent == nil {
		return RouteClassification{}, fmt.Errorf("sidecar not initialized")
	}
	matchPrompt := fmt.Sprintf(`Classify whether the user task requires a "fast" execution path (single agent, simple lookup/edit/test) or a "team" execution path (multi-agent, multi-role research/design/refactor/deploy workflow).

Return ONLY JSON in this exact format:
{"route": "fast" or "team", "reason": "brief explanation"}

User task: %s`, prompt)

	result, err := s.generate(ctx, matchPrompt, ClassifierProfile)
	if err != nil {
		return RouteClassification{}, fmt.Errorf("sidecar classify route generate failed: %w", err)
	}
	result = strings.TrimSpace(result)
	if extracted := jsonCodeBlockRe.FindStringSubmatch(result); len(extracted) >= 2 {
		result = strings.TrimSpace(extracted[1])
	}

	var parsed RouteClassification
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		return RouteClassification{}, fmt.Errorf("failed to parse route classification response %q: %w", result, err)
	}
	parsed.Route = strings.ToLower(strings.TrimSpace(parsed.Route))
	if parsed.Route != "fast" && parsed.Route != "team" {
		return RouteClassification{}, fmt.Errorf("invalid route %q", parsed.Route)
	}
	return parsed, nil
}

type GuardReviewResult struct {
	Approved bool
	Reason   string
}

func (s *Sidecar) ReviewToolCall(ctx context.Context, agentName, toolName, args string, rules []string) (GuardReviewResult, error) {
	if len(rules) == 0 {
		return GuardReviewResult{Approved: true}, nil
	}

	var ruleList strings.Builder
	for i, r := range rules {
		fmt.Fprintf(&ruleList, "%d. %s\n", i+1, r)
	}

	truncArgs := args
	if utf8.RuneCountInString(truncArgs) > 2000 {
		truncArgs = string([]rune(truncArgs)[:2000]) + "\n...(truncated)"
	}

	prompt := fmt.Sprintf(`You are a tool call reviewer. Determine whether the following tool call complies with ALL of the guard rules.

Guard Rules:
%s
Tool Call:
- Tool: %s
- Arguments: %s
- Agent: %s

Respond with JSON only:
{"approved": true, "reason": ""}
or
{"approved": false, "reason": "explanation of which rules are violated"}`, ruleList.String(), toolName, truncArgs, agentName)

	result, err := s.generate(ctx, prompt, ClassifierProfile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar guard review generate failed: %v\n", err)
		return GuardReviewResult{Approved: false, Reason: fmt.Sprintf("guard review generation failed: %v", err)}, err
	}

	result = strings.TrimSpace(result)
	reviewResult, err := parseReviewToolCallResponse(result)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar guard review: failed to parse JSON response %q: %v\n", result, err)
		return GuardReviewResult{Approved: false, Reason: fmt.Sprintf("failed to parse guard review response: %v", err)}, err
	}
	return reviewResult, nil
}

func parseReviewToolCallResponse(response string) (GuardReviewResult, error) {
	response = strings.TrimSpace(response)
	extracted := jsonCodeBlockRe.FindStringSubmatch(response)
	if len(extracted) >= 2 {
		response = strings.TrimSpace(extracted[1])
	}
	var resp struct {
		Approved bool   `json:"approved"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(response), &resp); err != nil {
		return GuardReviewResult{Approved: true}, err
	}
	return GuardReviewResult{Approved: resp.Approved, Reason: resp.Reason}, nil
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

	result, err := s.generate(ctx, prompt, ClassifierProfile)
	if err != nil {
		return -1, fmt.Errorf("sidecar similar task generate failed: %w", err)
	}
	result = strings.TrimSpace(result)
	if result == "" {
		// Log empty response cases for debugging LLM response issues
		fmt.Fprintf(os.Stderr, "warning: sidecar similar task: empty response from LLM\n")
		return -1, nil
	}

	extracted := jsonCodeBlockRe.FindStringSubmatch(result)
	if len(extracted) >= 2 {
		result = strings.TrimSpace(extracted[1])
	}

	var resp struct {
		Match int `json:"match"`
	}
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		// Log JSON parse failures for debugging LLM response issues
		fmt.Fprintf(os.Stderr, "warning: sidecar similar task: failed to parse JSON response %q: %v\n", result, err)
		return -1, fmt.Errorf("sidecar similar task: failed to parse JSON response %q: %w", result, err)
	}
	if resp.Match == 0 {
		return -1, nil
	}
	if resp.Match < 1 || resp.Match > len(pastTasks) {
		// Log out-of-bounds match values for debugging LLM response issues
		fmt.Fprintf(os.Stderr, "warning: sidecar similar task: match value %d out of bounds (expected 1-%d)\n", resp.Match, len(pastTasks))
		return -1, nil
	}
	return resp.Match - 1, nil
}

func (s *Sidecar) ReviewPathAccess(ctx context.Context, command string, path string) (bool, error) {
	if s == nil || s.agent == nil {
		return true, nil
	}

	truncCmd := command
	if utf8.RuneCountInString(truncCmd) > 3000 {
		truncCmd = string([]rune(truncCmd)[:3000]) + "\n...(truncated)"
	}

	prompt := fmt.Sprintf(`You are a path access classifier. Determine whether the path "%s" in the given command is a REAL filesystem access or just a pattern that LOOKS like a path but is NOT actually reading/writing a file.

Rules:
- Paths in sed/grep/awk replacements (e.g., s/foo/bar/, s|/a|/b|, sed 's/X/Y/') are NOT file accesses
- Paths after "=" in variable assignments (e.g., FOO=/path, HOME=/home/user) are NOT file accesses
- Paths in URL-like strings (e.g., https://example.com/path) are NOT file accesses
- Actual file read/write/ls/cd operations ARE file accesses
- Paths that are command names (e.g., /usr/bin/ls) ARE file accesses

Command:
%s

Path: %s

Return ONLY a JSON object: {"is_file_access": true/false, "reason": "brief explanation"}`, path, truncCmd, path)

	result, err := s.generate(ctx, prompt, ClassifierProfile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar ReviewPathAccess generate failed: %v\n", err)
		return true, err
	}

	result = strings.TrimSpace(result)
	extracted := jsonCodeBlockRe.FindStringSubmatch(result)
	if len(extracted) >= 2 {
		result = strings.TrimSpace(extracted[1])
	}

	var parsed struct {
		IsFileAccess bool   `json:"is_file_access"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar ReviewPathAccess: failed to parse JSON response %q: %v\n", result, err)
		return true, err
	}

	return parsed.IsFileAccess, nil
}

func (s *Sidecar) ChooseAskUserResponse(ctx context.Context, question, qtype string, opts []tools.AskUserTUIOption, allowAny bool) (tools.AskUserResponse, error) {
	if s == nil || s.agent == nil {
		return tools.AskUserResponse{}, fmt.Errorf("sidecar not initialized")
	}
	if len(opts) == 0 {
		return tools.AskUserResponse{}, fmt.Errorf("no options provided")
	}

	var list strings.Builder
	for i, opt := range opts {
		value := strings.TrimSpace(opt.Value)
		if value == "" {
			value = strings.TrimSpace(opt.Label)
		}
		fmt.Fprintf(&list, "%d. %s", i+1, opt.Label)
		if value != opt.Label {
			fmt.Fprintf(&list, " (value: %s)", value)
		}
		list.WriteByte('\n')
	}

	prompt := fmt.Sprintf(`You are choosing the best answer for an ask_user prompt in unattended mode.
Pick the safest and most appropriate answer from the options.
Use the exact option value when present; otherwise use the label.

Return ONLY JSON in this exact shape:
{"answers":["..."],"free_text":""}

Question type: %s
Allow free text: %t

Options:
%s
User question: %s`, qtype, allowAny, list.String(), question)

	result, err := s.generate(ctx, prompt, ClassifierProfile)
	if err != nil {
		return tools.AskUserResponse{}, err
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return tools.AskUserResponse{}, fmt.Errorf("empty ask_user selection response")
	}

	if extracted := jsonCodeBlockRe.FindStringSubmatch(result); len(extracted) >= 2 {
		result = strings.TrimSpace(extracted[1])
	}

	var resp tools.AskUserResponse
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		return tools.AskUserResponse{}, fmt.Errorf("failed to parse ask_user selection response %q: %w", result, err)
	}

	normalized, ok := normalizeAskUserSelection(resp, opts, qtype, allowAny)
	if !ok {
		return tools.AskUserResponse{}, fmt.Errorf("ask_user selection response was invalid")
	}
	return normalized, nil
}

func normalizeAskUserSelection(resp tools.AskUserResponse, opts []tools.AskUserTUIOption, qtype string, allowAny bool) (tools.AskUserResponse, bool) {
	if len(opts) == 0 {
		if strings.TrimSpace(resp.Free) == "" {
			return tools.AskUserResponse{}, false
		}
		return tools.AskUserResponse{Free: strings.TrimSpace(resp.Free)}, true
	}

	lookup := make(map[string]string, len(opts)*2)
	for idx, opt := range opts {
		val := strings.TrimSpace(opt.Value)
		if val == "" {
			val = strings.TrimSpace(opt.Label)
		}
		lookup[strings.ToLower(val)] = val
		lookup[strings.ToLower(strings.TrimSpace(opt.Label))] = val
		lookup[fmt.Sprintf("%d", idx+1)] = val
	}

	var answers []string
	for _, ans := range resp.Answers {
		trimmed := strings.TrimSpace(ans)
		if trimmed == "" {
			continue
		}
		if normalized, ok := lookup[strings.ToLower(trimmed)]; ok {
			answers = append(answers, normalized)
			continue
		}
		if idx, err := strconv.Atoi(trimmed); err == nil && idx >= 1 && idx <= len(opts) {
			opt := opts[idx-1]
			val := strings.TrimSpace(opt.Value)
			if val == "" {
				val = strings.TrimSpace(opt.Label)
			}
			answers = append(answers, val)
			continue
		}
		if allowAny {
			answers = append(answers, trimmed)
			continue
		}
		return tools.AskUserResponse{}, false
	}

	if len(answers) == 0 {
		if allowAny && strings.TrimSpace(resp.Free) != "" {
			return tools.AskUserResponse{Free: strings.TrimSpace(resp.Free)}, true
		}
		return tools.AskUserResponse{}, false
	}

	if qtype == "single_choice" && len(answers) > 1 {
		answers = answers[:1]
	}

	return tools.AskUserResponse{Answers: answers, Free: strings.TrimSpace(resp.Free)}, true
}

func (s *Sidecar) CompactStructured(ctx context.Context, conversationText, prevSummaryText, originalGoal string) (string, error) {
	return s.compactStructured(ctx, conversationText, prevSummaryText, originalGoal, sidecarInvocationOptions{
		observeUsage:  true,
		preparePrompt: true,
	})
}

// CompactStructuredTransient performs one projection-only compaction. It
// bypasses both durable prompt preparation and the shared usage observer. The
// bypasses are invocation-scoped: they do not replace or mutate either hook
// used by concurrent durable sidecar work.
func (s *Sidecar) CompactStructuredTransient(ctx context.Context, conversationText, prevSummaryText, originalGoal string) (string, error) {
	return s.compactStructured(ctx, conversationText, prevSummaryText, originalGoal, sidecarInvocationOptions{
		observeUsage:  false,
		preparePrompt: false,
	})
}

func (s *Sidecar) compactStructured(ctx context.Context, conversationText, prevSummaryText, originalGoal string, options sidecarInvocationOptions) (string, error) {
	if s == nil || s.agent == nil {
		return "", fmt.Errorf("sidecar not initialized")
	}

	if prevSummaryText == "" {
		prevSummaryText = "(none)"
	}

	prompt := fmt.Sprintf(`You are an expert conversation summarizer for autonomous agent systems.
Compress the conversation segment into a structured summary. You must merge with the previous structured summary if one exists.

Original User Goal: %s

Previous Structured Summary:
%s

New Conversation Segment to Compact:
%s

You MUST produce a JSON object with the following exact keys:
{
  "goal": "Original user goal and any refined goal",
  "constraints": ["Constraint 1", "Constraint 2"],
  "completed_tasks": ["Task 1", "Task 2"],
  "in_progress_tasks": ["Task A"],
  "blocked_tasks": ["Task B"],
  "key_decisions": ["Decision 1"],
  "errors_and_fixes": ["Error 1 -> Fix 1"],
  "files_read": ["path/to/file1"],
  "files_modified": ["path/to/file2"],
  "artifacts_produced": ["path/to/artifact"],
  "verification_results": ["PASS: test_x", "FAIL: verify_cmd"],
  "open_questions": ["Question 1"],
  "next_actions": ["Next step 1"]
}

Rules:
1. Preserve the original user goal and any user corrections/feedback verbatim or in summary.
2. Preserve all failed verification results and errors. Do NOT omit failures.
3. Preserve all file paths read, modified, and artifacts produced, merging with previous lists without losing history.
4. Ensure every single key is present in the JSON output.

Return ONLY the JSON object.`, originalGoal, prevSummaryText, conversationText)

	result, err := s.generateWithOptions(ctx, prompt, CompactorProfile, options)
	if err != nil {
		return "", fmt.Errorf("sidecar compact structured generate failed: %w", err)
	}
	return strings.TrimSpace(result), nil
}
