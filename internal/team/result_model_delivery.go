package team

import (
	"encoding/json/v2"
	"fmt"
	"slices"
)

// Only accepted, hash-verified payloads use this recovery path. Recompute the
// receipt from the original sealed transcript; the caller compares the entire
// canonical payload hash before retaining any accepted result.
func modelDeliveryClaimsForRevalidation(spec *resultToolEvidenceSpec, raw []byte) ([]byte, error) {
	if spec == nil || !spec.RequireModelDelivery {
		return raw, nil
	}
	value, err := decodeResultContractJSON(raw)
	if err != nil {
		return nil, err
	}
	groups, err := resolveJSONPointer(value, spec.GroupsPointer)
	if err != nil {
		return nil, err
	}
	key, _ := evidenceMember(spec.ModelDeliveryPointer)
	if list, ok := groups.([]any); ok {
		for _, group := range list {
			rawItems, err := resolveJSONPointer(group, spec.ItemsPointer)
			if err != nil {
				return nil, err
			}
			if items, ok := rawItems.([]any); ok {
				for _, item := range items {
					if object, ok := item.(map[string]any); ok {
						delete(object, key)
					}
				}
			}
		}
	}
	return json.Marshal(value)
}

func (spec *resultToolEvidenceSpec) validateModelDelivery(root map[string]any) error {
	if spec.RequireModelDelivery != (spec.ModelDeliveryPointer != "") {
		return fmt.Errorf("tool evidence requires require_model_delivery and model_delivery_pointer together")
	}
	if !spec.RequireModelDelivery {
		return nil
	}
	if _, err := evidenceMember(spec.ModelDeliveryPointer); err != nil {
		return err
	}
	used := []string{spec.ValuePointer, spec.QuotePointer, spec.StatusPointer, spec.CallIDPointer, spec.HistoricalPointer}
	if spec.Diagnostics != nil {
		used = append(used, spec.Diagnostics.FetchPointer, spec.Diagnostics.CitationPointer)
	}
	if slices.Contains(used, spec.ModelDeliveryPointer) {
		return fmt.Errorf("model delivery must not overwrite another evidence field")
	}
	pointer := spec.GroupsPointer + "/0" + spec.ItemsPointer + "/0" + spec.ModelDeliveryPointer
	if resultProjectionSchemaAt(root, pointer) == nil {
		return fmt.Errorf("model delivery requires a declared source receipt schema")
	}
	return nil
}

// Remove just the receipt at the declared source location. Unrelated input
// fields with the same name remain model-authored.
func omitModelDeliverySchema(root map[string]any, spec *resultToolEvidenceSpec) {
	if spec == nil || !spec.RequireModelDelivery {
		return
	}
	item := resultProjectionSchemaAt(root, spec.GroupsPointer+"/0"+spec.ItemsPointer+"/0")
	properties, _ := item["properties"].(map[string]any)
	key, _ := evidenceMember(spec.ModelDeliveryPointer)
	delete(properties, key)
	required, _ := item["required"].([]any)
	if required != nil {
		item["required"] = slices.DeleteFunc(required, func(v any) bool { return v == key })
	}
}

func rejectModelDeliveryOverride(value any, spec *resultToolEvidenceSpec) error {
	if spec == nil || !spec.RequireModelDelivery {
		return nil
	}
	raw, err := resolveJSONPointer(value, spec.GroupsPointer)
	if err != nil {
		return nil
	} // Normal schema validation owns shape errors.
	groups, _ := raw.([]any)
	key, _ := evidenceMember(spec.ModelDeliveryPointer)
	for _, group := range groups {
		raw, err := resolveJSONPointer(group, spec.ItemsPointer)
		if err != nil {
			continue
		}
		items, _ := raw.([]any)
		for _, item := range items {
			if object, ok := item.(map[string]any); ok {
				if _, present := object[key]; present {
					return fmt.Errorf("%s is runtime-owned; omit it entirely", spec.ModelDeliveryPointer)
				}
			}
		}
	}
	return nil
}

func bindModelDelivery(item map[string]any, spec *resultToolEvidenceSpec, records []taskTranscriptRecord, observations []successfulToolObservation, match string, index int) (bool, error) {
	if !spec.RequireModelDelivery {
		return true, nil
	}
	receipt := &toolModelInputReceipt{Status: "not_observed"}
	// Prefer the observation whose quote matched. Do not bind a delivery from
	// another snapshot of the same document with different bytes.
	if index >= 0 {
		receipt = observedToolModelInput(records, observations[index].id, observations[index].output)
	} else {
		for _, observation := range observations {
			if observation.match == match {
				receipt = observedToolModelInput(records, observation.id, observation.output)
				break
			}
		}
	}
	value, err := modelDeliveryValue(receipt)
	if err != nil {
		return false, err
	}
	key, _ := evidenceMember(spec.ModelDeliveryPointer)
	item[key] = value
	return receipt.Status == "full", nil
}
