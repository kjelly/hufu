package embedding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type wordPieceTokenizer struct {
	vocab                map[string]int
	unkID                int
	maxInputCharsPerWord int
	maxInputTokens       int
}

func newWordPieceTokenizer(tokens []string, config tokenizerManifest) (*wordPieceTokenizer, error) {
	vocab := make(map[string]int, len(tokens))
	for id, token := range tokens {
		vocab[token] = id
	}
	unkID, ok := vocab[config.UNKToken]
	if !ok {
		return nil, fmt.Errorf("unknown token %q is absent from vocabulary", config.UNKToken)
	}
	for _, id := range config.SkipTokenIDs {
		if id < 0 || id >= len(tokens) {
			return nil, fmt.Errorf("skip token ID %d is outside vocabulary", id)
		}
	}
	return &wordPieceTokenizer{
		vocab:                vocab,
		unkID:                unkID,
		maxInputCharsPerWord: config.MaxInputCharsPerWord,
		maxInputTokens:       config.MaxInputTokens,
	}, nil
}

func (t *wordPieceTokenizer) tokenize(ctx context.Context, text string) ([]int, error) {
	if !utf8.ValidString(text) {
		return nil, errors.New("embedding input is not valid UTF-8")
	}
	words, err := basicTokens(ctx, text)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, min(len(words), t.maxInputTokens))
	for i, word := range words {
		if i%64 == 0 {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
		}
		pieces := t.wordPiece(word)
		for _, id := range pieces {
			if len(ids) == t.maxInputTokens {
				return ids, nil
			}
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (t *wordPieceTokenizer) wordPiece(word string) []int {
	runes := []rune(word)
	if len(runes) > t.maxInputCharsPerWord {
		return []int{t.unkID}
	}
	var ids []int
	for start := 0; start < len(runes); {
		matchedID := -1
		matchedEnd := start
		for end := len(runes); end > start; end-- {
			piece := string(runes[start:end])
			if start > 0 {
				piece = "##" + piece
			}
			if id, ok := t.vocab[piece]; ok {
				matchedID = id
				matchedEnd = end
				break
			}
		}
		if matchedID < 0 {
			return []int{t.unkID}
		}
		ids = append(ids, matchedID)
		start = matchedEnd
	}
	return ids
}

func basicTokens(ctx context.Context, text string) ([]string, error) {
	var cleaned strings.Builder
	cleaned.Grow(len(text))
	for i, r := range text {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		switch {
		case r == 0 || r == utf8.RuneError:
			continue
		case isWhitespace(r):
			cleaned.WriteByte(' ')
		case unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r):
			continue
		case isChineseRune(r):
			cleaned.WriteByte(' ')
			cleaned.WriteRune(r)
			cleaned.WriteByte(' ')
		default:
			cleaned.WriteRune(r)
		}
	}

	var result []string
	for word := range strings.FieldsSeq(cleaned.String()) {
		var current strings.Builder
		flush := func() {
			if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
		}
		for _, r := range word {
			if isPunctuation(r) {
				flush()
				result = append(result, string(r))
			} else {
				current.WriteRune(r)
			}
		}
		flush()
	}
	return result, nil
}

func isWhitespace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || unicode.Is(unicode.Zs, r)
}

func isPunctuation(r rune) bool {
	return r >= 0x21 && r <= 0x2f ||
		r >= 0x3a && r <= 0x40 ||
		r >= 0x5b && r <= 0x60 ||
		r >= 0x7b && r <= 0x7e ||
		unicode.IsPunct(r)
}

func isChineseRune(r rune) bool {
	return r >= 0x3400 && r <= 0x4dbf ||
		r >= 0x4e00 && r <= 0x9fff ||
		r >= 0xf900 && r <= 0xfaff ||
		r >= 0x20000 && r <= 0x2a6df ||
		r >= 0x2a700 && r <= 0x2b73f ||
		r >= 0x2b740 && r <= 0x2b81f ||
		r >= 0x2b820 && r <= 0x2ceaf ||
		r >= 0x2f800 && r <= 0x2fa1f
}
