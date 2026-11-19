package team

import (
	"fmt"
	"slices"
	"strings"
)

// runtimeCompletion selects a runtime input object containing the successful
// status and summary. A worker may instead report an unsuccessful outcome.
func compileRuntimeCompletion(root map[string]any, inputs []resultRuntimeInputSpec) (string, error) {
	value, present := root["x-hufu-runtime-completion"]
	if !present {
		return "", nil
	}
	pointer, ok := value.(string)
	if ok {
		for _, input := range inputs {
			if input.Target == pointer {
				for _, field := range []string{"/status", "/summary"} {
					node, err := runtimeInputSchemaAt(root, pointer+field)
					if err != nil || node["type"] != "string" {
						return "", fmt.Errorf("x-hufu-runtime-completion requires declared status and summary strings")
					}
				}
				return pointer, nil
			}
		}
	}
	return "", fmt.Errorf("x-hufu-runtime-completion must select a runtime input target")
}

func runtimeCompletionFields(value any) (string, string, error) {
	fields, _ := value.(map[string]any)
	status, _ := fields["status"].(string)
	summary, _ := fields["summary"].(string)
	if !taskResultStatusIsSuccessful(status) || strings.TrimSpace(summary) == "" {
		return "", "", fmt.Errorf("runtime completion has invalid status or summary")
	}
	return status, summary, nil
}

func bindRuntimeCompletion(pointer, status, summary string, supplied bool, lookup func() (map[string]any, error)) (string, string, error) {
	if pointer == "" || (status != "" && validWorkerResultProposalStatus(status) && !taskResultStatusIsSuccessful(status)) {
		return status, summary, nil
	}
	if supplied || status != "" || summary != "" {
		return "", "", fmt.Errorf("successful status and summary are runtime-owned; omit both fields")
	}
	if lookup == nil {
		return "", "", fmt.Errorf("runtime completion source is unavailable")
	}
	values, err := lookup()
	if err != nil {
		return "", "", err
	}
	return runtimeCompletionFields(values[pointer])
}

func omitRuntimeCompletionRequirements(parameters map[string]any, required []string) []string {
	parameters["status"] = map[string]any{"type": "string", "enum": []string{TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked},
		"description": "Only for an unsuccessful outcome. Omit status and summary when submitting completed work."}
	parameters["summary"] = map[string]any{"type": "string", "description": "Only for an unsuccessful outcome. Summarize the concrete blocker or failure."}
	return slices.DeleteFunc(required, func(field string) bool {
		return field == "status" || field == "summary" || field == "structured_payload"
	})
}

func runtimeCompletionFailure(compiled *CompiledResultContract, status string) bool {
	return hasRuntimeCompletion(compiled) && validWorkerResultProposalStatus(status) && !taskResultStatusIsSuccessful(status)
}

func hasRuntimeCompletion(compiled *CompiledResultContract) bool {
	return compiled != nil && compiled.runtimeCompletion != ""
}
