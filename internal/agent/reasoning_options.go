package agent

import (
	"maps"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/kjelly/hufu/internal/config"
)

// ReasoningEffortProviderOptions retains the original constructor while
// selecting the option type/key from the immutable transport binding.
func ReasoningEffortProviderOptions(model fantasy.LanguageModel, effort string) fantasy.ProviderOptions {
	return MergeReasoningEffortProviderOptions(model, nil, effort)
}

// MergeReasoningEffortProviderOptions copies the option map and the changed
// provider option struct, preserving all unrelated caller-owned fields.
func MergeReasoningEffortProviderOptions(model fantasy.LanguageModel, existing fantasy.ProviderOptions, effort string) fantasy.ProviderOptions {
	options := maps.Clone(existing)
	if options == nil {
		options = make(fantasy.ProviderOptions)
	}
	value := new(openai.ReasoningEffort(effort))
	if ModelAPIProtocol(model).Effective == config.APIProtocolResponses {
		var responseOptions openai.ResponsesProviderOptions
		if old, ok := options[openai.Name].(*openai.ResponsesProviderOptions); ok && old != nil {
			responseOptions = *old
		}
		responseOptions.ReasoningEffort = value
		options[openai.Name] = &responseOptions
		return options
	}
	name := openaicompat.Name
	if model != nil && model.Provider() != "" {
		name = model.Provider()
	}
	var chatOptions openaicompat.ProviderOptions
	if old, ok := options[name].(*openaicompat.ProviderOptions); ok && old != nil {
		chatOptions = *old
	}
	chatOptions.ReasoningEffort = value
	options[name] = &chatOptions
	return options
}
