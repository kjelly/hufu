package team

import "bytes"

// collectionResponseJSON removes only one complete transport code block around
// one bounded JSON object. The response artifact retains its original bytes;
// the returned claims still undergo all schema, source and receipt checks.
// Prose, multiple frames, duplicate keys and non-object values stay rejected by
// the strict claims decoder rather than being guessed or repaired here.
func collectionResponseJSON(raw []byte) ([]byte, bool) {
	if len(raw) > resultCollectionResponseMaxBytes {
		return raw, false
	}
	trimmed := bytes.TrimSpace(raw)
	var body []byte
	for _, prefix := range []string{"```json\n", "```json\r\n", "```\n", "```\r\n"} {
		candidate, opened := bytes.CutPrefix(trimmed, []byte(prefix))
		if opened {
			if content, closed := bytes.CutSuffix(candidate, []byte("\n```")); closed {
				body = bytes.TrimSpace(content)
				break
			}
		}
	}
	value, err := decodeUniqueJSON(body)
	if err != nil {
		return raw, false
	}
	if _, object := value.(map[string]any); !object {
		return raw, false
	}
	return body, true
}
