// Package config loads the code-index configuration: an optional JSON file
// overridden by environment variables.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wamra/code-index/internal/provider"
)

// DefaultMaxFileSize is index.max_file_size when unset (bytes).
const DefaultMaxFileSize = 1 << 20

// DefaultIgnore are the index.ignore globs used when the file sets none.
var DefaultIgnore = []string{".git", "node_modules", "vendor", "dist", "build", "__pycache__", ".venv", "target"}

// Embed is the "embed" section: the provider settings plus an on/off switch.
// Enabled left unset means "on when a provider or model is given".
type Embed struct {
	Enabled *bool `json:"enabled,omitempty"`
	provider.EmbedConfig
}

// Language is one language server entry; same shape as a Claude Code plugin's
// .lsp.json entry plus the fields below.
type Language struct {
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Env adds environment variables for the server process (like .lsp.json "env").
	Env             map[string]string `json:"env,omitempty"`
	Extensions      []string          `json:"extensions"`
	LanguageID      string            `json:"language_id,omitempty"`
	StartupTimeoutS int               `json:"startup_timeout_s,omitempty"`
	RequestTimeoutS int               `json:"request_timeout_s,omitempty"`
	// Extractor names the extractor(s) of the language: one name or an array
	// of names, defined in Config.Extractors or built in ("powershell-ast": call
	// graph, aliases and dynamic calls from the PowerShell parser). With the
	// legacy Command field set, the language server is an implicit lsp
	// extractor that runs first (docs/extractors.md).
	Extractor ExtractorNames `json:"extractor,omitempty"`
}

// Index is the "index" section.
type Index struct {
	Ignore      []string `json:"ignore,omitempty"`
	MaxFileSize int64    `json:"max_file_size,omitempty"`
}

// Config is the contents of config.json.
type Config struct {
	Comment    string                   `json:"_comment,omitempty"`
	Embed      Embed                    `json:"embed"`
	Summarize  provider.SummarizeConfig `json:"summarize"`
	Languages  map[string]Language      `json:"languages,omitempty"`
	Extractors map[string]Extractor     `json:"extractors,omitempty"`
	Index      Index                    `json:"index"`
}

// Path returns the config file path: $CODE_INDEX_CONFIG, else
// <home>/.config/code-index/config.json.
func Path(home string, getenv func(string) string) string {
	if p := getenv("CODE_INDEX_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(home, ".config", "code-index", "config.json")
}

// readFile decodes one config file. sections names the top-level sections the
// file contains. A missing file is not an error (found is false).
func readFile(path string) (c Config, sections map[string]bool, found bool, err error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		return Config{}, nil, false, nil
	default:
		return Config{}, nil, false, fmt.Errorf("config %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, nil, true, fmt.Errorf("config %s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	sections = map[string]bool{}
	for k := range raw {
		sections[k] = true
	}
	return c, sections, true, nil
}

// Load reads the file at path (a missing file means defaults) and applies the
// environment overrides. The result is NOT resolved: call Resolved for the
// effective values. found reports whether the file existed.
//
// Environment, applied over the file: CODE_INDEX_EMBED_{PROVIDER,MODEL,BASE_URL}
// and CODE_INDEX_SUMMARIZE_{PROVIDER,MODEL,BASE_URL}.
func Load(path string, getenv func(string) string) (c Config, found bool, err error) {
	c, _, found, err = readFile(path)
	if err != nil {
		return Config{}, found, err
	}
	finish(&c, getenv)
	return c, found, nil
}

// Sources says which file each section of a layered config came from:
// keys "embed", "summarize", "index" and "languages.<name>"; the value is a
// file path, or "default" when no file set it.
type Sources map[string]string

// RepoPath is the per-repo override file: <root>/.Wammy/config.json.
func RepoPath(root string) string { return filepath.Join(root, ".Wammy", "config.json") }

// LoadLayered reads the global file and the optional per-repo override and
// merges them section by section: "languages" per language key, "embed",
// "summarize" and "index" replaced as whole sections when the repo file has
// them. Environment overrides apply last.
func LoadLayered(globalPath, repoPath string, getenv func(string) string) (Config, Sources, error) {
	src := Sources{"embed": "default", "summarize": "default", "index": "default"}
	g, gsec, gfound, err := readFile(globalPath)
	if err != nil {
		return Config{}, nil, err
	}
	c := Config{Languages: map[string]Language{}}
	if gfound {
		for _, sec := range []string{"embed", "summarize", "index"} {
			if gsec[sec] {
				src[sec] = globalPath
			}
		}
		c.Embed, c.Summarize, c.Index = g.Embed, g.Summarize, g.Index
		for n, l := range g.Languages {
			c.Languages[n] = l
			src["languages."+n] = globalPath
		}
	}
	if repoPath != "" {
		r, rsec, rfound, err := readFile(repoPath)
		if err != nil {
			return Config{}, nil, err
		}
		if rfound {
			if rsec["embed"] {
				c.Embed, src["embed"] = r.Embed, repoPath
			}
			if rsec["summarize"] {
				c.Summarize, src["summarize"] = r.Summarize, repoPath
			}
			if rsec["index"] {
				c.Index, src["index"] = r.Index, repoPath
			}
			for n, l := range r.Languages {
				c.Languages[n] = l
				src["languages."+n] = repoPath
			}
		}
	}
	finish(&c, getenv)
	return c, src, nil
}

func finish(c *Config, getenv func(string) string) {
	applyEnv(c, getenv)
	if c.Embed.Enabled == nil {
		on := c.Embed.Provider != "" || c.Embed.Model != ""
		c.Embed.Enabled = &on
	}
}

func applyEnv(c *Config, getenv func(string) string) {
	set := func(dst *string, key string) {
		if v := getenv(key); v != "" {
			*dst = v
		}
	}
	set(&c.Embed.Provider, "CODE_INDEX_EMBED_PROVIDER")
	set(&c.Embed.Model, "CODE_INDEX_EMBED_MODEL")
	set(&c.Embed.BaseURL, "CODE_INDEX_EMBED_BASE_URL")
	set(&c.Summarize.Provider, "CODE_INDEX_SUMMARIZE_PROVIDER")
	set(&c.Summarize.Model, "CODE_INDEX_SUMMARIZE_MODEL")
	set(&c.Summarize.BaseURL, "CODE_INDEX_SUMMARIZE_BASE_URL")
}

// EmbedEnabled reports whether an embedder is configured and switched on.
func (c Config) EmbedEnabled() bool { return c.Embed.Enabled != nil && *c.Embed.Enabled }

// Resolved returns c with every default filled in.
func (c Config) Resolved() Config {
	c.Embed.EmbedConfig = c.Embed.EmbedConfig.Resolved()
	c.Summarize = c.Summarize.Resolved()
	if len(c.Index.Ignore) == 0 {
		c.Index.Ignore = append([]string(nil), DefaultIgnore...)
	}
	if c.Index.MaxFileSize <= 0 {
		c.Index.MaxFileSize = DefaultMaxFileSize
	}
	langs := make(map[string]Language, len(c.Languages))
	for name, l := range c.Languages {
		if l.LanguageID == "" {
			l.LanguageID = name
		}
		if l.StartupTimeoutS <= 0 {
			l.StartupTimeoutS = 120
		}
		if l.RequestTimeoutS <= 0 {
			l.RequestTimeoutS = 30
		}
		langs[name] = l
	}
	c.Languages = langs
	return c
}

// Validate checks a resolved config.
func (c Config) Validate() error {
	if c.EmbedEnabled() {
		if err := c.Embed.EmbedConfig.Validate(); err != nil {
			return err
		}
	}
	if err := c.Summarize.Validate(); err != nil {
		return err
	}
	seen := map[string]string{}
	for _, name := range c.LanguageNames() {
		l := c.Languages[name]
		if l.Command == "" {
			return fmt.Errorf("languages.%s.command is required", name)
		}
		if l.Extractor != "" && l.Extractor != "powershell-ast" {
			return fmt.Errorf("languages.%s.extractor: unknown extractor %q (known: powershell-ast)", name, l.Extractor)
		}
		if len(l.Extensions) == 0 {
			return fmt.Errorf("languages.%s.extensions is required", name)
		}
		for _, e := range l.Extensions {
			if !strings.HasPrefix(e, ".") {
				return fmt.Errorf("languages.%s.extensions: %q must start with a dot", name, e)
			}
			key := strings.ToLower(e)
			if other, ok := seen[key]; ok {
				return fmt.Errorf("extension %s is claimed by both languages.%s and languages.%s", e, other, name)
			}
			seen[key] = name
		}
	}
	return nil
}

// LanguageNames returns the configured language names, sorted.
func (c Config) LanguageNames() []string {
	names := make([]string, 0, len(c.Languages))
	for n := range c.Languages {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LanguageFor returns the language whose extensions include ext (with dot,
// case-insensitive).
func (c Config) LanguageFor(ext string) (string, bool) {
	ext = strings.ToLower(ext)
	for _, n := range c.LanguageNames() {
		for _, e := range c.Languages[n].Extensions {
			if strings.ToLower(e) == ext {
				return n, true
			}
		}
	}
	return "", false
}

type embedOut struct {
	Enabled     bool    `json:"enabled"`
	Provider    string  `json:"provider"`
	BaseURL     string  `json:"base_url"`
	Model       string  `json:"model"`
	Dims        int     `json:"dims"`
	DocPrefix   *string `json:"doc_prefix,omitempty"`
	QueryPrefix *string `json:"query_prefix,omitempty"`
	APIKeyEnv   string  `json:"api_key_env"`
	Region      string  `json:"region,omitempty"`
	Profile     string  `json:"profile,omitempty"`
	// MaxItemsPerDay is shown as configured (0 = unset: hosted default or unlimited).
	MaxItemsPerDay int `json:"max_items_per_day,omitempty"`
}

type summarizeOut struct {
	Provider       string  `json:"provider"`
	BaseURL        string  `json:"base_url"`
	Model          string  `json:"model"`
	APIKeyEnv      string  `json:"api_key_env"`
	MaxTokens      int     `json:"max_tokens"`
	Temperature    float64 `json:"temperature"`
	Enabled        bool    `json:"enabled"`
	MaxItemsPerRun int     `json:"max_items_per_run"`
	Region         string  `json:"region,omitempty"`
	Profile        string  `json:"profile,omitempty"`
	MaxItemsPerDay int     `json:"max_items_per_day,omitempty"`
	MaxInputChars  int     `json:"max_input_chars"`
}

type out struct {
	Comment   string              `json:"_comment,omitempty"`
	Embed     embedOut            `json:"embed"`
	Summarize summarizeOut        `json:"summarize"`
	Languages map[string]Language `json:"languages"`
	Index     Index               `json:"index"`
}

func (c Config) out(comment string) out {
	e, s := c.Embed, c.Summarize
	o := out{Comment: comment, Languages: c.Languages, Index: c.Index}
	if o.Languages == nil {
		o.Languages = map[string]Language{}
	}
	o.Embed = embedOut{c.EmbedEnabled(), e.Provider, e.BaseURL, e.Model, e.Dims, e.DocPrefix, e.QueryPrefix, e.APIKeyEnv, e.Region, e.Profile, e.MaxItemsPerDay}
	o.Summarize = summarizeOut{s.Provider, s.BaseURL, s.Model, s.APIKeyEnv, s.MaxTokens, *s.Temperature, *s.Enabled, s.MaxItemsPerRun,
		s.Region, s.Profile, s.MaxItemsPerDay, s.MaxInputChars}
	return o
}

// Effective renders the resolved config as JSON. API keys are never read:
// only the NAME of the key variable is shown, with whether it is set. src
// says which file each section came from.
func Effective(c Config, src Sources, getenv func(string) string) []byte {
	r := c.Resolved()
	state := func(name string) string {
		switch {
		case name == "":
			return "none"
		case getenv(name) != "":
			return "set"
		}
		return "unset"
	}
	b, _ := json.MarshalIndent(struct {
		Sources Sources `json:"_sources"`
		out
		EmbedKey     string `json:"_embed_api_key"`
		SummarizeKey string `json:"_summarize_api_key"`
		EmbedCap     string `json:"_embed_daily_cap"`
	}{src, r.out(""), state(r.Embed.APIKeyEnv), state(r.Summarize.APIKeyEnv), capText(r.Embed.DailyCap)}, "", "  ")
	return append(b, '\n')
}

func capText(f func() (int, string)) string {
	n, src := f()
	if n == 0 {
		return src
	}
	return fmt.Sprintf("%d (%s)", n, src)
}

// InitComment is the "_comment" of the file written by `config init`.
const InitComment = "code-index configuration. embed: provider \"ollama\", \"openai\" (any OpenAI-compatible server) or \"bedrock\"; set enabled to true to build and use embeddings (without them everything works through full-text search). api_key_env is the NAME of an environment variable holding the API key, never the key itself. summarize is parsed but not used yet. languages: one entry per language server (same shape as a Claude Code plugin's .lsp.json entry); extensions must include the dot. index.ignore: path or glob patterns skipped in addition to .gitignore; index.max_file_size in bytes. Environment overrides: CODE_INDEX_CONFIG, CODE_INDEX_DB, CODE_INDEX_{EMBED,SUMMARIZE}_{PROVIDER,MODEL,BASE_URL}."

// KnownServers are the language server commands `config init` looks for on PATH.
var KnownServers = []struct {
	Name string
	Lang Language
}{
	{"bash", Language{Command: "bash-language-server", Args: []string{"start"}, Extensions: []string{".sh", ".bash", ".zsh", ".ksh"}, LanguageID: "shellscript"}},
	{"go", Language{Command: "gopls", Extensions: []string{".go"}, LanguageID: "go"}},
	{"python", Language{Command: "pyright-langserver", Args: []string{"--stdio"}, Extensions: []string{".py"}, LanguageID: "python"}},
	{"typescript", Language{Command: "typescript-language-server", Args: []string{"--stdio"}, Extensions: []string{".ts", ".tsx", ".js", ".jsx"}, LanguageID: "typescript"}},
	{"rust", Language{Command: "rust-analyzer", Extensions: []string{".rs"}, LanguageID: "rust"}},
	{"c", Language{Command: "clangd", Extensions: []string{".c", ".h", ".cc", ".cpp", ".hpp"}, LanguageID: "cpp"}},
}

// Discover returns language entries for the servers found on the machine:
// the known commands on PATH, and PowerShell Editor Services when pwsh is on
// PATH and a start-pses.ps1 script is installed by a Claude Code plugin under
// <home>/.claude/plugins/cache.
func Discover(home string, lookPath func(string) (string, error)) map[string]Language {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	langs := map[string]Language{}
	for _, k := range KnownServers {
		if _, err := lookPath(k.Lang.Command); err == nil {
			langs[k.Name] = k.Lang
		}
	}
	if _, err := lookPath("pwsh"); err == nil {
		scripts, _ := filepath.Glob(filepath.Join(home, ".claude", "plugins", "cache", "*", "*", "*", "scripts", "start-pses.ps1"))
		sort.Strings(scripts)
		if len(scripts) > 0 {
			langs["powershell"] = Language{
				Command:         "pwsh",
				Args:            []string{"-NoLogo", "-NoProfile", "-File", scripts[len(scripts)-1]},
				Extensions:      []string{".ps1", ".psm1", ".psd1"},
				LanguageID:      "powershell",
				StartupTimeoutS: 120,
				Extractor:       "powershell-ast",
			}
		}
	}
	return langs
}

// Init renders the default config file with the discovered languages.
func Init(langs map[string]Language) []byte {
	off := false
	c := Config{
		Embed:     Embed{Enabled: &off, EmbedConfig: provider.EmbedConfig{Provider: provider.Ollama, Model: provider.DefaultEmbedModel}},
		Languages: langs,
	}
	r := c.Resolved()
	o := r.out(InitComment)
	if _, known := provider.KnownEmbedModel(r.Embed.Model); known {
		o.Embed.DocPrefix, o.Embed.QueryPrefix = nil, nil
	}
	b, _ := json.MarshalIndent(o, "", "  ")
	return append(b, '\n')
}
