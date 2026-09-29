package cost

// BillingMode describes how an execution target is paid for.
type BillingMode string

const (
	BillingMetered      BillingMode = "metered"
	BillingLocal        BillingMode = "local"
	BillingSubscription BillingMode = "subscription"
	BillingUnknown      BillingMode = "unknown"
)

// EstimateSource preserves whether a number came from observed usage or a
// conservative pre-transport bound. Non-metered and unknown calls remain
// explicit rather than being folded into numeric zero.
type EstimateSource string

const (
	EstimateUsage          EstimateSource = "usage"
	EstimateAdmissionBound EstimateSource = "admission_bound"
	EstimateNotMetered     EstimateSource = "not_metered"
	EstimateSubscription   EstimateSource = "subscription"
	EstimateUnknown        EstimateSource = "unknown"
)

type TokenUsage struct {
	InputTokens         int64 `json:"input_tokens,omitzero"`
	CacheReadTokens     int64 `json:"cache_read_tokens,omitzero"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitzero"`
	OutputTokens        int64 `json:"output_tokens,omitzero"`
	TotalTokens         int64 `json:"total_tokens,omitzero"`
}

type PriceSnapshot struct {
	ID                           string      `json:"id"`
	ExecutionTarget              string      `json:"execution_target"`
	BillingMode                  BillingMode `json:"billing_mode"`
	InputMicrosPerMillion        *int64      `json:"input_micros_per_million,omitempty"`
	CacheReadMicrosPerMillion    *int64      `json:"cache_read_micros_per_million,omitempty"`
	CacheWriteMicrosPerMillion   *int64      `json:"cache_write_micros_per_million,omitempty"`
	OutputMicrosPerMillion       *int64      `json:"output_micros_per_million,omitempty"`
	OpaqueMaxMicrosPerInvocation *int64      `json:"opaque_max_micros_per_invocation,omitempty"`
	CatalogHash                  string      `json:"catalog_hash"`
}

type Estimate struct {
	Micros      *int64
	Source      EstimateSource
	BillingMode BillingMode
}
