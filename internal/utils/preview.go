package utils

// TruncatePreview truncates text to a maximum length, appending "..." for preview purposes.
// It handles multi-byte characters correctly by working with runes.
//
// Behavior:
//   - If text is empty, returns empty string
//   - If text length <= maxLen, returns text unchanged
//   - If text length > maxLen, truncates to maxLen-3 characters and appends "..."
//
// Example:
//
//	TruncatePreview("Hello, World!", 10) // returns "Hello, Wor..."
//	TruncatePreview("Hi", 10)           // returns "Hi"
//	TruncatePreview("", 10)             // returns ""
func TruncatePreview(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if maxLen <= 3 {
		// Not enough space for text + "...", just return ellipsis or truncated text
		runes := []rune(text)
		if len(runes) == 0 {
			return ""
		}
		if len(runes) <= maxLen {
			return string(runes[:len(runes)])
		}
		return string(runes[:maxLen])
	}

	runes := []rune(text)
	if len(runes) <= maxLen-3 {
		return text
	}
	return string(runes[:maxLen-3]) + "..."
}
