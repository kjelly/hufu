package context

import (
	"strings"
	"unicode"
)

// ftsQuery turns operational identifiers (paths, commands, punctuation) into
// plain FTS tokens so an exact-matchable path cannot make the lexical stage
// fail with an FTS syntax error. The uppercase FTS5 operators (AND, OR, NOT,
// NEAR) are dropped rather than searched for: in task text they carry no
// retrieval signal. Every remaining term is quoted, repeated terms are kept
// once, and the terms are joined with OR so a record matches when it shares
// any term and bm25 ranks it by how many rare terms it shares. Joined
// implicitly, every term was required: a task goal of a hundred words or more
// matched no record at all. Keep Unicode letters, numbers and combining marks
// so query terms can match the existing unicode61 index without an encoder.
// This preserves complete tokens; it does not segment words or interpret intent.
func ftsQuery(query string) string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) && r != '_'
	})
	terms := make([]string, 0, len(fields))
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		switch field {
		case "AND", "OR", "NOT", "NEAR":
			continue
		}
		// The unicode61 tokenizer folds ASCII case, so case variants are the
		// same term.
		if key := strings.ToLower(field); !seen[key] {
			seen[key] = true
			terms = append(terms, `"`+field+`"`)
		}
	}
	return strings.Join(terms, " OR ")
}
