package team

const minTruncatedContextTokens = 12

const truncatedContextMarker = "..."

// Required normative instructions must fit in full or fail compilation.
func truncatable(item ContextItem) bool {
	return item.Compressible && (!item.Required || normalizedContextAuthority(item) != ContextAuthorityNormative)
}

// truncateContextItemToTokens measures the retained prefix and its marker with
// the item's estimator. Average tokens per rune are unsafe for mixed scripts:
// a CJK prefix can cost much more than the ASCII remainder it replaces.
func truncateContextItemToTokens(item ContextItem, remaining int) (ContextItem, bool) {
	if remaining < minTruncatedContextTokens {
		return item, false
	}
	runes := []rune(item.Content)
	lo, hi := 0, len(runes)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if estimateTextTokens(string(runes[:mid])+truncatedContextMarker, item.tokenEstimator) <= remaining {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo <= 0 {
		return item, false
	}
	item.Content = string(runes[:lo]) + truncatedContextMarker
	item.TokenCount = estimateTextTokens(item.Content, item.tokenEstimator)
	item.Compressed = true
	return item, true
}
