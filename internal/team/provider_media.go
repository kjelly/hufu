package team

import "github.com/kjelly/hufu/internal/agent"

func providerRequestHasMedia(request agent.ProviderRequest) bool {
	return agent.ProviderRequestHasMedia(request)
}
