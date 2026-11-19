package team

import (
	"fmt"
	"strconv"
	"strings"
)

func invalidAcceptedWorksetPaths(value any, tokens []string, pointer string, choices map[string]string, invalid *[]string) error {
	if len(tokens) == 0 {
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("accepted path reference must be a string")
		}
		if _, _, err := splitWorksetPathLine(text); err != nil {
			return err
		}
		resolved, err := resolveWorksetPathLine(text, choices)
		if err != nil || resolved != text {
			*invalid = append(*invalid, pointer)
		}
		return nil
	}
	if tokens[0] == "*" {
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("accepted path selector requires an array")
		}
		for i, item := range items {
			if err := invalidAcceptedWorksetPaths(item, tokens[1:], pointer+"/"+strconv.Itoa(i), choices, invalid); err != nil {
				return err
			}
		}
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("accepted path selector requires an object")
	}
	child, exists := object[tokens[0]]
	if !exists {
		return nil // Schema-owned optional fields need not be present.
	}
	token := strings.ReplaceAll(strings.ReplaceAll(tokens[0], "~", "~0"), "/", "~1")
	return invalidAcceptedWorksetPaths(child, tokens[1:], pointer+"/"+token, choices, invalid)
}

func replaceAcceptedWorksetPath(payload any, selection AcceptedReferenceSelection, choices map[string]string) error {
	path, ok := choices[selection.Reference]
	if !ok {
		for _, canonical := range choices {
			if canonical == selection.Reference {
				path, ok = canonical, true
				break
			}
		}
	}
	if !ok {
		return fmt.Errorf("selected path is not in the receipt-verified frozen workset")
	}
	current, err := resolveJSONPointer(payload, selection.Pointer)
	if err != nil {
		return err
	}
	text, ok := current.(string)
	if !ok {
		return fmt.Errorf("accepted path reference must be a string")
	}
	_, line, err := splitWorksetPathLine(text)
	if err != nil {
		return err
	}
	return replaceAcceptedReference(payload, fixedCollectionPointerTokens(selection.Pointer), path+":"+line)
}
