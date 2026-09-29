package cost

import (
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

const microsPerUSD int64 = 1_000_000

// ParseUSDMicros parses a non-negative decimal USD string without using
// floating-point arithmetic. The accepted syntax is intentionally narrower
// than strconv: no signs, exponent, whitespace, or more than six fractional
// digits are allowed.
func ParseUSDMicros(value string) (int64, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return 0, fmt.Errorf("USD value must be a non-empty decimal string without whitespace")
	}
	wholeText, fractionText, hasFraction := strings.Cut(value, ".")
	if wholeText == "" || (len(wholeText) > 1 && wholeText[0] == '0') || !decimalDigits(wholeText) {
		return 0, fmt.Errorf("invalid USD decimal %q", value)
	}
	if hasFraction && (fractionText == "" || len(fractionText) > 6 || !decimalDigits(fractionText)) {
		return 0, fmt.Errorf("invalid USD decimal %q", value)
	}
	whole, err := strconv.ParseUint(wholeText, 10, 64)
	if err != nil || whole > uint64(math.MaxInt64/microsPerUSD) {
		return 0, fmt.Errorf("USD value %q overflows micro-dollar representation", value)
	}
	fraction := uint64(0)
	if hasFraction {
		parsed, parseErr := strconv.ParseUint(fractionText, 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("invalid USD decimal %q", value)
		}
		fraction = parsed
		for range 6 - len(fractionText) {
			fraction *= 10
		}
	}
	micros := whole*uint64(microsPerUSD) + fraction
	if micros > math.MaxInt64 {
		return 0, fmt.Errorf("USD value %q overflows micro-dollar representation", value)
	}
	return int64(micros), nil
}

// FormatUSDMicros returns the canonical fixed-six-decimal USD representation.
func FormatUSDMicros(micros int64) (string, error) {
	if micros < 0 {
		return "", fmt.Errorf("USD micros must be non-negative")
	}
	return fmt.Sprintf("%d.%06d", micros/microsPerUSD, micros%microsPerUSD), nil
}

func decimalDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// TokenRateMicros calculates ceil(tokens * microsPerMillion / 1,000,000)
// with checked integer arithmetic.
func TokenRateMicros(tokens, microsPerMillion int64) (int64, error) {
	if tokens < 0 || microsPerMillion < 0 {
		return 0, fmt.Errorf("tokens and rate must be non-negative")
	}
	if tokens == 0 || microsPerMillion == 0 {
		return 0, nil
	}
	wholeTokens := uint64(tokens / microsPerUSD)
	remainderTokens := uint64(tokens % microsPerUSD)
	rate := uint64(microsPerMillion)
	if wholeTokens > uint64(math.MaxInt64)/rate {
		return 0, fmt.Errorf("token price multiplication overflows")
	}
	wholeCost := wholeTokens * rate
	high, low := bits.Mul64(remainderTokens, rate)
	fractionCost, remainder := bits.Div64(high, low, uint64(microsPerUSD))
	if remainder != 0 {
		fractionCost++
	}
	if wholeCost > uint64(math.MaxInt64)-fractionCost {
		return 0, fmt.Errorf("token price multiplication overflows")
	}
	return int64(wholeCost + fractionCost), nil
}

func addMicros(values ...int64) (int64, error) {
	total := int64(0)
	for _, value := range values {
		if value < 0 || total > math.MaxInt64-value {
			return 0, fmt.Errorf("cost total overflows")
		}
		total += value
	}
	return total, nil
}
