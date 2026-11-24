package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wamra/code-index/internal/provider"
)

// RepoPath is the per-repo override file: <root>/.Wammy/config.json.
func RepoPath(root string) string { return filepath.Join(root, ".Wammy", "config.json") }

// layer is one config source, in merge order. trusted says it may define
// commands and choose provider endpoints.
type layer struct {
	source   string
	cfg      Config
	sections map[string]bool
	trusted  bool
}

// LoadLayered merges, in this order: the built-in defaults, the top level of the
// central file (path central), its repos entry for the repo at root, the repo's
// own <root>/.Wammy/config.json, and the environment. All of them are Config
// files of the same schema; any may be missing. The merge is section by section:
// "languages" per language key, "extractors" per extractor name (an entry
// replaces a built-in or earlier entry of the same name as a whole), "embed",
// "summarize" and "index" replaced as whole sections when the layer has them.
//
// The repo's own file is untrusted unless the matching repos entry has
// trust_repo_config true. An untrusted file keeps languages.<n>.extensions,
// extractor (names) and the other non-command language fields, and index; its
// extractors, language command/args/env, embed and summarize are dropped, each
// with a note. For a language that exists in an earlier layer its fields overlay
// that entry field by field. repos and trust_repo_config in the repo's file are
// always ignored (with a note). Repos entries match by canonical path.
//
// The notes are for the user (print them to stderr).
func LoadLayered(central, root string, getenv func(string) string) (Config, Sources, []string, error) {
	src := Sources{"embed": "default", "summarize": "default", "index": "default"}
	g, gsec, gentries, gfound, err := readFile(central)
	if err != nil {
		return Config{}, nil, nil, err
	}
	merged := Config{Languages: map[string]Language{}, Extractors: map[string]Extractor{}}
	var layers []layer
	var notes []string
	trusted := false
	if gfound {
		if gsec["trust_repo_config"] {
			return Config{}, nil, nil, fmt.Errorf("config %s: trust_repo_config is only allowed inside a repos entry (repos.\"<root>\")", central)
		}
		for _, key := range sortedRepoKeys(g.Repos) {
			if gentries[key]["repos"] {
				return Config{}, nil, nil, fmt.Errorf("config %s: repos.%q.repos is not allowed (a repos entry cannot hold repos)", central, key)
			}
		}
		canon, err := canonKeys(g.Repos)
		if err != nil {
			return Config{}, nil, nil, fmt.Errorf("config %s: %w", central, err)
		}
		merged.Repos = g.Repos
		layers = append(layers, layer{central, g, gsec, true})
		if key, ok := canon[canonPath(root)]; ok {
			e := g.Repos[key]
			esrc := central + "#repos." + key
			src["repos"] = esrc
			trusted = e.TrustRepoConfig
			layers = append(layers, layer{esrc, e, gentries[key], true})
		}
	}
	rp := RepoPath(root)
	r, rsec, _, rfound, err := readFile(rp)
	if err != nil {
		return Config{}, nil, nil, err
	}
	if rfound {
		fix := fmt.Sprintf("repos.\"%s\"", root)
		if rsec["repos"] {
			notes = append(notes, fmt.Sprintf("%s: ignored repos (a repo file cannot configure repos); set it in %s under %s", rp, central, fix))
		}
		if rsec["trust_repo_config"] {
			notes = append(notes, fmt.Sprintf("%s: ignored trust_repo_config (a repo cannot trust itself); set %s.trust_repo_config to true in %s", rp, fix, central))
		}
		l := layer{rp, r, rsec, trusted}
		if !trusted {
			var gn []string
			l, gn, merged.gate = gateLayer(l, fmt.Sprintf("define it in %s under %s, or set %s.trust_repo_config to true", central, fix, fix))
			notes = append(notes, gn...)
		}
		layers = append(layers, l)
	}
	for _, l := range layers {
		l.mergeInto(&merged, src)
	}
	merged.TrustRepoConfig = trusted
	finish(&merged, getenv)
	return merged, src, notes, nil
}

// mergeInto applies l over c, recording sources.
func (l layer) mergeInto(c *Config, src Sources) {
	if l.sections["embed"] {
		c.Embed, src["embed"] = l.cfg.Embed, l.source
	}
	if l.sections["summarize"] {
		c.Summarize, src["summarize"] = l.cfg.Summarize, l.source
	}
	if l.sections["index"] {
		c.Index, src["index"] = l.cfg.Index, l.source
	}
	for n, lang := range l.cfg.Languages {
		if !l.trusted {
			lang = overlayLanguage(c.Languages[n], lang)
		}
		c.Languages[n] = lang
		src["languages."+n] = l.source
	}
	for n, e := range l.cfg.Extractors {
		c.Extractors[n] = e
		src["extractors."+n] = l.source
	}
}

// overlayLanguage sets the fields of with that over has, leaving the others
// (the command, args and env of an earlier layer) as they were.
func overlayLanguage(base, with Language) Language {
	if with.Extensions != nil {
		base.Extensions = with.Extensions
	}
	if with.Extractor != "" {
		base.Extractor = with.Extractor
	}
	if with.LanguageID != "" {
		base.LanguageID = with.LanguageID
	}
	if with.StartupTimeoutS != 0 {
		base.StartupTimeoutS = with.StartupTimeoutS
	}
	if with.RequestTimeoutS != 0 {
		base.RequestTimeoutS = with.RequestTimeoutS
	}
	return base
}

// gateLayer is the allowlist applied to an untrusted repo file: it keeps index
// and, per language, extensions, extractor, language_id and the timeouts. It
// returns the filtered layer, one note per dropped key, and the hints Validate
// appends when a kept name points at something that was dropped.
func gateLayer(l layer, fix string) (layer, []string, map[string]string) {
	var notes []string
	gate := map[string]string{}
	drop := func(key, why string) string {
		n := fmt.Sprintf("%s: ignored %s (%s); %s", l.source, key, why, fix)
		notes = append(notes, n)
		return n
	}
	const cmds = "a repo file cannot define commands unless trusted"
	const dest = "a repo file cannot choose where source is sent unless trusted"
	out := l
	out.sections = map[string]bool{}
	for k, v := range l.sections {
		out.sections[k] = v
	}
	if l.sections["embed"] {
		drop("embed", dest)
		out.sections["embed"], out.cfg.Embed = false, Embed{}
	}
	if l.sections["summarize"] {
		drop("summarize", dest)
		out.sections["summarize"], out.cfg.Summarize = false, provider.SummarizeConfig{}
	}
	out.cfg.Extractors = nil
	for _, n := range sortedKeys(l.cfg.Extractors) {
		gate["extractors."+n] = drop("extractors."+n, cmds)
	}
	out.cfg.Languages = map[string]Language{}
	for _, n := range sortedKeys(l.cfg.Languages) {
		lang := l.cfg.Languages[n]
		if lang.Command != "" {
			gate["languages."+n+".command"] = drop("languages."+n+".command", cmds)
		}
		if len(lang.Args) > 0 {
			drop("languages."+n+".args", cmds)
		}
		if len(lang.Env) > 0 {
			drop("languages."+n+".env", cmds)
		}
		lang.Command, lang.Args, lang.Env = "", nil, nil
		out.cfg.Languages[n] = lang
	}
	return out, notes, gate
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func sortedRepoKeys(m map[string]Config) []string { return sortedKeys(m) }

// canonPath is p made comparable: ~/ expanded, absolute, symlinks resolved and
// cleaned. When the path does not exist (EvalSymlinks fails) it is only made
// absolute and cleaned.
func canonPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[1:])
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p)
}

// canonKeys maps the canonical path of each repos key to the key. A key that is
// neither absolute nor under ~/ is an error, and so are two keys with the same
// canonical path.
func canonKeys(repos map[string]Config) (map[string]string, error) {
	out := map[string]string{}
	for _, key := range sortedRepoKeys(repos) {
		if !strings.HasPrefix(key, "~/") && !filepath.IsAbs(key) {
			return nil, fmt.Errorf("repos.%q: the key must be an absolute path or start with ~/", key)
		}
		c := canonPath(key)
		if other, dup := out[c]; dup {
			return nil, fmt.Errorf("repos.%q and repos.%q are the same directory (%s)", other, key, c)
		}
		out[c] = key
	}
	return out, nil
}

// validateRepos checks the shape of every repos entry, whether or not it
// matches the current repo: no nested repos, well-formed extractors, no two
// keys for one directory.
func (c Config) validateRepos() error {
	for _, key := range sortedRepoKeys(c.Repos) {
		e := c.Repos[key]
		if len(e.Repos) > 0 {
			return fmt.Errorf("repos.%q.repos is not allowed (a repos entry cannot hold repos)", key)
		}
		for _, n := range sortedKeys(e.Extractors) {
			if err := validateExtractor(n, e.Extractors[n]); err != nil {
				return fmt.Errorf("repos.%q.%w", key, err)
			}
		}
	}
	_, err := canonKeys(c.Repos)
	return err
}
