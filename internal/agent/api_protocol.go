package agent

import (
	"errors"

	"github.com/kjelly/hufu/internal/config"
)

// ErrResponsesNotEnabled fences production construction until admission,
// accounting, durable binding and integration gates have been implemented.
var ErrResponsesNotEnabled = errors.New("api-protocol responses is not enabled yet")

// APIProtocolResolution is copied into provider-owned transport policy. It
// never interprets a model name, endpoint name, or model-authored input.
type APIProtocolResolution struct {
	Requested string `json:"requested,omitempty"`
	Effective string `json:"effective"`
	Source    string `json:"source"`
	Version   int    `json:"version"`
}

func ResolveAPIProtocol(raw string) (APIProtocolResolution, error) {
	requested, err := config.NormalizeAPIProtocol(raw)
	if err != nil {
		return APIProtocolResolution{}, err
	}
	resolution := APIProtocolResolution{Requested: requested, Effective: requested, Source: "explicit", Version: 1}
	if requested == "" {
		resolution.Effective = config.APIProtocolChatCompletions
		resolution.Source = "legacy"
	}
	return resolution, nil
}
