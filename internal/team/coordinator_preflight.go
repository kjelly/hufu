package team

// Provider-aware, deterministic shaping for the coordinator's first model
// request. This runs before Fantasy sends a request, so a provider can reject
// an oversized prompt only when its advertised model metadata is stale or its
// runtime context is hardware-dependent.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"charm.land/fantasy"
)

type coordinatorPreflightContextKey struct{}

type coordinatorRequestPreflight struct {
	mu         sync.RWMutex
	modelID    string
	userPrompt string
	fullSystem string
	fullTools  []fantasy.AgentTool
	window     int
}

func newCoordinatorRequestPreflight(modelID, userPrompt, system string, tools []fantasy.AgentTool) *coordinatorRequestPreflight {
	if strings.TrimSpace(modelID) == "" {
		return nil
	}
	return &coordinatorRequestPreflight{
		modelID:    modelID,
		userPrompt: userPrompt,
		fullSystem: system,
		fullTools:  append([]fantasy.AgentTool(nil), tools...),
		window:     GlobalModelSpecRegistry().GetSpec(modelID).ContextWindow,
	}
}

func withCoordinatorRequestPreflight(ctx context.Context, preflight *coordinatorRequestPreflight) context.Context {
	if preflight == nil {
		return ctx
	}
	return context.WithValue(ctx, coordinatorPreflightContextKey{}, preflight)
}

func coordinatorRequestPreflightFromContext(ctx context.Context) *coordinatorRequestPreflight {
	if ctx == nil {
		return nil
	}
	preflight, _ := ctx.Value(coordinatorPreflightContextKey{}).(*coordinatorRequestPreflight)
	return preflight
}

func (p *coordinatorRequestPreflight) configuration() (string, []fantasy.AgentTool) {
	if p == nil {
		return "", nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.fullSystem, append([]fantasy.AgentTool(nil), p.fullTools...)
}

func (p *coordinatorRequestPreflight) windowValue() int {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.window
}

// observeWindow applies a provider-reported runtime limit to this request.
// The next PrepareStep call then reshapes the complete coordinator request,
// including the system prompt and tool definitions, before retrying.
func (p *coordinatorRequestPreflight) observeWindow(window int) {
	if p == nil || window <= 0 {
		return
	}
	p.mu.Lock()
	if p.window <= 0 || window < p.window {
		p.window = window
	}
	p.mu.Unlock()
}

// prepare returns only the fields that Fantasy should override for this step.
// A nil system/tools result means the full agent configuration remains active.
func (p *coordinatorRequestPreflight) prepare(ctx context.Context, stepMessages []fantasy.Message, prompt string, maxOutputTokens, stepNumber int) (string, []fantasy.AgentTool, bool, error) {
	if p == nil {
		return "", nil, false, nil
	}
	p.mu.RLock()
	modelID := p.modelID
	userPrompt := p.userPrompt
	fullSystem := p.fullSystem
	fullTools := append([]fantasy.AgentTool(nil), p.fullTools...)
	window := p.window
	p.mu.RUnlock()
	if window <= 0 {
		return "", nil, false, nil
	}
	if prompt != "" {
		userPrompt = prompt
	}
	spec := GlobalModelSpecRegistry().GetSpec(modelID).WithEffectiveMaxOutputTokens(maxOutputTokens)
	if spec.ContextWindow > 0 && (window <= 0 || spec.ContextWindow < window) {
		window = spec.ContextWindow
	}
	spec.ContextWindow = window
	budget := CalculateContextBudget(spec, 0, 0).Available
	if budget <= 0 {
		// ContextWindowManager is the owner of the final decision. Return the
		// unmodified configuration as a candidate so an impossible request is
		// reported as CannotFit there, rather than as a preflight projection
		// error that hides the admission result.
		return fullSystem, fullTools, false, nil
	}

	fullTokens := requestContextTokens(ctx, modelID, fullSystem, userPrompt, stepMessages, fullTools)
	if fullTokens <= budget {
		// Fantasy keeps the last non-nil step tool set for the remainder of a
		// stream. Explicitly restore the complete coordinator configuration on
		// later steps after an earlier step used a projection.
		if stepNumber > 0 {
			return fullSystem, fullTools, true, nil
		}
		return "", nil, false, nil
	}

	system := compactCoordinatorSystemPrompt(fullSystem, false)
	tools := fullTools
	for pass := 0; pass < 3; pass++ {
		fixedTokens := requestContextTokens(ctx, modelID, "", userPrompt, stepMessages, nil)
		toolTokens, _ := defaultCounter.CountTools(ctx, modelID, tools)
		maxSystemTokens := budget - fixedTokens - toolTokens
		if maxSystemTokens > 0 {
			if shaped := shrinkCoordinatorSystemToBudget(ctx, modelID, system, maxSystemTokens); shaped != "" {
				system = shaped
			}
		}

		systemTokens, _ := defaultCounter.CountText(ctx, modelID, system)
		maxToolTokens := budget - fixedTokens - systemTokens
		if maxToolTokens > 0 {
			tools = projectCoordinatorToolsToBudget(ctx, modelID, fullTools, maxToolTokens)
		}

		total := requestContextTokens(ctx, modelID, system, userPrompt, stepMessages, tools)
		if total <= budget {
			return system, tools, true, nil
		}
		// The tool projection can make more room for the required prompt. On
		// the next pass compact the prompt again using the newly reduced tool
		// set. The loop is bounded and deterministic.
		if pass == 0 {
			system = compactCoordinatorSystemPrompt(system, true)
		}
	}

	return system, tools, true, nil
}

func requestContextTokens(ctx context.Context, modelID, system, prompt string, messages []fantasy.Message, tools []fantasy.AgentTool) int {
	if tokens, err := defaultCounter.CountProviderRequest(ctx, modelID, providerCallFromContextRequest(modelID, system, prompt, messages, tools)); err == nil {
		return tokens
	}
	messageTokens, _ := defaultCounter.CountMessages(ctx, modelID, messages)
	// Fantasy passes the initial system and user prompt inside opts.Messages.
	// Count the request as it will actually be sent: replace those messages
	// with the preflight overrides instead of counting them a second time. The
	// nil/partial-message case remains supported for focused unit tests and
	// callers that provide only conversation history.
	if system != "" {
		if systemMessage, ok := firstMessageWithRole(messages, fantasy.MessageRoleSystem); ok {
			messageTokens -= countSingleMessage(ctx, modelID, systemMessage)
			messageTokens += countSingleMessage(ctx, modelID, fantasy.NewSystemMessage(system))
		} else {
			messageTokens += countSingleMessage(ctx, modelID, fantasy.NewSystemMessage(system))
		}
	}
	if !hasExactUserMessage(messages, prompt) {
		messageTokens += countSingleMessage(ctx, modelID, fantasy.NewUserMessage(prompt))
	}
	toolTokens, _ := defaultCounter.CountTools(ctx, modelID, tools)
	return messageTokens + toolTokens
}

func countSingleMessage(ctx context.Context, modelID string, message fantasy.Message) int {
	tokens, _ := defaultCounter.CountMessages(ctx, modelID, []fantasy.Message{message})
	return tokens
}

func firstMessageWithRole(messages []fantasy.Message, role fantasy.MessageRole) (fantasy.Message, bool) {
	for _, message := range messages {
		if message.Role == role {
			return message, true
		}
	}
	return fantasy.Message{}, false
}

func hasExactUserMessage(messages []fantasy.Message, prompt string) bool {
	if prompt == "" {
		return true
	}
	for _, message := range messages {
		if message.Role != fantasy.MessageRoleUser || len(message.Content) != 1 {
			continue
		}
		part, ok := fantasy.AsMessagePart[fantasy.TextPart](message.Content[0])
		if ok && part.Text == prompt {
			return true
		}
	}
	return false
}

type coordinatorPromptSection struct {
	heading string
	body    string
}

func splitCoordinatorPromptSections(prompt string) (string, []coordinatorPromptSection) {
	lines := strings.Split(prompt, "\n")
	var preamble strings.Builder
	var sections []coordinatorPromptSection
	current := -1
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			sections = append(sections, coordinatorPromptSection{heading: strings.TrimSpace(strings.TrimPrefix(line, "## "))})
			current = len(sections) - 1
			continue
		}
		if current < 0 {
			preamble.WriteString(line)
			preamble.WriteByte('\n')
			continue
		}
		sections[current].body += line + "\n"
	}
	return strings.TrimRight(preamble.String(), "\n"), sections
}

func renderCoordinatorPromptSections(preamble string, sections []coordinatorPromptSection) string {
	var b strings.Builder
	if strings.TrimSpace(preamble) != "" {
		b.WriteString(strings.TrimSpace(preamble))
		b.WriteString("\n\n")
	}
	for i, section := range sections {
		if strings.TrimSpace(section.heading) == "" {
			continue
		}
		fmt.Fprintf(&b, "## %s\n", section.heading)
		b.WriteString(strings.TrimRight(section.body, "\n"))
		if i+1 < len(sections) {
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func compactCoordinatorSystemPrompt(prompt string, aggressive bool) string {
	preamble, sections := splitCoordinatorPromptSections(prompt)
	compact := make([]coordinatorPromptSection, 0, len(sections))
	for _, section := range sections {
		title := strings.ToLower(strings.TrimSpace(section.heading))
		switch title {
		case "tools":
			section.body = "Tool schemas are authoritative. Use only the tools exposed in this request and follow their declared arguments."
		case "worker tools":
			if aggressive {
				continue
			}
			section.body = "Workers receive their configured tools plus the runtime result protocol."
		case "available skills":
			section.body = "Relevant skills can be loaded with load_skill. Include required skill names in delegated task descriptions."
		case "auto-loaded skills":
			if aggressive {
				continue
			}
		case "available agents":
			section.body = compactAvailableAgentsBody(section.body)
		case "available models":
			section.body = compactAvailableModelsBody(section.body)
		case "environment & rules":
			section.body = compactEnvironmentBody(section.body)
		}
		compact = append(compact, section)
	}
	return renderCoordinatorPromptSections(preamble, compact)
}

func compactAvailableAgentsBody(body string) string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Valid names:") || strings.HasPrefix(trimmed, "### ") {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		return "Use only the exact worker names declared by the agent tool."
	}
	return strings.Join(kept, "\n")
}

func compactAvailableModelsBody(body string) string {
	var models []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- **") {
			models = append(models, trimmed)
		}
	}
	if len(models) == 0 {
		return "Use the configured default model when no task-specific model is selected."
	}
	return "Available models: " + strings.Join(models, " ")
}

func compactEnvironmentBody(body string) string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) > 4 {
		lines = lines[:4]
	}
	return strings.Join(lines, "\n")
}

func shrinkCoordinatorSystemToBudget(ctx context.Context, modelID, prompt string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if tokens, _ := defaultCounter.CountText(ctx, modelID, prompt); tokens <= maxTokens {
		return prompt
	}
	preamble, sections := splitCoordinatorPromptSections(prompt)
	optional := map[string]bool{
		"project context (agents.md)": true,
		"session context":             true,
		"available agents":            true,
		"available models":            true,
		"available skills":            true,
		"auto-loaded skills":          true,
		"environment & rules":         true,
	}
	for i := range sections {
		if !optional[strings.ToLower(strings.TrimSpace(sections[i].heading))] {
			continue
		}
		current := renderCoordinatorPromptSections(preamble, sections)
		currentTokens, _ := defaultCounter.CountText(ctx, modelID, current)
		if currentTokens <= maxTokens {
			return current
		}
		sectionTokens, _ := defaultCounter.CountText(ctx, modelID, sections[i].body)
		allowed := maxTokens - (currentTokens - sectionTokens)
		if allowed <= 0 {
			sections[i].body = ""
			continue
		}
		sections[i].body = squeezeTextToTokenBudget(ctx, modelID, sections[i].body, allowed)
	}
	result := renderCoordinatorPromptSections(preamble, sections)
	// Return the best deterministic reduction even when required sections alone
	// still exceed the target. The caller performs another bounded pass (and
	// ultimately fails closed) rather than silently discarding the reduction.
	return result
}

func squeezeTextToTokenBudget(ctx context.Context, modelID, text string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if tokens, _ := defaultCounter.CountText(ctx, modelID, text); tokens <= maxTokens {
		return text
	}
	runes := []rune(text)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		candidate := squeezeText(string(runes[:mid]), mid)
		tokens, _ := defaultCounter.CountText(ctx, modelID, candidate)
		if tokens <= maxTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		return ""
	}
	return squeezeText(string(runes[:lo]), lo)
}

func projectCoordinatorToolsToBudget(ctx context.Context, modelID string, tools []fantasy.AgentTool, maxTokens int) []fantasy.AgentTool {
	if len(tools) == 0 || maxTokens <= 0 {
		return tools
	}
	selected := make([]fantasy.AgentTool, 0, len(tools))
	selectedNames := make(map[string]bool, len(tools))
	used := 0
	for _, tool := range tools {
		if tool == nil || !coordinatorToolRequiredForInitialRequest(tool.Info().Name) {
			continue
		}
		selected = append(selected, tool)
		selectedNames[tool.Info().Name] = true
		tokens, _ := defaultCounter.CountTools(ctx, modelID, []fantasy.AgentTool{tool})
		used += tokens
	}
	if used > maxTokens {
		return selected
	}
	for _, tool := range tools {
		if tool == nil || selectedNames[tool.Info().Name] {
			continue
		}
		tokens, _ := defaultCounter.CountTools(ctx, modelID, []fantasy.AgentTool{tool})
		if used+tokens > maxTokens {
			continue
		}
		selected = append(selected, tool)
		selectedNames[tool.Info().Name] = true
		used += tokens
	}
	if len(selected) == 0 {
		return tools
	}
	return selected
}

func coordinatorToolRequiredForInitialRequest(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "agent", "run_agents", "finish":
		return true
	default:
		return false
	}
}
