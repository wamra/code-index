package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Extractor types, the value of Extractor.Type.
const (
	ExtractorExec = "exec" // a subprocess that prints JSON-lines records
	ExtractorLSP  = "lsp"  // a language server spoken to over stdio
)

// BuiltinPowerShellAST is the name of the built-in PowerShell AST extractor.
const BuiltinPowerShellAST = "powershell-ast"

// Extractor is one entry of the "extractors" section: a way to get symbols
// and/or call edges for the files of a language. The JSON shape and the
// meaning of every field are documented in docs/extractors.md. Loading,
// default-filling and validation are done by Config.Resolved and
// Config.Validate (they are not part of this type).
type Extractor struct {
	// Type is "exec" or "lsp".
	Type string `json:"type"`
	// Argv is the command line template; Argv[0] is the command. Placeholders:
	// {root}, {listFile} (exec only) and {embedded:NAME}.
	Argv []string `json:"argv"`
	// Env adds environment variables for the process.
	Env map[string]string `json:"env,omitempty"`
	// Extensions is the subset of the language's extensions this extractor is
	// given; empty means all of them.
	Extensions []string `json:"extensions,omitempty"`
	// Version is a static version string for the cache key.
	Version string `json:"version,omitempty"`
	// VersionArgv is a command whose trimmed first stdout line is the version
	// (10 s timeout). Version wins when both are set.
	VersionArgv []string `json:"version_argv,omitempty"`
	// CommandFallbacks are other commands tried for Argv[0], in order, when it
	// is not on PATH.
	CommandFallbacks []string `json:"command_fallbacks,omitempty"`
	// CommandEnv names an environment variable that overrides Argv[0]. When it
	// is set and the command is not found the extractor is unavailable (no
	// fallback).
	CommandEnv string `json:"command_env,omitempty"`

	// TimeoutS is the exec run timeout in seconds (default 600).
	TimeoutS int `json:"timeout_s,omitempty"`

	// LanguageID is the LSP languageId sent in didOpen (default: the language name).
	LanguageID string `json:"language_id,omitempty"`
	// StartupTimeoutS is the LSP initialize timeout in seconds (default 120).
	StartupTimeoutS int `json:"startup_timeout_s,omitempty"`
	// RequestTimeoutS is the per-request LSP timeout in seconds (default 30).
	RequestTimeoutS int `json:"request_timeout_s,omitempty"`
}

// BuiltinExtractors returns the extractor definitions that exist without a
// config file: a config may reference them by name and may override one by
// defining the same name. Each call returns a fresh copy.
func BuiltinExtractors() map[string]Extractor {
	return map[string]Extractor{
		BuiltinPowerShellAST: {
			Type: ExtractorExec,
			Argv: []string{"pwsh", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
				"-File", "{embedded:powershell-ast.ps1}", "-ListFile", "{listFile}"},
			CommandFallbacks: []string{"powershell"},
			CommandEnv:       "CODE_INDEX_POWERSHELL",
			Extensions:       []string{".ps1", ".psm1"},
		},
	}
}

// ExtractorNames is the value of a language's "extractor": one extractor name
// or an ordered list of names. In JSON it is a string or an array of strings;
// a single name is written back as a string. In Go it is the names joined by
// a comma (so a plain string constant is a valid value, as the legacy single
// "extractor" field always was); read it with Names. Extractor names must not
// contain a comma; UnmarshalJSON rejects one.
type ExtractorNames string

const extractorNameSep = ","

// NewExtractorNames joins names into an ExtractorNames.
func NewExtractorNames(names ...string) ExtractorNames {
	return ExtractorNames(strings.Join(names, extractorNameSep))
}

// Names returns the names in order (nil for the empty value).
func (n ExtractorNames) Names() []string {
	if n == "" {
		return nil
	}
	return strings.Split(string(n), extractorNameSep)
}

// MarshalJSON writes one name as a string and several as an array.
func (n ExtractorNames) MarshalJSON() ([]byte, error) {
	names := n.Names()
	switch len(names) {
	case 0:
		return []byte("null"), nil
	case 1:
		return json.Marshal(names[0])
	}
	return json.Marshal(names)
}

// UnmarshalJSON accepts a string or an array of strings. An empty array, an
// empty name inside an array and a name that contains a comma are errors; null
// and the empty string (accepted by the legacy single field) leave the value empty.
func (n *ExtractorNames) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		*n = ""
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if one == "" { // the legacy single field accepted "": no extractor
			*n = ""
			return nil
		}
		return n.set([]string{one})
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("extractor must be a name or an array of names")
	}
	if len(many) == 0 {
		return errors.New("extractor: the array must name at least one extractor")
	}
	return n.set(many)
}

func (n *ExtractorNames) set(names []string) error {
	for _, s := range names {
		if s == "" {
			return errors.New("extractor: names must not be empty")
		}
		if strings.Contains(s, extractorNameSep) {
			return fmt.Errorf("extractor: name %q must not contain a comma", s)
		}
	}
	*n = NewExtractorNames(names...)
	return nil
}
