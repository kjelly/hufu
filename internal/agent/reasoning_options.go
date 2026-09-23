package agent

import (
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
)

// ReasoningEffortProviderOptions builds openai-compatible provider options
// carrying a reasoning effort, keyed by the model's provider name. In
// charm.land/fantasy v0.41.1, providers/openaicompat/language_model_hooks.go's
// PrepareCallFunc reads call.ProviderOptions[model.Provider()]. The model
// reports the configured provider name (for example "local" or "ollama");
// keying by openaicompat.Name silently dropped the effort for other names.
func ReasoningEffortProviderOptions(model fantasy.LanguageModel, effort string) fantasy.ProviderOptions {
	name := openaicompat.Name
	if model != nil && model.Provider() != "" {
		name = model.Provider()
	}
	return fantasy.ProviderOptions{name: &openaicompat.ProviderOptions{ReasoningEffort: new(openai.ReasoningEffort(effort))}}
}
