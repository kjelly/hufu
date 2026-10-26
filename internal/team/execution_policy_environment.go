package team

import "slices"

func policySnapshotUsesBackend(snapshot *ExecutionPolicySnapshot, backend string) bool {
	return snapshot != nil && slices.ContainsFunc(snapshot.ModelRoutes, func(route ExecutionModelRouteSnapshot) bool {
		return route.Backend == backend
	})
}

// Ambient environment of an unreachable agent backend cannot affect an LLM
// continuation. Agent execution is separately denied unless a frozen route
// names that backend. Compare every other policy field, including executable,
// world, network, contracts and the entire route set, without migrating or
// rewriting the original policy snapshot.
func compatibleUnusedExecutionEnvironment(frozen, current *ExecutionPolicySnapshot) (bool, error) {
	normalized := cloneExecutionPolicySnapshot(current)
	for i := range normalized.ExecutionWorlds {
		world := &normalized.ExecutionWorlds[i]
		if policySnapshotUsesBackend(frozen, world.Backend) || policySnapshotUsesBackend(current, world.Backend) {
			continue
		}
		index := slices.IndexFunc(frozen.ExecutionWorlds, func(prior ExecutionWorldPolicySnapshot) bool {
			return prior.Backend == world.Backend
		})
		if index >= 0 {
			world.InheritEnv = slices.Clone(frozen.ExecutionWorlds[index].InheritEnv)
		}
	}
	hash, err := executionPolicyConfigurationHash(normalized)
	return hash == frozen.ConfigurationHash, err
}
