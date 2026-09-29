package cost

import "fmt"

func EstimateUsageCost(price PriceSnapshot, usage TokenUsage) (Estimate, error) {
	if err := validateUsage(usage); err != nil {
		return Estimate{}, err
	}
	switch price.BillingMode {
	case BillingLocal:
		return Estimate{Source: EstimateNotMetered, BillingMode: BillingLocal}, nil
	case BillingSubscription:
		return Estimate{Source: EstimateSubscription, BillingMode: BillingSubscription}, nil
	case BillingUnknown:
		return Estimate{Source: EstimateUnknown, BillingMode: BillingUnknown}, nil
	case BillingMetered:
	default:
		return Estimate{}, fmt.Errorf("unsupported billing mode %q", price.BillingMode)
	}
	if !hasTokenPrice(price) {
		return EstimateOpaqueBound(price)
	}
	input, err := TokenRateMicros(usage.InputTokens, *price.InputMicrosPerMillion)
	if err != nil {
		return Estimate{}, err
	}
	cacheRead, err := TokenRateMicros(usage.CacheReadTokens, *price.CacheReadMicrosPerMillion)
	if err != nil {
		return Estimate{}, err
	}
	cacheWrite, err := TokenRateMicros(usage.CacheCreationTokens, *price.CacheWriteMicrosPerMillion)
	if err != nil {
		return Estimate{}, err
	}
	output, err := TokenRateMicros(usage.OutputTokens, *price.OutputMicrosPerMillion)
	if err != nil {
		return Estimate{}, err
	}
	total, err := addMicros(input, cacheRead, cacheWrite, output)
	if err != nil {
		return Estimate{}, err
	}
	return Estimate{Micros: new(total), Source: EstimateUsage, BillingMode: BillingMetered}, nil
}

func EstimateTokenAdmissionBound(price PriceSnapshot, inputTokens, maxOutputTokens int64) (Estimate, error) {
	if price.BillingMode != BillingMetered || !hasTokenPrice(price) {
		return Estimate{}, fmt.Errorf("metered token rates are required for token admission")
	}
	if inputTokens < 0 || maxOutputTokens < 0 {
		return Estimate{}, fmt.Errorf("admission token counts must be non-negative")
	}
	inputRate := max(*price.InputMicrosPerMillion, *price.CacheReadMicrosPerMillion, *price.CacheWriteMicrosPerMillion)
	input, err := TokenRateMicros(inputTokens, inputRate)
	if err != nil {
		return Estimate{}, err
	}
	output, err := TokenRateMicros(maxOutputTokens, *price.OutputMicrosPerMillion)
	if err != nil {
		return Estimate{}, err
	}
	total, err := addMicros(input, output)
	if err != nil {
		return Estimate{}, err
	}
	return Estimate{Micros: new(total), Source: EstimateAdmissionBound, BillingMode: BillingMetered}, nil
}

func EstimateOpaqueBound(price PriceSnapshot) (Estimate, error) {
	if price.BillingMode != BillingMetered || price.OpaqueMaxMicrosPerInvocation == nil {
		return Estimate{}, fmt.Errorf("metered opaque maximum is required for opaque admission")
	}
	return Estimate{Micros: new(*price.OpaqueMaxMicrosPerInvocation), Source: EstimateAdmissionBound, BillingMode: BillingMetered}, nil
}

func hasTokenPrice(price PriceSnapshot) bool {
	return price.InputMicrosPerMillion != nil && price.CacheReadMicrosPerMillion != nil &&
		price.CacheWriteMicrosPerMillion != nil && price.OutputMicrosPerMillion != nil
}

func validateUsage(usage TokenUsage) error {
	if usage.InputTokens < 0 || usage.CacheReadTokens < 0 || usage.CacheCreationTokens < 0 || usage.OutputTokens < 0 || usage.TotalTokens < 0 {
		return fmt.Errorf("token usage must be non-negative")
	}
	return nil
}
