package team

import (
	"encoding/json/v2"
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/modelprofile"
)

// Construction and transport verification must compare the same projection,
// including the target bound by the runtime after profile resolution.
func providerInvocationProfileProjection(request agent.ProviderRequest) (modelprofile.TelemetryProjection, error) {
	var projection modelprofile.TelemetryProjection
	if err := json.Unmarshal([]byte(request.AdmissionContext.ProfileTelemetryJSON), &projection); err != nil {
		return projection, fmt.Errorf("decode provider profile commit projection: %w", err)
	}
	projection.InvocationID = request.InvocationID
	if projection.APIProtocol != request.AdmissionContext.APIProtocol {
		return projection, fmt.Errorf("provider profile API protocol differs from invocation binding")
	}
	if projection.ExecutionTarget != "" {
		return projection, fmt.Errorf("provider profile input overrides runtime execution target")
	}
	target, known, err := boundProviderInvocationTarget(request)
	if err != nil {
		return projection, err
	}
	if known {
		projection.ExecutionTarget = target.String()
	}
	return projection, nil
}

// boundProviderInvocationTarget translates only the immutable provider binding
// and the provider's selector syntax. It never resolves a bare model against
// the coordinator's default backend after the transport has been selected.
// Legacy unknown-prefix fallbacks cannot prove the transport's model name.
func boundProviderInvocationTarget(request agent.ProviderRequest) (execution.ExecutionTarget, bool, error) {
	bound := request.AdmissionContext
	if bound.ModelID != "" && bound.ModelID != request.ModelID {
		return execution.ExecutionTarget{}, false, fmt.Errorf("provider invocation model differs from immutable binding")
	}
	backend := execution.CanonicalTargetBackendName(bound.ProviderIdentity)
	prefix, model := agent.ParseModelProvider(request.ModelID)
	if backend == "" || (prefix != "" && execution.CanonicalTargetBackendName(prefix) != backend) {
		return execution.ExecutionTarget{}, false, nil
	}
	target := execution.ExecutionTarget{Backend: backend, Model: model}
	if err := target.Validate(); err != nil {
		return execution.ExecutionTarget{}, false, err
	}
	return target, true, nil
}
