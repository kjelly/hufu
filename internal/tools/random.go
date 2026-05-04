package tools

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"charm.land/fantasy"
)

type randomArgs struct {
	Type            string        `json:"type,omitempty"`
	Length          int           `json:"length,omitempty"`
	Charset         string        `json:"charset,omitempty"`
	Min             int           `json:"min,omitempty"`
	Max             int           `json:"max,omitempty"`
	Count           int           `json:"count,omitempty"`
	Items           []interface{} `json:"items,omitempty"`
	AllowDuplicates bool          `json:"allow_duplicates,omitempty"`
}

var charsetPresets = map[string]string{
	"alphanumeric": "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
	"alpha":        "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"numeric":      "0123456789",
	"hex":          "0123456789abcdef",
}

func NewRandomTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "random"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "random",
			Description: "Generate random values: random strings, random integers, or random selections from a list. Useful for generating unique IDs, test data, passwords, or making random selections.",
			Parameters: map[string]any{
				"type": map[string]any{
					"type":        "string",
					"description": "Type of random value to generate: \"string\" (random alphanumeric/hex string), \"integer\" (random integer in range), or \"choice\" (random selection from items list). Default: \"string\"",
					"enum":        []string{"string", "integer", "choice"},
				},
				"length": map[string]any{
					"type":        "number",
					"description": "For type=string: length of the random string (default: 16)",
				},
				"charset": map[string]any{
					"type":        "string",
					"description": "For type=string: character set to use. Options: \"alphanumeric\" (a-zA-Z0-9), \"alpha\" (a-zA-Z), \"numeric\" (0-9), \"hex\" (0-9a-f). Default: \"alphanumeric\"",
					"enum":        []string{"alphanumeric", "alpha", "numeric", "hex"},
				},
				"min": map[string]any{
					"type":        "number",
					"description": "For type=integer: minimum value inclusive (default: 0)",
				},
				"max": map[string]any{
					"type":        "number",
					"description": "For type=integer: maximum value inclusive (default: 100)",
				},
				"count": map[string]any{
					"type":        "number",
					"description": "For type=integer: number of integers to generate (default: 1). For type=choice: number of items to select (default: 1)",
				},
				"items": map[string]any{
					"type":        "array",
					"description": "For type=choice: array of items to select from",
				},
				"allow_duplicates": map[string]any{
					"type":        "boolean",
					"description": "For type=choice: whether to allow duplicate selections (default: true)",
				},
			},
			Required: []string{},
			Parallel: true,
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeRandom(call)
		},
	}
}

func executeRandom(call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args randomArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	switch args.Type {
	case "integer":
		return executeRandomInteger(args)
	case "choice":
		return executeRandomChoice(args)
	default:
		return executeRandomString(args)
	}
}

func executeRandomString(args randomArgs) (fantasy.ToolResponse, error) {
	length := args.Length
	if length <= 0 {
		length = 16
	}
	if length > 1024 {
		return fantasy.NewTextErrorResponse("length must be at most 1024"), nil
	}

	charset := args.Charset
	if charset == "" {
		charset = "alphanumeric"
	}
	chars, ok := charsetPresets[charset]
	if !ok {
		valid := make([]string, 0, len(charsetPresets))
		for k := range charsetPresets {
			valid = append(valid, k)
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown charset %q; valid options: %s", charset, strings.Join(valid, ", "))), nil
	}

	result, err := randomString(length, chars)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("generation failed: %v", err)), nil
	}

	return fantasy.NewTextResponse(result), nil
}

func executeRandomInteger(args randomArgs) (fantasy.ToolResponse, error) {
	min := args.Min
	max := args.Max
	if min > max {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("min (%d) must be <= max (%d)", min, max)), nil
	}

	count := args.Count
	if count <= 0 {
		count = 1
	}
	if count > 100 {
		return fantasy.NewTextErrorResponse("count must be at most 100"), nil
	}

	rangeSize := int64(max - min + 1)
	values := make([]int, count)
	for i := 0; i < count; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(rangeSize))
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("generation failed: %v", err)), nil
		}
		values[i] = int(n.Int64()) + min
	}

	if count == 1 {
		return fantasy.NewTextResponse(fmt.Sprintf("%d", values[0])), nil
	}

	out, err := json.Marshal(values)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("json error: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(out)), nil
}

func executeRandomChoice(args randomArgs) (fantasy.ToolResponse, error) {
	if len(args.Items) == 0 {
		return fantasy.NewTextErrorResponse("items is required for type=choice"), nil
	}

	count := args.Count
	if count <= 0 {
		count = 1
	}

	allowDuplicates := args.AllowDuplicates || args.Count == 0

	if !allowDuplicates && count > len(args.Items) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("count (%d) exceeds number of items (%d) when allow_duplicates is false", count, len(args.Items))), nil
	}

	if allowDuplicates {
		results := make([]interface{}, count)
		for i := 0; i < count; i++ {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(args.Items))))
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("generation failed: %v", err)), nil
			}
			results[i] = args.Items[n.Int64()]
		}
		return formatChoiceResult(results, count)
	}

	pool := make([]interface{}, len(args.Items))
	copy(pool, args.Items)
	for i := len(pool) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("generation failed: %v", err)), nil
		}
		j := n.Int64()
		pool[i], pool[j] = pool[j], pool[i]
	}
	results := pool[:count]
	return formatChoiceResult(results, count)
}

func formatChoiceResult(results []interface{}, count int) (fantasy.ToolResponse, error) {
	if count == 1 {
		out, err := json.Marshal(results[0])
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("json error: %v", err)), nil
		}
		return fantasy.NewTextResponse(string(out)), nil
	}
	out, err := json.Marshal(results)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("json error: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(out)), nil
}

func randomString(length int, chars string) (string, error) {
	result := make([]byte, length)
	charSetBig := big.NewInt(int64(len(chars)))
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, charSetBig)
		if err != nil {
			return "", err
		}
		result[i] = chars[n.Int64()]
	}
	return string(result), nil
}
