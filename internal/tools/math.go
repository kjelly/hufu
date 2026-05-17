//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"charm.land/fantasy"
)

type mathArgs struct {
	Expression string `json:"expression"`
	Precision  int    `json:"precision,omitempty"`
}

type mathResult struct {
	Result float64 `json:"result"`
	Output string  `json:"output"`
}

func NewMathTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "math"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "math",
			Description: "Evaluate mathematical expressions. Supports basic arithmetic (+, -, *, /), power (^), square root (sqrt), parentheses, and constants (pi, e). Examples: '2 + 2', '3^4', 'sqrt(16)', '(10-2)*5', 'pi*2'.",
			Parameters: map[string]any{
				"expression": map[string]any{
					"type":        "string",
					"description": "Mathematical expression to evaluate (e.g. '2 + 2', '3^4', 'sqrt(16)', '(10-2)*5')",
				},
				"precision": map[string]any{
					"type":        "number",
					"description": "Number of decimal places for result (default: 6, max: 15)",
				},
			},
			Required: []string{"expression"},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeMath(call)
		},
	}
}

func executeMath(call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args mathArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Expression == "" {
		return fantasy.NewTextErrorResponse("expression is required"), nil
	}
	if args.Precision <= 0 {
		args.Precision = 6
	}
	if args.Precision > 15 {
		args.Precision = 15
	}

	result, err := evaluateExpression(args.Expression)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	output := formatMathResult(result, args.Precision)
	res := mathResult{Result: result, Output: output}
	jsonData, err := json.Marshal(res)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal result: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(jsonData)), nil
}

func formatMathResult(value float64, precision int) string {
	// float64 can exactly represent integers up to 2^53 (9007199254740992)
	const maxExactInt = float64(1<<53)
	if math.Trunc(value) == value && !math.IsInf(value, 0) && math.Abs(value) <= maxExactInt {
		return strconv.FormatFloat(value, 'f', 0, 64)
	}
	return strconv.FormatFloat(value, 'f', precision, 64)
}

// ── Token ────────────────────────────────────────────────────────────────────

type tokenKind int

const (
	tokNumber tokenKind = iota
	tokPlus
	tokMinus
	tokStar
	tokSlash
	tokCaret
	tokLParen
	tokRParen
	tokIdent
	tokEOF
)

type token struct {
	kind  tokenKind
	value float64
	text  string
}

func tokenize(expr string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(expr) {
		ch := expr[i]
		if unicode.IsSpace(rune(ch)) {
			i++
			continue
		}
		switch ch {
		case '+':
			tokens = append(tokens, token{kind: tokPlus, text: "+"})
			i++
		case '-':
			tokens = append(tokens, token{kind: tokMinus, text: "-"})
			i++
		case '*':
			tokens = append(tokens, token{kind: tokStar, text: "*"})
			i++
		case '/':
			tokens = append(tokens, token{kind: tokSlash, text: "/"})
			i++
		case '^':
			tokens = append(tokens, token{kind: tokCaret, text: "^"})
			i++
		case '(':
			tokens = append(tokens, token{kind: tokLParen, text: "("})
			i++
		case ')':
			tokens = append(tokens, token{kind: tokRParen, text: ")"})
			i++
		default:
			if ch == '.' || (ch >= '0' && ch <= '9') {
				start := i
				hasDot := false
				for i < len(expr) && (expr[i] == '.' || (expr[i] >= '0' && expr[i] <= '9')) {
					if expr[i] == '.' {
						if hasDot {
							return nil, fmt.Errorf("invalid number: multiple decimal points in %q", expr[start:i+1])
						}
						hasDot = true
					}
					i++
				}
				// Scientific notation: 1e-10, 2.5E+3, etc.
				if i < len(expr) && (expr[i] == 'e' || expr[i] == 'E') {
					eStart := i
					i++
					if i < len(expr) && (expr[i] == '+' || expr[i] == '-') {
						i++
					}
					if i >= len(expr) || expr[i] < '0' || expr[i] > '9' {
						return nil, fmt.Errorf("invalid scientific notation: exponent digit required in %q", expr[start:eStart+1])
					}
					for i < len(expr) && expr[i] >= '0' && expr[i] <= '9' {
						i++
					}
				}
				numStr := expr[start:i]
				val, err := strconv.ParseFloat(numStr, 64)
				if err != nil {
					return nil, fmt.Errorf("invalid number: %q", numStr)
				}
				tokens = append(tokens, token{kind: tokNumber, value: val, text: numStr})
			} else if unicode.IsLetter(rune(ch)) {
				start := i
				for i < len(expr) && (unicode.IsLetter(rune(expr[i])) || unicode.IsDigit(rune(expr[i]))) {
					i++
				}
				text := expr[start:i]
				tokens = append(tokens, token{kind: tokIdent, text: text})
			} else {
				return nil, fmt.Errorf("unexpected character: %q", string(ch))
			}
		}
	}
	tokens = append(tokens, token{kind: tokEOF})
	return tokens, nil
}

// ── Parser (recursive descent) ───────────────────────────────────────────────

type parseResult struct {
	value     float64
	remaining []token
}

func evaluateExpression(expr string) (float64, error) {
	tokens, err := tokenize(expr)
	if err != nil {
		return 0, err
	}
	result, err := parseAddSub(tokens)
	if err != nil {
		return 0, err
	}
	if len(result.remaining) > 0 && result.remaining[0].kind != tokEOF {
		return 0, fmt.Errorf("unexpected token: %q", result.remaining[0].text)
	}
	return result.value, nil
}

func parseAddSub(tokens []token) (parseResult, error) {
	left, err := parseMulDiv(tokens)
	if err != nil {
		return parseResult{}, err
	}
	for len(left.remaining) > 0 {
		t := left.remaining[0]
		if t.kind != tokPlus && t.kind != tokMinus {
			break
		}
		right, err := parseMulDiv(left.remaining[1:])
		if err != nil {
			return parseResult{}, err
		}
		if t.kind == tokPlus {
			left.value += right.value
		} else {
			left.value -= right.value
		}
		left.remaining = right.remaining
	}
	return left, nil
}

func parseMulDiv(tokens []token) (parseResult, error) {
	left, err := parsePower(tokens)
	if err != nil {
		return parseResult{}, err
	}
	for len(left.remaining) > 0 {
		t := left.remaining[0]
		if t.kind != tokStar && t.kind != tokSlash {
			break
		}
		right, err := parsePower(left.remaining[1:])
		if err != nil {
			return parseResult{}, err
		}
		if t.kind == tokStar {
			left.value *= right.value
		} else {
			if right.value == 0 {
				return parseResult{}, fmt.Errorf("division by zero")
			}
			left.value /= right.value
		}
		left.remaining = right.remaining
	}
	return left, nil
}

func parsePower(tokens []token) (parseResult, error) {
	left, err := parseUnary(tokens)
	if err != nil {
		return parseResult{}, err
	}
	if len(left.remaining) > 0 && left.remaining[0].kind == tokCaret {
		right, err := parsePower(left.remaining[1:])
		if err != nil {
			return parseResult{}, err
		}
		left.value = math.Pow(left.value, right.value)
		left.remaining = right.remaining
	}
	return left, nil
}

func parseUnary(tokens []token) (parseResult, error) {
	if len(tokens) > 0 && tokens[0].kind == tokMinus {
		result, err := parseUnary(tokens[1:])
		if err != nil {
			return parseResult{}, err
		}
		result.value = -result.value
		return result, nil
	}
	if len(tokens) > 0 && tokens[0].kind == tokPlus {
		return parseUnary(tokens[1:])
	}
	return parsePrimary(tokens)
}

func parsePrimary(tokens []token) (parseResult, error) {
	if len(tokens) == 0 {
		return parseResult{}, fmt.Errorf("unexpected end of expression")
	}

	t := tokens[0]

	switch t.kind {
	case tokNumber:
		return parseResult{value: t.value, remaining: tokens[1:]}, nil

	case tokIdent:
		lower := strings.ToLower(t.text)
		switch lower {
		case "pi":
			return parseResult{value: math.Pi, remaining: tokens[1:]}, nil
		case "e":
			return parseResult{value: math.E, remaining: tokens[1:]}, nil
		case "sqrt":
			if len(tokens) < 3 || tokens[1].kind != tokLParen {
				return parseResult{}, fmt.Errorf("sqrt requires parentheses: sqrt(x)")
			}
			inner, err := parseAddSub(tokens[2:])
			if err != nil {
				return parseResult{}, err
			}
			if len(inner.remaining) == 0 || inner.remaining[0].kind != tokRParen {
				return parseResult{}, fmt.Errorf("sqrt: missing closing parenthesis")
			}
			if inner.value < 0 {
				return parseResult{}, fmt.Errorf("cannot compute square root of negative number")
			}
			return parseResult{value: math.Sqrt(inner.value), remaining: inner.remaining[1:]}, nil
		case "abs":
			if len(tokens) < 3 || tokens[1].kind != tokLParen {
				return parseResult{}, fmt.Errorf("abs requires parentheses: abs(x)")
			}
			inner, err := parseAddSub(tokens[2:])
			if err != nil {
				return parseResult{}, err
			}
			if len(inner.remaining) == 0 || inner.remaining[0].kind != tokRParen {
				return parseResult{}, fmt.Errorf("abs: missing closing parenthesis")
			}
			return parseResult{value: math.Abs(inner.value), remaining: inner.remaining[1:]}, nil
		default:
			return parseResult{}, fmt.Errorf("unknown function or constant: %q", t.text)
		}

	case tokLParen:
		inner, err := parseAddSub(tokens[1:])
		if err != nil {
			return parseResult{}, err
		}
		if len(inner.remaining) == 0 || inner.remaining[0].kind != tokRParen {
			return parseResult{}, fmt.Errorf("mismatched parentheses")
		}
		return parseResult{value: inner.value, remaining: inner.remaining[1:]}, nil

	default:
		return parseResult{}, fmt.Errorf("unexpected token: %q", t.text)
	}
}