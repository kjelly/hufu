package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

type askUserArgs struct {
	Question string       `json:"question"`
	Type     string       `json:"type"`
	Options  []askOption  `json:"options,omitempty"`
	AllowAny bool         `json:"allow_any,omitempty"`
}

type askOption struct {
	Label string `json:"label"`
	Value string `json:"value,omitempty"`
}

type askResponseType struct {
	Answers []string `json:"answers"`
	Free    string   `json:"free_text,omitempty"`
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
			return executeAskUser(call)
		},
	}
}

func executeAskUser(call fantasy.ToolCall) (fantasy.ToolResponse, error) {
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

	StdinMu.Lock()
	defer StdinMu.Unlock()

	reader := bufio.NewReader(os.Stdin)

	fmt.Fprintf(os.Stderr, "\n%s\n", boldFmt("─── Ask User ───"))
	fmt.Fprintf(os.Stderr, "%s\n", args.Question)

	switch questionType {
	case "single_choice":
		return handleSingleChoice(reader, args)
	case "multiple_choice":
		return handleMultipleChoice(reader, args)
	case "free_text":
		return handleFreeText(reader, args)
	case "mixed":
		return handleMixed(reader, args)
	default:
		if len(args.Options) > 0 {
			return handleSingleChoice(reader, args)
		}
		return handleFreeText(reader, args)
	}
}

func handleSingleChoice(reader *bufio.Reader, args askUserArgs) (fantasy.ToolResponse, error) {
	for i, opt := range args.Options {
		label := opt.Label
		if opt.Value != "" && opt.Value != opt.Label {
			label = fmt.Sprintf("%s (%s)", opt.Label, opt.Value)
		}
		fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt(fmt.Sprintf("[%d]", i+1)), label)
	}
	if args.AllowAny {
		fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt("[0]"), "Type your own answer")
	}
	fmt.Fprintf(os.Stderr, "%s ", boldFmt("Your choice:"))

	input := readLine(reader)
	choice, err := strconv.Atoi(strings.TrimSpace(input))
	if err != nil || choice < 0 || choice > len(args.Options) {
		if args.AllowAny {
			resp := askResponseType{Free: strings.TrimSpace(input)}
			data, _ := json.Marshal(resp)
			return fantasy.NewTextResponse(string(data)), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid choice: %q (please enter a number 1-%d)", input, len(args.Options))), nil
	}

	if choice == 0 && args.AllowAny {
		fmt.Fprintf(os.Stderr, "%s ", boldFmt("Your answer:"))
		freeInput := readLine(reader)
		resp := askResponseType{Free: strings.TrimSpace(freeInput)}
		data, _ := json.Marshal(resp)
		return fantasy.NewTextResponse(string(data)), nil
	}

	if choice < 1 {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid choice: %d", choice)), nil
	}

	selected := args.Options[choice-1]
	value := selected.Value
	if value == "" {
		value = selected.Label
	}

	resp := askResponseType{Answers: []string{value}}
	data, _ := json.Marshal(resp)
	return fantasy.NewTextResponse(string(data)), nil
}

func handleMultipleChoice(reader *bufio.Reader, args askUserArgs) (fantasy.ToolResponse, error) {
	for i, opt := range args.Options {
		label := opt.Label
		if opt.Value != "" && opt.Value != opt.Label {
			label = fmt.Sprintf("%s (%s)", opt.Label, opt.Value)
		}
		fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt(fmt.Sprintf("[%d]", i+1)), label)
	}
	fmt.Fprintf(os.Stderr, "%s ", boldFmt("Your choices (comma-separated, e.g. 1,3):"))

	input := readLine(reader)
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
	data, _ := json.Marshal(resp)
	return fantasy.NewTextResponse(string(data)), nil
}

func handleFreeText(reader *bufio.Reader, args askUserArgs) (fantasy.ToolResponse, error) {
	fmt.Fprintf(os.Stderr, "%s ", boldFmt("Your answer:"))
	input := readLine(reader)
	resp := askResponseType{Free: strings.TrimSpace(input)}
	data, _ := json.Marshal(resp)
	return fantasy.NewTextResponse(string(data)), nil
}

func handleMixed(reader *bufio.Reader, args askUserArgs) (fantasy.ToolResponse, error) {
	for i, opt := range args.Options {
		label := opt.Label
		if opt.Value != "" && opt.Value != opt.Label {
			label = fmt.Sprintf("%s (%s)", opt.Label, opt.Value)
		}
		fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt(fmt.Sprintf("[%d]", i+1)), label)
	}
	fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt("[0]"), "Type your own answer")
	fmt.Fprintf(os.Stderr, "%s ", boldFmt("Your choice(s) or free text:"))

	input := readLine(reader)
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
				fmt.Fprintf(os.Stderr, "%s ", boldFmt("Your answer:"))
				freeInput := readLine(reader)
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
	data, _ := json.Marshal(resp)
	return fantasy.NewTextResponse(string(data)), nil
}

func readLine(reader *bufio.Reader) string {
	line, _ := reader.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func boldFmt(s string) string {
	return fmt.Sprintf("\033[1m%s\033[0m", s)
}

func cyanFmt(s string) string {
	return fmt.Sprintf("\033[36m%s\033[0m", s)
}