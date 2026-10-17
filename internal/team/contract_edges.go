package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

func staticContractEdgesHash(session *TeamSession) (string, error) {
	participants := make(map[string]bool)
	for _, contract := range session.ContractTasks {
		for _, target := range append(append([]string(nil), contract.DependsOnContracts...), contract.OnFailureContract) {
			if target != "" {
				participants[contractEdgeKey(contract.ID)] = true
				participants[contractEdgeKey(target)] = true
			}
		}
	}
	if len(participants) == 0 {
		return "", nil
	}
	if err := validateStaticContractEdges(session); err != nil {
		return "", err
	}
	var identities []string
	for _, contract := range session.ContractTasks {
		if !participants[contractEdgeKey(contract.ID)] {
			continue
		}
		hash, err := effectiveContractHash(contract.ID, contract.Agent, contract.Execution, contract.OutputMode, contract.SideEffect, contract.Recovery, contract.MaxRetries, contract.Action, contract.FanOut, contract.Optional, contract)
		if err != nil {
			return "", err
		}
		identities = append(identities, hash)
	}
	slices.Sort(identities)
	encoded, err := json.Marshal(identities)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func contractEdgeKey(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// A connected set of named edges is an atomic dispatch group. Neither a
// producer nor its verifier can be admitted alone and left without its peer.
// Only the resolved scheduler edges are persisted on the task occurrence.
func bindStaticContractEdges(session *TeamSession, tasks []TaskDef) error {
	declared := make(map[string]TaskDef, len(session.ContractTasks))
	participants := make(map[string]bool)
	for _, contract := range session.ContractTasks {
		declared[contractEdgeKey(contract.ID)] = contract
		for _, target := range append(append([]string(nil), contract.DependsOnContracts...), contract.OnFailureContract) {
			if target != "" {
				participants[contractEdgeKey(contract.ID)] = true
				participants[contractEdgeKey(target)] = true
			}
		}
	}
	indices := make(map[string][]int)
	for i, task := range tasks {
		if task.CatalogAction != nil {
			continue
		}
		key := contractEdgeKey(task.ContractID)
		if participants[key] {
			indices[key] = append(indices[key], i)
		}
	}
	for source, contract := range declared {
		for _, target := range append(append([]string(nil), contract.DependsOnContracts...), contract.OnFailureContract) {
			if target == "" {
				continue
			}
			key := contractEdgeKey(target)
			if len(indices[source]) == 0 && len(indices[key]) == 0 {
				continue
			}
			if len(indices[source]) != 1 || len(indices[key]) != 1 {
				return fmt.Errorf("static workflow edge %q -> %q requires exactly one task for each contract in the same batch", contract.ID, target)
			}
		}
	}
	for key, positions := range indices {
		if len(positions) != 1 {
			return fmt.Errorf("static workflow contract %q must appear exactly once in the batch", key)
		}
		contract, ok := declared[key]
		if !ok {
			return fmt.Errorf("static workflow contract %q is not configured", key)
		}
		task := &tasks[positions[0]]
		// These contracts own all scheduling edges, including the producer's
		// empty edges. Do not retain model-authored indices or pipeline flags.
		task.DependsOn, task.OnFailure, task.Pipeline = nil, nil, false
		for _, target := range contract.DependsOnContracts {
			task.DependsOn = append(task.DependsOn, indices[contractEdgeKey(target)][0])
		}
		if contract.OnFailureContract != "" {
			task.OnFailure = new(indices[contractEdgeKey(contract.OnFailureContract)][0])
		}
	}
	return nil
}

func validateStaticContractEdges(session *TeamSession) error {
	declared := make(map[string]TaskDef, len(session.ContractTasks))
	for _, contract := range session.ContractTasks {
		declared[contractEdgeKey(contract.ID)] = contract
	}
	hasEdges := false
	for _, contract := range session.ContractTasks {
		hasEdges = hasEdges || len(contract.DependsOnContracts) > 0 || contract.OnFailureContract != ""
		seen := make(map[string]bool)
		for _, target := range contract.DependsOnContracts {
			key := contractEdgeKey(target)
			if key == "" || seen[key] || key == contractEdgeKey(contract.ID) {
				return fmt.Errorf("contract %q has an empty, duplicate, or self dependency", contract.ID)
			}
			seen[key] = true
		}
		for _, target := range append(append([]string(nil), contract.DependsOnContracts...), contract.OnFailureContract) {
			if target == "" {
				continue
			}
			peer, ok := declared[contractEdgeKey(target)]
			if !ok {
				return fmt.Errorf("contract %q references unknown workflow contract %q", contract.ID, target)
			}
			if contract.ID == "" || contract.FanOut != nil || peer.FanOut != nil || contract.Phase != peer.Phase {
				return fmt.Errorf("static workflow edge %q -> %q requires named, non-fan-out contracts in the same phase", contract.ID, target)
			}
		}
	}
	if !hasEdges {
		return nil
	}
	if !session.Config.Delegation.BindTaskGoalContracts && !session.Config.Delegation.BindInitialTaskContracts {
		return fmt.Errorf("named workflow edges require static contract binding")
	}
	tasks := append([]TaskDef(nil), session.ContractTasks...)
	for i := range tasks {
		tasks[i].ContractID = tasks[i].ID
		// Numeric dispatch edges on unrelated declarations are not part of
		// this named graph and are not inherited by static contract binding.
		tasks[i].DependsOn, tasks[i].OnFailure, tasks[i].Pipeline = nil, nil, false
	}
	if err := bindStaticContractEdges(session, tasks); err != nil {
		return err
	}
	if detectTaskCycle(tasks) {
		return fmt.Errorf("static workflow contracts contain a dependency cycle")
	}
	return validateOnFailureTargets(tasks)
}
