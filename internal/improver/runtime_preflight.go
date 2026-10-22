package improver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/providerproxy"
)

// Preflight resolves every configured role and compares each frozen arm before
// the controller admits work. Its temporary runtimes never invoke a provider;
// their physical policy hashes are retained only by their own disposable stores.
func (f *RuntimeFactory) Preflight() (models map[string]paired.ResolvedModel, retErr error) {
	root, err := os.MkdirTemp("", "hufu-paired-policy-")
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(root)) }()
	var boundary agent.ProviderBoundaryFactory
	if f.config.ExecutionMode == "fixture" {
		boundary = func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
			return nil, errors.New("preflight cannot start a provider")
		}
	}
	models = map[string]paired.ResolvedModel{}
	for _, role := range slices.Sorted(maps.Keys(f.overlays)) {
		var policyDigest string
		for i, slot := range slices.Sorted(maps.Keys(f.variants)) {
			runtime, err := f.open(filepath.Join(root, fmt.Sprintf("%s_%d", role, i)), slot, role, boundary)
			if err != nil {
				return nil, err
			}
			digest, digestErr := runtimePolicyDigest(runtime.policy, runtime.overlay)
			closeErr := runtime.close()
			if err := errors.Join(digestErr, closeErr); err != nil {
				return nil, err
			}
			if policyDigest != "" && policyDigest != digest {
				return nil, errors.New("protected_surface: resolved runtime policies differ between arms")
			}
			policyDigest = digest
		}
		overlay := f.overlays[role]
		requested := f.config.Models[role]
		models[role] = paired.ResolvedModel{
			Backend: requested.Backend, Model: requested.Model, PolicyDigest: policyDigest,
			Generation: map[string]string{
				"runtime_model":  overlay.Generation.Model,
				"context_window": strconv.Itoa(overlay.Generation.ContextWindow),
				"temperature":    overlay.Generation.Temperature, "max_output": overlay.Generation.MaxTokens,
				"top_p": overlay.Generation.TopP, "top_k": overlay.Generation.TopK, "reasoning_effort": overlay.Generation.ReasoningEffort,
			},
		}
	}
	return models, nil
}

func runtimePolicyDigest(policy PolicyProjection, overlay RuntimeOverlay) (string, error) {
	return projectionDigest("runtime_profile", map[string]any{"execution_policy": policy.Policy, "overlay": overlay})
}
