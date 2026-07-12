//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"github.com/manifoldco/promptui"
)

type askUserArgs struct {
	Question string      `json:"question"`
	Type     string      `json:"type"`
	Options  []askOption `json:"options,omitempty"`
	AllowAny bool        `json:"allow_any,omitempty"`
}

type askOption struct {
	Label string `json:"label"`
	Value string `json:"value,omitempty"`
}

type askResponseType = AskUserResponse

var askUserWordRe = regexp.MustCompile(`[a-z0-9]+`)

var askUserSafePhrases = []string{
	"dry run",
	"dry-run",
	"read only",
	"view only",
	"no changes",
	"preview",
	"inspect",
	"show only",
}

var askUserSafeTokens = map[string]int{
	"no":       -4,
	"cancel":   -4,
	"skip":     -4,
	"abort":    -4,
	"stop":     -4,
	"keep":     -3,
	"ignore":   -3,
	"back":     -3,
	"later":    -2,
	"undo":     -3,
	"revert":   -3,
	"view":     -2,
	"preview":  -2,
	"inspect":  -2,
	"show":     -2,
	"safe":     -2,
	"read":     -1,
	"ok":       -1,
	"okay":     -1,
	"yes":      -1,
	"y":        -1,
	"continue": -1,
	"proceed":  -1,
	"confirm":  -1,
	"allow":    -1,
	"accept":   -1,
	"approve":  -1,
}

var askUserDangerTokens = map[string]int{
	"delete":    3,
	"remove":    3,
	"destroy":   4,
	"wipe":      4,
	"format":    5,
	"reset":     4,
	"overwrite": 4,
	"drop":      4,
	"purge":     4,
	"kill":      4,
	"terminate": 4,
	"shutdown":  4,
	"reboot":    4,
	"restart":   3,
	"force":     3,
	"push":      3,
	"deploy":    3,
	"publish":   3,
	"install":   3,
	"uninstall": 4,
	"upgrade":   3,
	"downgrade": 3,
	"migrate":   3,
	"apply":     2,
	"patch":     2,
	"commit":    2,
	"merge":     2,
	"replace":   3,
	"write":     3,
	"edit":      2,
	"modify":    2,
	"change":    1,
	"run":       1,
	"execute":   1,
	"launch":    1,
	"sync":      2,
	"rebuild":   3,
	"reinstall": 3,
	"rm":        5,
	"dd":        5,
	"mkfs":      5,
	"chmod":     3,
	"chown":     3,
	"ssh":       2,
	"scp":       2,
	"curl":      2,
	"wget":      2,
}

func NewAskUserTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	_ = cfg
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "ask_user",
			Description: "Ask the user a question and wait for their response. Supports multiple choice, free text, or mixed (choices + free text). Use this when you need clarification, confirmation, or input from the user.",
			Parameters: map[string]any{
				"question": map[string]any{
					"type":        "string",
					"description": "The question to ask the user",
				},
				"type": map[string]any{
					"type":        "string",
					"description": "Question type: 'single_choice' (pick one), 'multiple_choice' (pick multiple), 'free_text' (type answer), or 'mixed' (pick from options or type free text)",
				},
				"options": map[string]any{
					"type":        "array",
					"description": "Available choices for single_choice, multiple_choice, or mixed type",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"label": map[string]any{
								"type":        "string",
								"description": "Display label for this option",
							},
							"value": map[string]any{
								"type":        "string",
								"description": "Value to return if selected (defaults to label if empty)",
							},
						},
						"required": []string{"label"},
					},
				},
				"allow_any": map[string]any{
					"type":        "boolean",
					"description": "For single_choice/multiple_choice: also allow free-text input as an answer",
				},
			},
			Required: []string{"question"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeAskUser(ctx, call)
		},
	}
}

func marshalAskUserResponse(resp AskUserResponse) (fantasy.ToolResponse, error) {
	data, err := json.Marshal(resp)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func firstChoiceResponse(args askUserArgs) (fantasy.ToolResponse, error) {
	first := args.Options[0]
	val := first.Value
	if val == "" {
		val = first.Label
	}
	return marshalAskUserResponse(AskUserResponse{Answers: []string{val}})
}

func scoreAskUserOption(text string) int {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return 0
	}

	score := 0
	for _, phrase := range askUserSafePhrases {
		if strings.Contains(normalized, phrase) {
			score -= 3
		}
	}

	for _, token := range askUserWordRe.FindAllString(normalized, -1) {
		if delta, ok := askUserSafeTokens[token]; ok {
			score += delta
		}
		if delta, ok := askUserDangerTokens[token]; ok {
			score += delta
		}
	}

	if strings.Contains(normalized, "rm -rf") || strings.Contains(normalized, "format c:") || strings.Contains(normalized, "mkfs.") {
		score += 8
	}

	return score
}

func optionDisplayText(opt askOption) string {
	text := strings.TrimSpace(opt.Label)
	if v := strings.TrimSpace(opt.Value); v != "" && !strings.EqualFold(v, text) {
		if text != "" {
			text += " "
		}
		text += v
	}
	return text
}

func chooseAutoApproveAskUserResponse(args askUserArgs) (AskUserResponse, bool) {
	if len(args.Options) == 0 {
		return AskUserResponse{}, false
	}

	bestIdx := -1
	bestScore := 0
	for i, opt := range args.Options {
		score := scoreAskUserOption(optionDisplayText(opt))
		if bestIdx == -1 || score < bestScore {
			bestIdx = i
			bestScore = score
		}
	}

	if bestIdx < 0 || bestScore > 0 {
		return AskUserResponse{}, false
	}

	opt := args.Options[bestIdx]
	val := strings.TrimSpace(opt.Value)
	if val == "" {
		val = strings.TrimSpace(opt.Label)
	}
	if val == "" {
		return AskUserResponse{}, false
	}

	if len(args.Options) > 1 && bestScore == 0 {
		return AskUserResponse{}, false
	}

	return AskUserResponse{Answers: []string{val}}, true
}

// unattendedAskUserResponse produces a safe, non-blocking answer when no human
// is available. For choice questions it first asks the configured selector to
// pick the best option; if that fails, it falls back to the first option as a
// documented default. For free-text it returns an error so the agent proceeds
// on its own judgement rather than waiting. Free-text prompts still fire a
// needs-human notification because they cannot be resolved safely without a
// human-provided answer.
func unattendedAskUserResponse(ctx context.Context, args askUserArgs, questionType string) (fantasy.ToolResponse, error) {
	fmt.Fprintf(os.Stderr, "\n%s no human available; auto-answering: %s\n", boldFmt("─── Ask User (unattended) ───"), args.Question)

	if len(args.Options) > 0 {
		if IsAutoApprove(ctx) {
			if resp, ok := chooseAutoApproveAskUserResponse(args); ok {
				return marshalAskUserResponse(resp)
			}
		}
		if selector, ok := ctx.Value(AskUserChoiceSelectorKey).(AskUserChoiceSelector); ok && selector != nil {
			resp, err := selector(ctx, args.Question, questionType, toTUIOptions(args.Options), args.AllowAny)
			if err == nil {
				if normalized, ok := normalizeAskUserResponse(resp, args, questionType); ok {
					return marshalAskUserResponse(normalized)
				}
				fmt.Fprintf(os.Stderr, "warning: unattended ask_user selector returned an invalid answer; falling back to first option\n")
			} else {
				fmt.Fprintf(os.Stderr, "warning: unattended ask_user selector failed: %v; falling back to first option\n", err)
			}
		}
		return firstChoiceResponse(args)
	}

	NotifyNeedsHuman(args.Question)
	return fantasy.NewTextErrorResponse("ask_user unavailable: running unattended with no human to answer. Proceed using your best judgement and reasonable defaults; do not ask again."), nil
}

func toTUIOptions(opts []askOption) []AskUserTUIOption {
	out := make([]AskUserTUIOption, len(opts))
	for i, o := range opts {
		out[i] = AskUserTUIOption{Label: o.Label, Value: o.Value}
	}
	return out
}

func normalizeAskUserResponse(resp AskUserResponse, args askUserArgs, questionType string) (AskUserResponse, bool) {
	_ = questionType
	if len(args.Options) == 0 {
		if strings.TrimSpace(resp.Free) == "" {
			return AskUserResponse{}, false
		}
		return AskUserResponse{Free: strings.TrimSpace(resp.Free)}, true
	}

	if len(resp.Answers) == 0 {
		if args.AllowAny && strings.TrimSpace(resp.Free) != "" {
			return AskUserResponse{Free: strings.TrimSpace(resp.Free)}, true
		}
		return AskUserResponse{}, false
	}

	valid := make([]string, 0, len(resp.Answers))
	lookup := make(map[string]string, len(args.Options)*2)
	for idx, opt := range args.Options {
		val := strings.TrimSpace(opt.Value)
		if val == "" {
			val = strings.TrimSpace(opt.Label)
		}
		lookup[strings.ToLower(val)] = val
		lookup[strings.ToLower(strings.TrimSpace(opt.Label))] = val
		lookup[strconv.Itoa(idx+1)] = val
	}

	for _, ans := range resp.Answers {
		trimmed := strings.TrimSpace(ans)
		if trimmed == "" {
			continue
		}
		if normalized, ok := lookup[strings.ToLower(trimmed)]; ok {
			valid = append(valid, normalized)
			continue
		}
		if idx, err := strconv.Atoi(trimmed); err == nil && idx >= 1 && idx <= len(args.Options) {
			opt := args.Options[idx-1]
			val := strings.TrimSpace(opt.Value)
			if val == "" {
				val = strings.TrimSpace(opt.Label)
			}
			valid = append(valid, val)
			continue
		}
		if args.AllowAny {
			valid = append(valid, trimmed)
			continue
		}
		return AskUserResponse{}, false
	}

	if len(valid) == 0 {
		return AskUserResponse{}, false
	}

	if questionType == "single_choice" && len(valid) > 1 {
		valid = valid[:1]
	}

	return AskUserResponse{Answers: valid, Free: strings.TrimSpace(resp.Free)}, true
}

func executeAskUser(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if IsInteractiveAbortRequested() {
		return fantasy.NewTextErrorResponse("ask_user cancelled during shutdown"), nil
	}

	var args askUserArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	if args.Question == "" {
		return fantasy.NewTextErrorResponse("question is required"), nil
	}

	questionType := args.Type
	if questionType == "" {
		if len(args.Options) > 0 {
			questionType = "single_choice"
		} else {
			questionType = "free_text"
		}
	}

	if (questionType == "single_choice" || questionType == "multiple_choice" || questionType == "mixed") && len(args.Options) == 0 && !args.AllowAny {
		questionType = "free_text"
	}

	if IsAutoApprove(ctx) && len(args.Options) > 0 {
		if resp, ok := chooseAutoApproveAskUserResponse(args); ok {
			if normalized, ok := normalizeAskUserResponse(resp, args, questionType); ok {
				return marshalAskUserResponse(normalized)
			}
		}
	}

	// TUI mode: use the native dialog — no terminal release or stdin needed.
	if jsonResp, ok := TryAskUserTUI(ctx, args.Question, questionType, toTUIOptions(args.Options), args.AllowAny); ok {
		return fantasy.NewTextResponse(jsonResp), nil
	}

	// TUI mode returned false — check if context was cancelled.
	if err := ctx.Err(); err != nil {
		return fantasy.NewTextErrorResponse("ask_user cancelled: " + err.Error()), nil
	}

	// Unattended / non-interactive: there is no human to read from stdin.
	// Returning here is critical — reading stdin would block forever (and the
	// ask_user-aware deadline means the agent timeout would not rescue it).
	// Instead return a safe default and fire a needs-human notification so an
	// operator can follow up out-of-band.
	if IsUnattended(ctx) || !IsInteractiveEnvironment() {
		return unattendedAskUserResponse(ctx, args, questionType)
	}

	if IsInteractiveAbortRequested() {
		return fantasy.NewTextErrorResponse("ask_user cancelled during shutdown"), nil
	}

	// CLI mode: read from stdin.
	StdinMu.Lock()
	defer StdinMu.Unlock()

	SetAskUserActive(true)
	defer SetAskUserActive(false)

	NotifyAskUserStart()

	fmt.Fprintf(os.Stderr, "\n%s\n", boldFmt("─── Ask User ───"))
	fmt.Fprintf(os.Stderr, "%s\n", args.Question)

	switch questionType {
	case "single_choice":
		return handleSingleChoice(args)
	case "multiple_choice":
		return handleMultipleChoice(args)
	case "free_text":
		return handleFreeText(args)
	case "mixed":
		return handleMixed(args)
	default:
		if len(args.Options) > 0 {
			return handleSingleChoice(args)
		}
		return handleFreeText(args)
	}
}

func handleSingleChoice(args askUserArgs) (fantasy.ToolResponse, error) {
	var items []string
	for _, opt := range args.Options {
		label := opt.Label
		if opt.Value != "" && opt.Value != opt.Label {
			label = fmt.Sprintf("%s (%s)", opt.Label, opt.Value)
		}
		items = append(items, label)
	}

	if args.AllowAny {
		items = append(items, "[Type your own answer]")
	}

	prompt := promptui.Select{
		Label: "Select Option",
		Items: items,
		Size:  10,
	}

	index, _, err := prompt.Run()
	if err != nil {
		if IsInteractiveAbortRequested() {
			return fantasy.NewTextErrorResponse("ask_user cancelled during shutdown"), nil
		}
		if err == promptui.ErrInterrupt {
			return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	if args.AllowAny && index == len(args.Options) {
		promptText := promptui.Prompt{
			Label: "Your answer",
		}
		freeInput, err := promptText.Run()
		if err != nil {
			if IsInteractiveAbortRequested() {
				return fantasy.NewTextErrorResponse("ask_user cancelled during shutdown"), nil
			}
			if err == promptui.ErrInterrupt {
				return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
			}
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		resp := askResponseType{Free: strings.TrimSpace(freeInput)}
		data, err := json.Marshal(resp)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
		}
		return fantasy.NewTextResponse(string(data)), nil
	}

	selected := args.Options[index]
	value := selected.Value
	if value == "" {
		value = selected.Label
	}

	resp := askResponseType{Answers: []string{value}}
	data, err := json.Marshal(resp)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func handleMultipleChoice(args askUserArgs) (fantasy.ToolResponse, error) {
	for i, opt := range args.Options {
		label := opt.Label
		if opt.Value != "" && opt.Value != opt.Label {
			label = fmt.Sprintf("%s (%s)", opt.Label, opt.Value)
		}
		fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt(fmt.Sprintf("[%d]", i+1)), label)
	}

	prompt := promptui.Prompt{
		Label: "Your choices (comma-separated, e.g. 1,3)",
	}
	input, err := prompt.Run()
	if err != nil {
		if IsInteractiveAbortRequested() {
			return fantasy.NewTextErrorResponse("ask_user cancelled during shutdown"), nil
		}
		if err == promptui.ErrInterrupt {
			return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	input = strings.TrimSpace(input)
	if input == "" {
		return fantasy.NewTextErrorResponse("no choices provided"), nil
	}

	var answers []string
	parts := strings.Split(input, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		choice, err := strconv.Atoi(p)
		if err != nil || choice < 1 || choice > len(args.Options) {
			if args.AllowAny {
				answers = append(answers, p)
				continue
			}
			return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid choice: %q", p)), nil
		}
		selected := args.Options[choice-1]
		value := selected.Value
		if value == "" {
			value = selected.Label
		}
		answers = append(answers, value)
	}

	resp := askResponseType{Answers: answers}
	data, err := json.Marshal(resp)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func handleFreeText(args askUserArgs) (fantasy.ToolResponse, error) {
	prompt := promptui.Prompt{
		Label: "Your answer",
	}
	input, err := prompt.Run()
	if err != nil {
		if IsInteractiveAbortRequested() {
			return fantasy.NewTextErrorResponse("ask_user cancelled during shutdown"), nil
		}
		if err == promptui.ErrInterrupt {
			return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	resp := askResponseType{Free: strings.TrimSpace(input)}
	data, err := json.Marshal(resp)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func handleMixed(args askUserArgs) (fantasy.ToolResponse, error) {
	for i, opt := range args.Options {
		label := opt.Label
		if opt.Value != "" && opt.Value != opt.Label {
			label = fmt.Sprintf("%s (%s)", opt.Label, opt.Value)
		}
		fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt(fmt.Sprintf("[%d]", i+1)), label)
	}
	fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt("[0]"), "Type your own answer")

	prompt := promptui.Prompt{
		Label: "Your choice(s) or free text",
	}
	input, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	input = strings.TrimSpace(input)
	if input == "" {
		return fantasy.NewTextErrorResponse("no input provided"), nil
	}

	var answers []string
	var freeText string

	parts := strings.Split(input, ",")
	hasNumber := false
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if choice, err := strconv.Atoi(p); err == nil && choice >= 0 && choice <= len(args.Options) {
			hasNumber = true
			if choice == 0 {
				promptText := promptui.Prompt{
					Label: "Your answer",
				}
				freeInput, err := promptText.Run()
				if err != nil {
					if IsInteractiveAbortRequested() {
						return fantasy.NewTextErrorResponse("ask_user cancelled during shutdown"), nil
					}
					if err == promptui.ErrInterrupt {
						return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
					}
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				freeText = strings.TrimSpace(freeInput)
			} else {
				selected := args.Options[choice-1]
				value := selected.Value
				if value == "" {
					value = selected.Label
				}
				answers = append(answers, value)
			}
		}
	}

	if !hasNumber {
		freeText = input
	}

	resp := askResponseType{Answers: answers, Free: freeText}
	data, err := json.Marshal(resp)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func boldFmt(s string) string {
	return fmt.Sprintf("\033[1m%s\033[0m", s)
}

func cyanFmt(s string) string {
	return fmt.Sprintf("\033[36m%s\033[0m", s)
}
