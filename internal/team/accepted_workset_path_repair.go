package team

import "fmt"

func invalidAcceptedWorksetPaths(value any, tokens []string, selection worksetPathSelection, invalid *[]string) error {
	_, err := transformWorksetPathLeaves(value, tokens, "", func(text, pointer string) (string, error) {
		if _, _, err := splitWorksetPathLine(text); err != nil {
			return "", err
		}
		resolved, err := selection.resolve(text)
		if err != nil || resolved != text {
			*invalid = append(*invalid, pointer)
		}
		return text, nil
	})
	return err
}

func replaceAcceptedWorksetPath(payload any, selection AcceptedReferenceSelection, paths worksetPathSelection) error {
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
	reference, err := paths.resolve(selection.Reference + ":" + line)
	if err != nil {
		return fmt.Errorf("%s: %w", selection.Pointer, err)
	}
	return replaceAcceptedReference(payload, fixedCollectionPointerTokens(selection.Pointer), reference)
}
