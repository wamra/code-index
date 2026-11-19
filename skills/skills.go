// Package skills embeds the Claude Code skill files installed by
// `code-index install-skill`.
package skills

import _ "embed"

// CodeSearch is skills/code-search/SKILL.md.
//
//go:embed code-search/SKILL.md
var CodeSearch []byte
