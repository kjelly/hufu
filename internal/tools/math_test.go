//go:build linux || darwin
// +build linux darwin

package tools

import (
	"encoding/json"
	"math"
	"testing"

	"charm.land/fantasy"
)

func runMath(t *testing.T, expression string) mathResult {
	t.Helper()
	tool := NewMathTool()
	inputBytes, _ := json.Marshal(map[string]string{"expression": expression})
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: string(inputBytes)})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error for %q: %s", expression, result.Content)
	}
	var mr mathResult
	if err := json.Unmarshal([]byte(result.Content), &mr); err != nil {
		t.Fatalf("expected JSON result, got %q", result.Content)
	}
	return mr
}

func runMathErr(t *testing.T, expression string) string {
	t.Helper()
	tool := NewMathTool()
	inputBytes, _ := json.Marshal(map[string]string{"expression": expression})
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: string(inputBytes)})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("expected error for %q, got %s", expression, result.Content)
	}
	return result.Content
}

func TestMathToolInfo(t *testing.T) {
	tool := NewMathTool()
	info := tool.Info()
	if info.Name != "math" {
		t.Errorf("Name = %q, want %q", info.Name, "math")
	}
	if !info.Parallel {
		t.Error("Parallel should be true")
	}
}

func TestMath_Addition(t *testing.T) {
	mr := runMath(t, "2 + 3")
	if mr.Result != 5 {
		t.Errorf("2 + 3 = %v, want 5", mr.Result)
	}
}

func TestMath_Subtraction(t *testing.T) {
	mr := runMath(t, "10 - 4")
	if mr.Result != 6 {
		t.Errorf("10 - 4 = %v, want 6", mr.Result)
	}
}

func TestMath_Multiplication(t *testing.T) {
	mr := runMath(t, "3 * 4")
	if mr.Result != 12 {
		t.Errorf("3 * 4 = %v, want 12", mr.Result)
	}
}

func TestMath_Division(t *testing.T) {
	mr := runMath(t, "15 / 3")
	if mr.Result != 5 {
		t.Errorf("15 / 3 = %v, want 5", mr.Result)
	}
}

func TestMath_DivisionWithDecimal(t *testing.T) {
	mr := runMath(t, "10 / 3")
	if math.Abs(mr.Result-3.333333) > 0.001 {
		t.Errorf("10 / 3 = %v, want ~3.333333", mr.Result)
	}
}

func TestMath_OrderOfOperations(t *testing.T) {
	mr := runMath(t, "2 + 3 * 4")
	if mr.Result != 14 {
		t.Errorf("2 + 3 * 4 = %v, want 14", mr.Result)
	}
}

func TestMath_Parentheses(t *testing.T) {
	mr := runMath(t, "(2 + 3) * 4")
	if mr.Result != 20 {
		t.Errorf("(2 + 3) * 4 = %v, want 20", mr.Result)
	}
}

func TestMath_NestedParentheses(t *testing.T) {
	mr := runMath(t, "((2 + 3) * (4 - 1))")
	if mr.Result != 15 {
		t.Errorf("((2 + 3) * (4 - 1)) = %v, want 15", mr.Result)
	}
}

func TestMath_Power(t *testing.T) {
	mr := runMath(t, "2^3")
	if mr.Result != 8 {
		t.Errorf("2^3 = %v, want 8", mr.Result)
	}
}

func TestMath_PowerRightAssociative(t *testing.T) {
	mr := runMath(t, "2^3^2")
	if mr.Result != 512 {
		t.Errorf("2^3^2 = %v, want 512 (2^(3^2)=2^9)", mr.Result)
	}
}

func TestMath_Sqrt(t *testing.T) {
	mr := runMath(t, "sqrt(16)")
	if mr.Result != 4 {
		t.Errorf("sqrt(16) = %v, want 4", mr.Result)
	}
}

func TestMath_SqrtOfZero(t *testing.T) {
	mr := runMath(t, "sqrt(0)")
	if mr.Result != 0 {
		t.Errorf("sqrt(0) = %v, want 0", mr.Result)
	}
}

func TestMath_Abs(t *testing.T) {
	mr := runMath(t, "abs(-5)")
	if mr.Result != 5 {
		t.Errorf("abs(-5) = %v, want 5", mr.Result)
	}
}

func TestMath_AbsPositive(t *testing.T) {
	mr := runMath(t, "abs(3)")
	if mr.Result != 3 {
		t.Errorf("abs(3) = %v, want 3", mr.Result)
	}
}

func TestMath_Pi(t *testing.T) {
	mr := runMath(t, "pi")
	if math.Abs(mr.Result-math.Pi) > 1e-10 {
		t.Errorf("pi = %v, want %v", mr.Result, math.Pi)
	}
}

func TestMath_E(t *testing.T) {
	mr := runMath(t, "e")
	if math.Abs(mr.Result-math.E) > 1e-10 {
		t.Errorf("e = %v, want %v", mr.Result, math.E)
	}
}

func TestMath_PiInExpression(t *testing.T) {
	mr := runMath(t, "pi * 2")
	if math.Abs(mr.Result-2*math.Pi) > 1e-10 {
		t.Errorf("pi * 2 = %v, want %v", mr.Result, 2*math.Pi)
	}
}

func TestMath_NegativeUnary(t *testing.T) {
	mr := runMath(t, "-5 + 3")
	if mr.Result != -2 {
		t.Errorf("-5 + 3 = %v, want -2", mr.Result)
	}
}

func TestMath_DoubleNegative(t *testing.T) {
	mr := runMath(t, "--3")
	if mr.Result != 3 {
		t.Errorf("--3 = %v, want 3", mr.Result)
	}
}

func TestMath_DecimalNumbers(t *testing.T) {
	mr := runMath(t, "3.14 * 2")
	if math.Abs(mr.Result-6.28) > 0.01 {
		t.Errorf("3.14 * 2 = %v, want ~6.28", mr.Result)
	}
}

func TestMath_ComplexExpression(t *testing.T) {
	mr := runMath(t, "(2 + 3) * 4 - 10 / 5")
	if mr.Result != 18 {
		t.Errorf("(2 + 3) * 4 - 10 / 5 = %v, want 18", mr.Result)
	}
}

func TestMath_Precision(t *testing.T) {
	tool := NewMathTool()
	inputBytes, _ := json.Marshal(map[string]any{"expression": "10/3", "precision": 2})
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: string(inputBytes)})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
	var mr mathResult
	if err := json.Unmarshal([]byte(result.Content), &mr); err != nil {
		t.Fatalf("expected JSON, got %q", result.Content)
	}
	if mr.Output != "3.33" {
		t.Errorf("10/3 with precision 2: output = %q, want %q", mr.Output, "3.33")
	}
}

func TestMath_PrecisionZero(t *testing.T) {
	tool := NewMathTool()
	inputBytes, _ := json.Marshal(map[string]any{"expression": "10/3", "precision": 0})
	result, err := tool.Run(t.Context(), fantasy.ToolCall{Input: string(inputBytes)})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}
}

func TestMath_EmptyExpression(t *testing.T) {
	runMathErr(t, "")
}

func TestMath_DivisionByZero(t *testing.T) {
	errMsg := runMathErr(t, "5 / 0")
	if errMsg == "" {
		t.Error("expected division by zero error")
	}
}

func TestMath_NegativeSqrt(t *testing.T) {
	errMsg := runMathErr(t, "sqrt(-4)")
	if errMsg == "" {
		t.Error("expected negative sqrt error")
	}
}

func TestMath_MismatchedParentheses(t *testing.T) {
	errMsg := runMathErr(t, "(2 + 3")
	if errMsg == "" {
		t.Error("expected mismatched parentheses error")
	}
}

func TestMath_CloseParenWithoutOpen(t *testing.T) {
	errMsg := runMathErr(t, "2 + 3)")
	if errMsg == "" {
		t.Error("expected error for closing paren without opening")
	}
}

func TestMath_UnknownFunction(t *testing.T) {
	errMsg := runMathErr(t, "foo(5)")
	if errMsg == "" {
		t.Error("expected unknown function error")
	}
}

func TestMath_UnexpectedCharacter(t *testing.T) {
	errMsg := runMathErr(t, "2 @ 3")
	if errMsg == "" {
		t.Error("expected unexpected character error")
	}
}

func TestMath_SqrtMissingParens(t *testing.T) {
	errMsg := runMathErr(t, "sqrt 16")
	if errMsg == "" {
		t.Error("expected error for sqrt without parentheses")
	}
}

func TestMath_IntegerResult(t *testing.T) {
	mr := runMath(t, "2 + 2")
	if mr.Output != "4" {
		t.Errorf("2 + 2 output = %q, want %q", mr.Output, "4")
	}
}

func TestMath_LargeNumbers(t *testing.T) {
	mr := runMath(t, "1000000 * 1000000")
	if mr.Result != 1e12 {
		t.Errorf("1000000 * 1000000 = %v, want 1e12", mr.Result)
	}
}

func TestMath_WhitespaceHandling(t *testing.T) {
	mr := runMath(t, "  2   +   3  ")
	if mr.Result != 5 {
		t.Errorf("'  2   +   3  ' = %v, want 5", mr.Result)
	}
}

func TestMath_MultipleOperations(t *testing.T) {
	mr := runMath(t, "1 + 2 - 3 + 4")
	if mr.Result != 4 {
		t.Errorf("1 + 2 - 3 + 4 = %v, want 4", mr.Result)
	}
}

func TestMath_CombinedSqrtAndPower(t *testing.T) {
	mr := runMath(t, "sqrt(2^2)")
	if mr.Result != 2 {
		t.Errorf("sqrt(2^2) = %v, want 2", mr.Result)
	}
}

func TestMath_ScientificNotation(t *testing.T) {
	tests := []struct {
		expr string
		want float64
	}{
		{"1e-10", 1e-10},
		{"2.5e3", 2500},
		{"1E+5", 100000},
		{"3.14e-2", 0.0314},
		{"1e0", 1},
		{"5e+2", 500},
	}
	for _, tt := range tests {
		mr := runMath(t, tt.expr)
		delta := math.Abs(tt.want * 1e-10)
		if delta < 1e-15 {
			delta = 1e-15
		}
		if math.Abs(mr.Result-tt.want) > delta {
			t.Errorf("%s = %v, want %v (delta=%v)", tt.expr, mr.Result, tt.want, math.Abs(mr.Result-tt.want))
		}
	}
}

func TestMath_ScientificNotationInExpression(t *testing.T) {
	mr := runMath(t, "1e3 + 2e2")
	if math.Abs(mr.Result-1200) > 1e-10 {
		t.Errorf("1e3 + 2e2 = %v, want 1200", mr.Result)
	}
}

func TestMath_InvalidScientificNotation(t *testing.T) {
	errMsg := runMathErr(t, "1e")
	if errMsg == "" {
		t.Error("expected error for '1e' (missing exponent)")
	}
}

func TestMath_LargeIntegerPrecision(t *testing.T) {
	mr := runMath(t, "9007199254740992")
	if mr.Output != "9007199254740992" {
		t.Errorf("9007199254740992 output = %q, want exact integer", mr.Output)
	}
}

func TestMath_JSONEscaping(t *testing.T) {
	mr := runMath(t, "1 + 2")
	if math.Abs(mr.Result-3) > 1e-10 {
		t.Errorf("1 + 2 = %v, want 3", mr.Result)
	}
}

func TestMath_NaNResult(t *testing.T) {
	errMsg := runMathErr(t, "sqrt(-1)")
	if errMsg == "" {
		t.Error("expected error for sqrt(-1) (NaN)")
	}
}
