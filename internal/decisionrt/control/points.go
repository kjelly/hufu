package control

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/decisionrt"
)

const (
	specVersion = "v1"
	// maxChoiceCandidates is the DecisionPrimitive choice-domain limit.
	maxChoiceCandidates = 21
	// contextBudgetBytes keeps each context string under the 4096-byte
	// DecisionPrimitive limit with headroom for redaction markers.
	contextBudgetBytes = 3584
	// descriptionBudgetBytes keeps option descriptions under the 1024-byte
	// limit after the "name: " prefix.
	descriptionBudgetBytes = 900
	// The rune limits match the sidecar prompts these points shadow.
	descriptionRunes = 300
	commandRunes     = 3000
	argumentRunes    = 2000
)

const (
	agentMatcherQuestion = "Select the single worker best suited to carry out the task in state.task. " +
		"Each option names one authorized worker and describes what it does."
	askUserQuestion = "An unattended run must answer the question in state.question without a human. " +
		"Choose the safest and most appropriate option."
	pathReviewerQuestion = "Is state.path a real filesystem access in the shell command state.command? " +
		"Reading, writing, listing, changing into, or executing the path is a file access. " +
		"A path that only appears inside a sed, grep, or awk pattern or replacement, " +
		"after '=' in a variable assignment, or inside a URL is not a file access."
	guardReviewerQuestion = "Does the tool call described in state comply with every guard rule listed in state.rules? " +
		"state.agent made the call, state.tool is the tool name, and state.arguments are its arguments."
)

// Worker is one candidate for the agent matcher.
type Worker struct {
	Name        string
	Description string
}

// Option is one ask_user choice.
type Option struct {
	Label string
	Value string
}

// AgentMatcherRequest asks which worker should carry out task. Options keep
// the order of workers, so an index outcome maps back to workers[index]. It
// returns false when the point does not apply to this call.
func AgentMatcherRequest(task string, workers []Worker) (decisionrt.Request, bool) {
	if len(workers) < 2 || len(workers) > maxChoiceCandidates {
		return decisionrt.Request{}, false
	}
	options := make([]decisionrt.Option, 0, len(workers))
	for index, worker := range workers {
		description := worker.Name
		if text := strings.TrimSpace(worker.Description); text != "" {
			description += ": " + text
		}
		options = append(options, decisionrt.Option{ID: fmt.Sprintf("w%02d", index+1), Description: limitText(description, descriptionRunes, descriptionBudgetBytes)})
	}
	return buildRequest(AgentMatcher, decisionrt.KindChoice, agentMatcherQuestion, options, map[string]any{
		"task": limitText(task, 0, contextBudgetBytes),
	})
}

// AskUserRequest asks which option an unattended run should answer with.
// Options keep their order, so an index outcome maps back to options[index].
func AskUserRequest(question string, options []Option) (decisionrt.Request, bool) {
	if len(options) < 2 || len(options) > maxChoiceCandidates {
		return decisionrt.Request{}, false
	}
	specOptions := make([]decisionrt.Option, 0, len(options))
	for index, option := range options {
		label := strings.TrimSpace(option.Label)
		value := strings.TrimSpace(option.Value)
		description := label
		if value != "" && value != label {
			description += " (value: " + value + ")"
		}
		specOptions = append(specOptions, decisionrt.Option{ID: fmt.Sprintf("option-%02d", index+1), Description: limitText(description, descriptionRunes, descriptionBudgetBytes)})
	}
	return buildRequest(AskUser, decisionrt.KindChoice, askUserQuestion, specOptions, map[string]any{
		"question": limitText(question, 0, contextBudgetBytes),
	})
}

// PathReviewerRequest asks whether path is a real filesystem access in
// command.
func PathReviewerRequest(command, path string) (decisionrt.Request, bool) {
	if strings.TrimSpace(path) == "" || len(path) > contextBudgetBytes {
		return decisionrt.Request{}, false
	}
	return buildRequest(PathReviewer, decisionrt.KindBoolean, pathReviewerQuestion, nil, map[string]any{
		"command": limitText(command, commandRunes, contextBudgetBytes),
		"path":    path,
	})
}

// GuardReviewerRequest asks whether a tool call complies with every rule.
// Rules are never truncated: a rule list over the context budget makes the
// point inapplicable, so the existing reviewer sees every rule.
func GuardReviewerRequest(agent, tool, arguments string, rules []string) (decisionrt.Request, bool) {
	if len(rules) == 0 {
		return decisionrt.Request{}, false
	}
	var ruleList strings.Builder
	for index, rule := range rules {
		fmt.Fprintf(&ruleList, "%d. %s\n", index+1, rule)
	}
	if ruleList.Len() > contextBudgetBytes || !utf8.ValidString(ruleList.String()) {
		return decisionrt.Request{}, false
	}
	return buildRequest(GuardReviewer, decisionrt.KindBoolean, guardReviewerQuestion, nil, map[string]any{
		"agent":     limitText(agent, 0, contextBudgetBytes),
		"tool":      limitText(tool, 0, contextBudgetBytes),
		"arguments": limitText(arguments, argumentRunes, contextBudgetBytes),
		"rules":     ruleList.String(),
	})
}

func buildRequest(point Point, kind decisionrt.Kind, question string, options []decisionrt.Option, context map[string]any) (decisionrt.Request, bool) {
	id := "hufu." + string(point)
	request := decisionrt.Request{
		Purpose: id,
		Spec:    decisionrt.Spec{ID: id, Version: specVersion, Kind: kind, Question: question, Options: options},
		Context: context,
	}
	if request.Validate() != nil {
		return decisionrt.Request{}, false
	}
	return request, true
}

// limitText makes text valid UTF-8 and truncates it to at most maxRunes runes
// (when positive) and maxBytes bytes, always at a rune boundary.
func limitText(text string, maxRunes, maxBytes int) string {
	text = strings.ToValidUTF8(text, "�")
	if maxRunes > 0 && utf8.RuneCountInString(text) > maxRunes {
		text = string([]rune(text)[:maxRunes])
	}
	if len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
