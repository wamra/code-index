// Package search implements routed identifier / full-text / vector search over
// the index.
package search

import (
	"regexp"
	"strings"
)

// identRe matches shapes that mean "a name, path or flag" in code: paths,
// --flags, dotted or :: qualified names, snake_case, camelCase,
// Verb-Noun (PowerShell), CONSTANTS.
var identRe = regexp.MustCompile(`/[\w.-]+/|\./|--[a-z]|\w\.\w|\w::\w|[a-z0-9]_[a-z0-9]|[a-z][A-Z]|[A-Za-z0-9]-[A-Za-z0-9]|\b[A-Z][A-Z0-9_]{3,}\b`)

// LooksLikeIdentifier reports whether q is a short, identifier-shaped query
// best served by trigram name search rather than embeddings: at most three
// words, and either one bare token or an identifier-like shape.
func LooksLikeIdentifier(q string) bool {
	f := strings.Fields(q)
	if len(f) == 0 || len(f) > 3 {
		return false
	}
	if len(f) == 1 {
		return true // a single token is a name until proven otherwise
	}
	return identRe.MatchString(q)
}
