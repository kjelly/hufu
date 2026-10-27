package team

// Only duplicate representations of findings and attested invariant claims
// qualify as format repair. Other assertion failures remain contract failures.
// Never retry admission that may already have materialized artifact declarations.
func collectionProjectionFormatFailure(contract taskResultSubmissionContract, result *TaskResult) bool {
	if result == nil || len(result.Artifacts) != 0 {
		return false
	}
	document, err := canonicalTaskResultDocument(result)
	if err != nil {
		return false
	}
	found := false
	for _, assertion := range contract.SubmissionTaskResultAssertions {
		if len(evaluateTaskResultAssertions(document, []TaskResultAssertion{assertion})) == 0 {
			continue
		}
		if assertion.Op != "equals_projection" || (assertion.Pointer != "/findings" && assertion.Pointer != "/invariant_verification/assessments") {
			return false
		}
		projection, err := decodeTaskResultProjection(assertion.Value)
		if err != nil || taskResultAssertionRootField(projection.Pointer) != "structured_payload" {
			return false
		}
		found = true
	}
	return found
}
