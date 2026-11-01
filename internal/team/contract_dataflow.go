package team

import (
	"fmt"
	"slices"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// lintContractDataflow checks declared producer contracts, without interpreting
// prompts or guessing which dynamic evidence the coordinator will select.
func lintContractDataflow(session *TeamSession) []ContractFinding {
	tasks := make(map[string]TaskDef)
	for _, task := range session.ContractTasks {
		tasks[task.ID] = task
	}
	var findings []ContractFinding
	for i, task := range session.ContractTasks {
		if task.ResultContract == nil {
			continue
		}
		consumer := session.ResultContracts[task.ResultContract.ID]
		if consumer == nil {
			continue
		}
		field := fmt.Sprintf("tasks[%d].result-contract", i)
		for _, collection := range consumer.fixedCollections {
			add := func(code, severity, message string) {
				findings = append(findings, ContractFinding{Code: code, Severity: severity, Field: field, Message: message, Hint: "Declare source_task_contracts and keep downstream source_contracts aligned with producer result contracts."})
			}
			if len(collection.SourceTaskContracts) == 0 {
				add("contract_dataflow_unverified", FindingSeverityWarning, fmt.Sprintf("%s collection %s has dynamic sources without source_task_contracts; producer coverage is unverified", task.ID, collection.ItemsPointer))
			}
			for _, id := range collection.SourceTaskContracts {
				producer, ok := tasks[id]
				if !ok || producer.ResultContract == nil {
					add("contract_dataflow_source_invalid", FindingSeverityError, fmt.Sprintf("%s collection %s source task contract %q is missing or has no result contract", task.ID, collection.ItemsPointer, id))
					continue
				}
				if !slices.Contains(collection.SourceContracts, producer.ResultContract.ID) {
					add("contract_dataflow_source_rejected", FindingSeverityError, fmt.Sprintf("%s collection %s rejects producer %s result contract %s", task.ID, collection.ItemsPointer, id, producer.ResultContract.ID))
				}
			}
			for _, id := range collection.SourceContracts {
				producer := session.ResultContracts[id]
				if producer == nil {
					continue
				} // The schema loader reports absent contracts.
				status := collectionSourceArray(producer.schema, collection.SourcePointer)
				switch status {
				case "invalid":
					add("contract_dataflow_pointer_invalid", FindingSeverityError, fmt.Sprintf("%s collection %s source %s pointer %s is not a declared array", task.ID, collection.ItemsPointer, id, collection.SourcePointer))
				case "unknown":
					add("contract_dataflow_pointer_unverified", FindingSeverityWarning, fmt.Sprintf("%s source %s pointer %s cannot be statically verified (open, composed or recursive schema)", task.ID, id, collection.SourcePointer))
				}
			}
		}
	}
	return findings
}

// Only prove simple property paths with bounded local references. Composition
// without a direct type/property constraint is reported as unknown.
func collectionSourceArray(root *jsonschema.Schema, pointer string) string {
	node := root
	tokens := fixedCollectionPointerTokens(pointer)
	for step := 0; step < 128; step++ {
		if node == nil {
			return "invalid"
		}
		composed := len(node.AllOf) > 0 || len(node.AnyOf) > 0 || len(node.OneOf) > 0 || node.If != nil || node.DynamicRef != nil
		if node.Ref != nil {
			if composed || node.Types != nil || len(node.Properties) > 0 {
				return "unknown"
			}
			node = node.Ref
			continue
		}
		if len(tokens) == 0 {
			if node.Types == nil {
				return "unknown"
			}
			if slices.Equal(node.Types.ToStrings(), []string{"array"}) {
				return "valid"
			}
			return "invalid"
		}
		next, ok := node.Properties[tokens[0]]
		if !ok {
			if composed || node.AdditionalProperties != false || node.Types == nil {
				return "unknown"
			}
			return "invalid"
		}
		node, tokens = next, tokens[1:]
	}
	return "unknown"
}
