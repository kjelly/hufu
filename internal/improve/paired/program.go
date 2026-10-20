package paired

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/scripted"
)

type Program struct {
	SchemaVersion int      `json:"schema_version"`
	Scripts       []Script `json:"scripts"`
}

type Script struct {
	Stage         string          `json:"stage"`
	CaseID        string          `json:"case_id,omitempty"`
	VariantSlotID string          `json:"variant_slot_id,omitempty"`
	Attempt       int64           `json:"attempt"`
	Steps         []scripted.Step `json:"steps"`
}

// ParseProgram checks syntax and explicit usage before any transport exists.
func ParseProgram(raw []byte) (Program, error) {
	if len(raw) > MaxDataBytes {
		return Program{}, errors.New("fixture_program: size limit exceeded")
	}
	var program Program
	if err := json.Unmarshal(raw, &program, json.RejectUnknownMembers(true)); err != nil {
		return Program{}, fmt.Errorf("fixture_program: %w", err)
	}
	if program.SchemaVersion != SchemaVersion || len(program.Scripts) == 0 {
		return Program{}, errors.New("fixture_program: requires schema_version 1 and scripts")
	}
	// Presence is distinct from a zero value or unknown usage.
	var presence struct {
		Scripts []map[string]jsontext.Value `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &presence); err != nil {
		return Program{}, err
	}
	selectors := map[string]bool{}
	for i, script := range program.Scripts {
		if err := script.validate(); err != nil {
			return Program{}, fmt.Errorf("scripts[%d]: %w", i, err)
		}
		selector := fmt.Sprintf("%s/%s/%s/%d", script.Stage, script.CaseID, script.VariantSlotID, script.Attempt)
		if selectors[selector] {
			return Program{}, fmt.Errorf("scripts[%d]: duplicate selector", i)
		}
		selectors[selector] = true
		if script.Stage != "executor" {
			for _, key := range []string{"case_id", "variant_slot_id"} {
				if _, present := presence.Scripts[i][key]; present {
					return Program{}, fmt.Errorf("scripts[%d]: %s must be omitted", i, key)
				}
			}
		}
		var steps []map[string]jsontext.Value
		if err := json.Unmarshal(presence.Scripts[i]["steps"], &steps); err != nil {
			return Program{}, err
		}
		for j, step := range script.Steps {
			if err := validateStep(step, steps[j]); err != nil {
				return Program{}, fmt.Errorf("scripts[%d].steps[%d]: %w", i, j, err)
			}
		}
	}
	return program, nil
}

func (s Script) validate() error {
	if !slices.Contains([]string{"executor", "procedure", "domain", "compose", "reflect"}, s.Stage) {
		return errors.New("stage: unsupported value")
	}
	if s.Attempt < 1 || s.Attempt > 2 || len(s.Steps) == 0 {
		return errors.New("requires attempt 1 or 2 and nonempty steps")
	}
	if s.Stage == "executor" {
		if !ValidID(s.CaseID) || !ValidID(s.VariantSlotID) {
			return errors.New("executor requires valid case_id and variant_slot_id")
		}
	} else if s.CaseID != "" || s.VariantSlotID != "" {
		return errors.New("non-executor stage forbids case_id and variant_slot_id")
	}
	return nil
}

func validateStep(step scripted.Step, fields map[string]jsontext.Value) error {
	for name, value := range fields {
		if name != "usage" && value.Kind() == 'n' {
			return fmt.Errorf("%s: null is not permitted", name)
		}
	}
	usage, exists := fields["usage"]
	if !exists {
		return errors.New("usage: explicit value or null required")
	}
	if step.Usage != nil {
		var values map[string]jsontext.Value
		if err := json.Unmarshal(usage, &values); err != nil {
			return err
		}
		for _, key := range []string{"input_tokens", "output_tokens"} {
			value, exists := values[key]
			if !exists || string(value) == "null" {
				return fmt.Errorf("usage.%s: integer required", key)
			}
		}
		if step.Usage.InputTokens < 0 || step.Usage.OutputTokens < 0 ||
			step.Usage.InputTokens > math.MaxInt64-step.Usage.OutputTokens {
			return errors.New("usage: negative value or overflow")
		}
	}
	if step.Match != nil && strings.TrimSpace(step.Match.Contains) == "" {
		return errors.New("match.contains: nonempty string required")
	}
	if step.ToolCall != nil && (strings.TrimSpace(step.ToolCall.Name) == "" || strings.TrimSpace(step.ToolCall.Arguments) == "") {
		return errors.New("tool_call: name and arguments required")
	}
	if step.ToolCall == nil && step.Content == "" {
		return errors.New("content or tool_call required")
	}
	return nil
}
