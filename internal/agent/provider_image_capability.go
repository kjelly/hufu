package agent

// SupportsNativeImageInput describes the pinned OpenAI-compatible adapter's
// encoder, independently of any model or backend claim. It grants no media
// access or destination authorization.
func (p *OpenAICompatibleProvider) SupportsNativeImageInput() bool {
	return p != nil && p.provider != nil
}
