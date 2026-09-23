package agent

import (
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
)

// ReasoningEffortProviderOptions builds openai-compatible provider options
// carrying a reasoning effort, keyed by model's provider name. openaicompat's
// PrepareCallFunc looks options up by model.Provider(), which is the
// configured provider name (for example "local" or "ollama"), not
// openaicompat.Name; keying by openaicompat.Name silently dropped the effort
// for every provider with another name.
func ReasoningEffortProviderOptions(model fantasy.LanguageModel, effort string) fantasy.ProviderOptions {
	name := openaicompat.Name
	if model != nil && model.Provider() != "" {
		name = model.Provider()
	}
	return fantasy.ProviderOptions{name: &openaicompat.ProviderOptions{ReasoningEffort: new(openai.ReasoningEffort(effort))}}
}
