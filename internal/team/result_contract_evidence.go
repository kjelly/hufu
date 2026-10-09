package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

// Vocabulary and fallback values belong to the owning schema. Core only
// matches declared JSON fields against successful, runner-owned observations.
type resultToolEvidenceSpec struct {
	GroupsPointer    string   `json:"groups_pointer"`
	ItemsPointer     string   `json:"items_pointer"`
	Tool             string   `json:"tool"`
	Tools            []string `json:"tools,omitempty"`
	OutputFormat     string   `json:"output_format,omitempty"`
	TextPattern      string   `json:"text_pattern,omitempty"`
	TargetPattern    string   `json:"target_pattern,omitempty"`
	textPattern      *regexp.Regexp
	targetPattern    *regexp.Regexp
	InputPointer     string                       `json:"input_pointer"`
	ValuePointer     string                       `json:"value_pointer"`
	OutputPointer    string                       `json:"output_pointer"`
	QuotePointer     string                       `json:"quote_pointer"`
	StatusPointer    string                       `json:"status_pointer"`
	VerifiedStatus   string                       `json:"verified_status"`
	UnverifiedStatus string                       `json:"unverified_status"`
	CallIDPointer    string                       `json:"call_id_pointer"`
	GroupFallback    map[string]any               `json:"group_fallback"`
	GroupAppend      map[string]string            `json:"group_append,omitempty"`
	Diagnostics      *resultEvidenceDiagnostics   `json:"diagnostics,omitempty"`
	Corroboration    *resultEvidenceCorroboration `json:"corroboration,omitempty"`
}

type resultEvidenceCorroboration struct {
	GroupPointer  string `json:"group_pointer"`
	GroupValue    string `json:"group_value"`
	OriginPointer string `json:"origin_pointer"`
	Minimum       int    `json:"minimum"`
}

// The schema owns the vocabulary; runtime supplies only observation outcomes.
type resultEvidenceDiagnostics struct {
	FetchPointer    string            `json:"fetch_pointer"`
	CitationPointer string            `json:"citation_pointer"`
	FetchValues     map[string]string `json:"fetch_values"`
	CitationValues  map[string]string `json:"citation_values"`
}

func (d *resultEvidenceDiagnostics) validate() error {
	if d == nil {
		return nil
	}
	for _, pointer := range []string{d.FetchPointer, d.CitationPointer} {
		if _, err := evidenceMember(pointer); err != nil {
			return err
		}
	}
	if d.FetchPointer == d.CitationPointer {
		return fmt.Errorf("evidence diagnostic pointers must be distinct")
	}
	for _, field := range []struct {
		values map[string]string
		keys   []string
	}{
		{d.FetchValues, []string{"succeeded", "failed", "unusable", "absent", "pending"}},
		{d.CitationValues, []string{"matched", "mismatch", "empty", "unavailable"}},
	} {
		if len(field.values) != len(field.keys) {
			return fmt.Errorf("evidence diagnostics require all outcome values")
		}
		seen := make(map[string]bool)
		for _, key := range field.keys {
			value := field.values[key]
			if strings.TrimSpace(value) == "" || seen[value] {
				return fmt.Errorf("evidence diagnostic outcome values must be non-empty and distinct")
			}
			seen[value] = true
		}
	}
	return nil
}

func compileResultContractExtensions(root map[string]any) (*resultToolEvidenceSpec, *template.Template, error) {
	var spec *resultToolEvidenceSpec
	if raw, present := root["x-hufu-tool-evidence"]; present {
		data, err := json.Marshal(raw)
		if err != nil {
			return nil, nil, err
		}
		spec = new(resultToolEvidenceSpec)
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(spec); err != nil {
			return nil, nil, fmt.Errorf("x-hufu-tool-evidence: %w", err)
		}
		for _, pointer := range []string{spec.GroupsPointer, spec.ItemsPointer, spec.ValuePointer, spec.QuotePointer, spec.StatusPointer, spec.CallIDPointer} {
			if pointer == "" {
				return nil, nil, fmt.Errorf("x-hufu-tool-evidence requires all pointer fields")
			}
			if err := validateJSONPointer(pointer); err != nil {
				return nil, nil, err
			}
		}
		// Writes are intentionally confined to a single object member. This
		// avoids ambiguous array mutations and ancestor replacement.
		for _, pointer := range []string{spec.StatusPointer, spec.CallIDPointer} {
			if _, err := evidenceMember(pointer); err != nil {
				return nil, nil, err
			}
		}
		if _, err := evidenceMember(spec.QuotePointer); err != nil {
			return nil, nil, err
		}
		if err := spec.validateOutput(); err != nil {
			return nil, nil, err
		}
		if spec.VerifiedStatus == "" || spec.UnverifiedStatus == "" || spec.VerifiedStatus == spec.UnverifiedStatus || len(spec.GroupFallback) == 0 {
			return nil, nil, fmt.Errorf("x-hufu-tool-evidence requires a tool, distinct statuses and a group fallback")
		}
		for pointer := range spec.GroupFallback {
			if _, err := evidenceMember(pointer); err != nil {
				return nil, nil, err
			}
		}
		for pointer := range spec.GroupAppend {
			if _, err := evidenceMember(pointer); err != nil {
				return nil, nil, err
			}
		}
		if err := spec.Diagnostics.validate(); err != nil {
			return nil, nil, err
		}
		if spec.Diagnostics != nil {
			for _, pointer := range []string{spec.Diagnostics.FetchPointer, spec.Diagnostics.CitationPointer} {
				if slices.Contains([]string{spec.ValuePointer, spec.QuotePointer, spec.StatusPointer, spec.CallIDPointer}, pointer) {
					return nil, nil, fmt.Errorf("evidence diagnostics must not overwrite source identity, quote or binding")
				}
			}
		}
		if policy := spec.Corroboration; policy != nil {
			if policy.GroupValue == "" || policy.Minimum < 2 || policy.Minimum > 16 {
				return nil, nil, fmt.Errorf("corroboration requires a value and a minimum between 2 and 16")
			}
			for _, pointer := range []string{policy.GroupPointer, policy.OriginPointer} {
				if pointer == "" {
					return nil, nil, fmt.Errorf("corroboration requires non-empty pointers")
				}
				if err := validateJSONPointer(pointer); err != nil {
					return nil, nil, err
				}
			}
			fallback, present := spec.GroupFallback[policy.GroupPointer]
			if !present || fallback == policy.GroupValue {
				return nil, nil, fmt.Errorf("corroboration requires group_fallback for %q to differ from group_value %q", policy.GroupPointer, policy.GroupValue)
			}
		}
	}
	var report *template.Template
	if raw, present := root["x-hufu-final-report"]; present {
		text, ok := raw.(string)
		if !ok || strings.TrimSpace(text) == "" || len(text) > 8<<10 {
			return nil, nil, fmt.Errorf("x-hufu-final-report must be a non-empty template of at most 8192 bytes")
		}
		var err error
		report, err = template.New("final-report").Option("missingkey=error").Parse(text)
		if err != nil {
			return nil, nil, fmt.Errorf("x-hufu-final-report: %w", err)
		}
	}
	return spec, report, nil
}

func evidenceMember(pointer string) (string, error) {
	if err := validateJSONPointer(pointer); err != nil {
		return "", err
	}
	if !strings.HasPrefix(pointer, "/") || strings.Contains(pointer[1:], "/") {
		return "", fmt.Errorf("evidence write pointer %q must name one object member", pointer)
	}
	return strings.ReplaceAll(strings.ReplaceAll(pointer[1:], "~1", "/"), "~0", "~"), nil
}

func evidenceString(value any, pointer string) string {
	v, err := resolveJSONPointer(value, pointer)
	if err != nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

type successfulToolObservation struct{ id, match, output string }

func (spec *resultToolEvidenceSpec) matchesTool(name string) bool {
	return name != "" && (name == spec.Tool || slices.Contains(spec.Tools, name))
}

func (spec *resultToolEvidenceSpec) validateOutput() error {
	if (spec.InputPointer == "") == (spec.TargetPattern == "") {
		return fmt.Errorf("tool evidence requires exactly one of input_pointer or target_pattern")
	}
	if spec.InputPointer != "" {
		if err := validateJSONPointer(spec.InputPointer); err != nil {
			return err
		}
	}
	if (strings.TrimSpace(spec.Tool) == "") == (len(spec.Tools) == 0) {
		return fmt.Errorf("tool evidence requires exactly one of tool or tools")
	}
	seen := make(map[string]bool)
	for _, tool := range spec.Tools {
		if tool == "" || tool != strings.TrimSpace(tool) || seen[tool] {
			return fmt.Errorf("tool evidence tools must be non-empty, exact and unique")
		}
		seen[tool] = true
	}
	switch spec.OutputFormat {
	case "", "json":
		if spec.OutputPointer == "" || spec.TextPattern != "" || spec.TargetPattern != "" {
			return fmt.Errorf("JSON tool evidence requires output_pointer and does not accept text patterns")
		}
		return validateJSONPointer(spec.OutputPointer)
	case "text":
		if spec.OutputPointer != "" {
			return fmt.Errorf("text tool evidence does not accept output_pointer")
		}
		for _, field := range []struct {
			raw         string
			destination **regexp.Regexp
		}{
			{spec.TextPattern, &spec.textPattern}, {spec.TargetPattern, &spec.targetPattern},
		} {
			if field.raw == "" {
				continue
			}
			pattern, err := regexp.Compile(field.raw)
			if len(field.raw) > 4096 || err != nil || pattern.NumSubexp() != 1 {
				return fmt.Errorf("tool evidence text patterns require a valid regexp with exactly one capture group, at most 4096 bytes")
			}
			*field.destination = pattern
		}
		return nil
	default:
		return fmt.Errorf("tool evidence output_format must be json or text")
	}
}

func (spec *resultToolEvidenceSpec) observedTarget(input any, output string) string {
	if spec.targetPattern != nil {
		matches := spec.targetPattern.FindAllStringSubmatch(output, 2)
		if len(matches) != 1 {
			return ""
		}
		return matches[0][1]
	}
	return evidenceString(input, spec.InputPointer)
}

func (spec *resultToolEvidenceSpec) observedText(output string) string {
	if spec.OutputFormat == "text" {
		if spec.textPattern != nil {
			matches := spec.textPattern.FindAllStringSubmatch(output, 2)
			if len(matches) != 1 {
				return ""
			}
			return matches[0][1]
		}
		return output
	}
	value, err := decodeResultContractJSON([]byte(output))
	if err != nil {
		return ""
	}
	return evidenceString(value, spec.OutputPointer)
}

func matchingToolObservations(spec *resultToolEvidenceSpec, records []taskTranscriptRecord) []successfulToolObservation {
	type observationKey struct{ tool, id string }
	calls := make(map[observationKey]taskTranscriptRecord)
	callCounts, resultCounts := make(map[observationKey]int), make(map[observationKey]int)
	for _, record := range records {
		key := observationKey{record.Tool, record.ToolCallID}
		switch record.Event {
		case "tool_call":
			callCounts[key]++
		case "tool_result":
			resultCounts[key]++
		}
	}
	var observations []successfulToolObservation
	for _, record := range records {
		key := observationKey{record.Tool, record.ToolCallID}
		if !spec.matchesTool(record.Tool) || record.ToolCallID == "" || callCounts[key] != 1 || resultCounts[key] != 1 {
			continue
		}
		if record.Event == "tool_call" {
			calls[observationKey{record.Tool, record.ToolCallID}] = record
			continue
		}
		call, ok := calls[observationKey{record.Tool, record.ToolCallID}]
		if !ok || record.Event != "tool_result" || record.Error {
			continue
		}
		input, err := decodeResultContractJSON([]byte(call.Input))
		if err != nil {
			continue
		}
		match := spec.observedTarget(input, record.Output)
		body := spec.observedText(record.Output)
		if match != "" && strings.TrimSpace(body) != "" {
			observations = append(observations, successfulToolObservation{record.ToolCallID, match, body})
		}
	}
	return observations
}

func bindResultEvidenceDiagnostics(item map[string]any, spec *resultToolEvidenceSpec, observations []successfulToolObservation, records []taskTranscriptRecord, match, quote string, index int) bool {
	callKey, _ := evidenceMember(spec.CallIDPointer)
	statusKey, _ := evidenceMember(spec.StatusPointer)
	observed := slices.IndexFunc(observations, func(o successfulToolObservation) bool { return o.match == match })
	fetch, citation := "absent", "unavailable"
	if observed >= 0 {
		fetch, citation = "succeeded", "mismatch"
		item[callKey] = observations[observed].id
		if strings.TrimSpace(quote) == "" {
			citation = "empty"
		}
		if index >= 0 {
			citation = "matched"
			item[callKey] = observations[index].id
		}
	} else {
		for _, record := range records {
			if record.Event != "tool_call" || !spec.matchesTool(record.Tool) || record.ToolCallID == "" {
				continue
			}
			input, err := decodeResultContractJSON([]byte(record.Input))
			if err != nil {
				continue
			}
			target := evidenceString(input, spec.InputPointer)
			if spec.targetPattern != nil {
				for _, result := range records {
					if result.Event == "tool_result" && result.Tool == record.Tool && result.ToolCallID == record.ToolCallID {
						target = spec.observedTarget(input, result.Output)
					}
				}
			}
			if target != match {
				continue
			}
			if fetch == "absent" {
				fetch = "pending"
			}
			for _, result := range records {
				if result.Event == "tool_result" && result.Tool == record.Tool && result.ToolCallID == record.ToolCallID {
					fetch = "unusable"
					if result.Error {
						fetch = "failed"
					}
				}
			}
		}
	}
	fetchKey, _ := evidenceMember(spec.Diagnostics.FetchPointer)
	citationKey, _ := evidenceMember(spec.Diagnostics.CitationPointer)
	item[fetchKey], item[citationKey] = spec.Diagnostics.FetchValues[fetch], spec.Diagnostics.CitationValues[citation]
	// Keep the submitted quote for diagnosis, not as verified evidence.
	item[statusKey] = spec.UnverifiedStatus
	if index >= 0 {
		item[statusKey] = spec.VerifiedStatus
		return true
	}
	return false
}

func bindResultToolEvidence(value any, spec *resultToolEvidenceSpec, records []taskTranscriptRecord) (int, error) {
	if spec == nil {
		return 0, nil
	}
	rawGroups, err := resolveJSONPointer(value, spec.GroupsPointer)
	if err != nil {
		return 0, err
	}
	groups, ok := rawGroups.([]any)
	if !ok {
		return 0, fmt.Errorf("groups pointer must resolve to an array")
	}
	observations := matchingToolObservations(spec, records)
	statusKey, _ := evidenceMember(spec.StatusPointer)
	quoteKey, _ := evidenceMember(spec.QuotePointer)
	callKey, _ := evidenceMember(spec.CallIDPointer)
	downgrades := 0
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			return 0, fmt.Errorf("evidence group must be an object")
		}
		rawItems, err := resolveJSONPointer(group, spec.ItemsPointer)
		if err != nil {
			return 0, err
		}
		items, ok := rawItems.([]any)
		if !ok {
			return 0, fmt.Errorf("items pointer must resolve to an array")
		}
		missing := len(items) == 0
		// Count only sources bound by this attempt, never model-claimed
		// origins from evidence that was downgraded.
		verifiedItems := make([]map[string]any, 0, len(items))
		for _, rawItem := range items {
			item, ok := rawItem.(map[string]any)
			if !ok {
				return 0, fmt.Errorf("evidence item must be an object")
			}
			// Never trust a model-supplied call ID, even on an inaccessible item.
			item[callKey] = ""
			match, quote := evidenceString(item, spec.ValuePointer), evidenceString(item, spec.QuotePointer)
			index := slices.IndexFunc(observations, func(o successfulToolObservation) bool {
				return o.match == match && strings.TrimSpace(quote) != "" && strings.Contains(strings.Join(strings.Fields(o.output), " "), strings.Join(strings.Fields(quote), " "))
			})
			if spec.Diagnostics != nil {
				if bindResultEvidenceDiagnostics(item, spec, observations, records, match, quote, index) {
					verifiedItems = append(verifiedItems, item)
				} else {
					missing = true
				}
				continue
			}
			if item[statusKey] == spec.VerifiedStatus && index >= 0 {
				item[callKey] = observations[index].id
				verifiedItems = append(verifiedItems, item)
				continue
			}
			item[statusKey], item[quoteKey] = spec.UnverifiedStatus, ""
			missing = true
		}
		if missing {
			downgrades++
			for pointer, fallback := range spec.GroupFallback {
				key, _ := evidenceMember(pointer)
				group[key] = fallback
			}
			for pointer, suffix := range spec.GroupAppend {
				key, _ := evidenceMember(pointer)
				previous, _ := group[key].(string)
				if !strings.Contains(previous, suffix) {
					group[key] = strings.TrimSpace(previous + " " + suffix)
				}
			}
		}
		if policy := spec.Corroboration; policy != nil && evidenceString(group, policy.GroupPointer) == policy.GroupValue {
			origins := make(map[string]bool)
			targets := make(map[string]bool)
			for _, item := range verifiedItems {
				target := evidenceString(item, spec.ValuePointer)
				if origin := strings.TrimSpace(evidenceString(item, policy.OriginPointer)); origin != "" && !targets[target] {
					origins[origin] = true
					targets[target] = true
				}
			}
			if len(origins) < policy.Minimum {
				return 0, fmt.Errorf("corroboration requires %d distinct verified source origins; got %d", policy.Minimum, len(origins))
			}
		}
	}
	return downgrades, nil
}

type boundedReportBuffer struct{ bytes.Buffer }

func (b *boundedReportBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > resultPayloadMaxBytes {
		return 0, fmt.Errorf("final report exceeds byte limit")
	}
	return b.Buffer.Write(p)
}

// renderedContractFinalReport seals the declared terminal worker's payload
// without a second model synthesis. Templates are team-owned and schema-hashed.
func (c *Coordinator) renderedContractFinalReport() (string, bool, error) {
	if c == nil || c.session == nil || c.taskTracker == nil {
		return "", false, nil
	}
	var selected *TodoItem
	var report *template.Template
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.ResultContract == nil {
			continue
		}
		compiled, err := c.compiledResultContract(item.ResultContract)
		if err != nil {
			return "", false, err
		}
		if compiled.finalReport == nil {
			continue
		}
		if item.Status != TaskDone || item.TypedResult == nil || !taskResultStatusIsSuccessful(item.TypedResult.Status) || item.TypedResult.StructuredPayload == nil {
			return "", true, fmt.Errorf("final report task %s has no completed structured result", item.ID)
		}
		if selected != nil {
			return "", true, fmt.Errorf("multiple final report tasks are ambiguous")
		}
		selected, report = item, compiled.finalReport
	}
	if selected == nil {
		// A declared final worker must not be bypassed by finish before dispatch.
		for _, compiled := range c.session.ResultContracts {
			if compiled.finalReport != nil {
				return "", true, fmt.Errorf("required final report task has not completed")
			}
		}
		return "", false, nil
	}
	value, err := decodeResultContractJSON(selected.TypedResult.StructuredPayload.Value)
	if err != nil {
		return "", true, err
	}
	payload := selected.TypedResult.StructuredPayload
	if err := c.validateSubmittedEvidenceInputs(selected.ID, payload); err != nil {
		return "", true, err
	}
	sum := sha256.Sum256(payload.Value)
	if payload.Contract != *selected.ResultContract || payload.SHA256 != hex.EncodeToString(sum[:]) {
		return "", true, fmt.Errorf("final report payload identity does not match its contract or hash")
	}
	compiled, err := c.compiledResultContract(selected.ResultContract)
	if err != nil {
		return "", true, err
	}
	if err := compiled.schema.Validate(value); err != nil {
		return "", true, fmt.Errorf("final report payload no longer satisfies its schema: %w", err)
	}
	var out boundedReportBuffer
	if err := report.Execute(&out, value); err != nil {
		return "", true, err
	}
	if strings.TrimSpace(out.String()) == "" {
		return "", true, fmt.Errorf("final report template rendered empty output")
	}
	return out.String(), true, nil
}
