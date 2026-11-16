package taskdistill

// ActionExtractOnly is derived by the runtime from approved distill fragments
// and retained fragments of the same source. Models cannot submit this action.
const ActionExtractOnly = "extract_only"

// RetainsSource is used only after canonical draft validation. Both actions
// keep the original task or workspace; extract_only also preserves approved
// knowledge from independent distill fragments.
func RetainsSource(action string) bool {
	return action == "retain" || action == ActionExtractOnly
}

func allowsKnowledge(action string) bool {
	return action == "distill" || action == ActionExtractOnly
}

// Join already validated fragment decisions without changing their judgments.
// Retention and approved knowledge are independent, and discard is neutral.
func combineFragmentActions(current, next string) string {
	retained := RetainsSource(current) || RetainsSource(next)
	extracted := allowsKnowledge(current) || allowsKnowledge(next)
	switch {
	case retained && extracted:
		return ActionExtractOnly
	case retained:
		return "retain"
	case extracted:
		return "distill"
	default:
		return "discard"
	}
}

func decodeCheckpointDraft(raw string, sources []Source, schema string) (Draft, error) {
	// Enforce the canonical byte bound before parsing a versioned contract.
	draft, err := decodeDraft(raw, sources)
	if err != nil {
		return Draft{}, err
	}
	if schema != Schema {
		if err := validateJSONSchema([]byte(raw), schema); err != nil {
			return Draft{}, err
		}
	}
	return draft, nil
}
