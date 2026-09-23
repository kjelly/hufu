package memoryconflict

import (
	"strings"
	"unicode"
)

// stopwords are frequent English words that carry no topic.
var stopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "this": {}, "that": {}, "from": {}, "into": {},
	"when": {}, "then": {}, "than": {}, "use": {}, "used": {}, "using": {}, "must": {}, "should": {},
	"not": {}, "are": {}, "was": {}, "were": {}, "has": {}, "have": {}, "all": {}, "any": {}, "can": {}, "will": {},
}

type runeClass int

const (
	classSeparator runeClass = iota
	classASCII
	classCJK
	classOther
)

func classify(r rune) runeClass {
	switch {
	case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'):
		return classASCII
	case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
		return classCJK
	case r >= 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
		return classOther
	default:
		return classSeparator
	}
}

// Tokens returns the topic tokens of content: lowercase ASCII words of at
// least three characters that are not stopwords, rune bigrams for CJK runs
// (a single rune for a one-rune run), and other non-ASCII words of at least
// three runes.
func Tokens(content string) map[string]struct{} {
	tokens := map[string]struct{}{}
	var run []rune
	current := classSeparator
	flush := func() {
		switch current {
		case classASCII:
			if word := string(run); len(run) >= 3 {
				if _, stop := stopwords[word]; !stop {
					tokens[word] = struct{}{}
				}
			}
		case classCJK:
			if len(run) == 1 {
				tokens[string(run)] = struct{}{}
			}
			for i := 0; i+1 < len(run); i++ {
				tokens[string(run[i:i+2])] = struct{}{}
			}
		case classOther:
			if len(run) >= 3 {
				tokens[string(run)] = struct{}{}
			}
		}
		run = run[:0]
	}
	for _, r := range strings.ToLower(content) {
		class := classify(r)
		if class != current {
			flush()
			current = class
		}
		if class != classSeparator {
			run = append(run, r)
		}
	}
	flush()
	return tokens
}
