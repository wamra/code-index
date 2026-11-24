// Package lsptest is a tiny fake language server for tests. It "parses"
// source text line by line: `function NAME`, `NAME()` (shell) and
// `class NAME` lines become symbols, and the comment lines directly above a
// symbol become its hover documentation.
package lsptest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/wamra/code-index/internal/lsp"
)

// Options select the quirks the fake server exhibits.
type Options struct {
	Flat           bool   // answer documentSymbol with SymbolInformation[]
	Junk           string // printed on stdout before the first header
	EmptyFirst     int    // the first N documentSymbol requests answer [] (no error)
	Starting       int    // the first N documentSymbol/hover requests fail with "server is starting"
	NoHover        bool   // do not advertise hoverProvider
	NoDocSymbol    bool   // do not advertise documentSymbolProvider
	FailPath       string // documentSymbol fails for URIs containing this text
	AskConfig      int    // send workspace/configuration with this many items after initialized
	ConfigReply    func(raw json.RawMessage)
	Refs           bool      // advertise referencesProvider and answer it by scanning the workspace for the word at the position
	RefsFail       bool      // textDocument/references answers an error
	CallHier       bool      // advertise callHierarchyProvider; prepareCallHierarchy and incomingCalls scan the workspace like references, minus every declaration
	CallHierFail   bool      // callHierarchy/incomingCalls answers an error (implies CallHier)
	DiePath        string    // the server exits when documentSymbol, prepareCallHierarchy or references arrive for a URI containing this text
	OpenCalls      *[]string // records didOpen URIs
	ConfigRepliesN *int
}

// ParseOptions reads the CODE_INDEX_FAKE_LSP value: comma-separated flags
// "flat", "junk", "starting=N", "nohover", "nodocsymbol", "failpath=TEXT", "config=N",
// "refs", "refsfail", "callhier", "callhierfail", "diepath=TEXT".
func ParseOptions(s string) Options {
	var o Options
	for _, f := range strings.Split(s, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		switch k {
		case "flat":
			o.Flat = true
		case "junk":
			o.Junk = "fake language server banner: not an LSP message\n"
		case "emptyfirst":
			o.EmptyFirst, _ = strconv.Atoi(v)
		case "starting":
			o.Starting, _ = strconv.Atoi(v)
		case "nohover":
			o.NoHover = true
		case "nodocsymbol":
			o.NoDocSymbol = true
		case "refs":
			o.Refs = true
		case "refsfail":
			o.Refs, o.RefsFail = true, true
		case "callhier":
			o.CallHier = true
		case "callhierfail":
			o.CallHier, o.CallHierFail = true, true
		case "diepath":
			o.DiePath = v
		case "failpath":
			o.FailPath = v
		case "config":
			o.AskConfig, _ = strconv.Atoi(v)
		}
	}
	return o
}

// Main serves on stdin/stdout with options from $CODE_INDEX_FAKE_LSP.
func Main() {
	Serve(os.Stdin, os.Stdout, ParseOptions(os.Getenv("CODE_INDEX_FAKE_LSP")))
}

type pos struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type rng struct {
	Start pos `json:"start"`
	End   pos `json:"end"`
}

type sym struct {
	name       string
	kind       int
	r, sel     rng
	doc        string
	childrenOf int // index of the parent symbol or -1
}

var (
	fnRe    = regexp.MustCompile(`^(\s*)function\s+([\w-]+)`)
	shRe    = regexp.MustCompile(`^(\s*)([A-Za-z_][\w-]*)\s*\(\)\s*\{`)
	classRe = regexp.MustCompile(`^(\s*)class\s+([\w-]+)`)
)

func parse(text string) []sym {
	lines := strings.Split(text, "\n")
	var syms []sym
	for i, l := range lines {
		var m []string
		kind := 12
		switch {
		case classRe.MatchString(l):
			m, kind = classRe.FindStringSubmatch(l), 5
		case fnRe.MatchString(l):
			m = fnRe.FindStringSubmatch(l)
		case shRe.MatchString(l):
			m = shRe.FindStringSubmatch(l)
		default:
			continue
		}
		indent, name := m[1], m[2]
		end := i
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], indent+"}") {
				end = j
				break
			}
			end = j
		}
		col := strings.Index(l, name)
		var doc []string
		for j := i - 1; j >= 0 && strings.HasPrefix(strings.TrimSpace(lines[j]), "#"); j-- {
			doc = append([]string{strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[j]), "#"))}, doc...)
		}
		syms = append(syms, sym{
			name: name, kind: kind, doc: strings.Join(doc, " "), childrenOf: -1,
			r:   rng{pos{i, 0}, pos{end, len(lines[end])}},
			sel: rng{pos{i, col}, pos{i, col + len(name)}},
		})
	}
	// parents: innermost earlier symbol whose range contains this one
	for i := range syms {
		for j := i - 1; j >= 0; j-- {
			if syms[j].r.Start.Line <= syms[i].r.Start.Line && syms[j].r.End.Line >= syms[i].r.End.Line {
				syms[i].childrenOf = j
				break
			}
		}
	}
	return syms
}

func hierarchical(syms []sym, parent int) []map[string]any {
	var out []map[string]any
	for i, s := range syms {
		if s.childrenOf != parent {
			continue
		}
		out = append(out, map[string]any{
			"name": s.name, "kind": s.kind, "range": s.r, "selectionRange": s.sel,
			"children": hierarchical(syms, i),
		})
	}
	return out
}

// Serve runs the server until exit or EOF.
func Serve(in io.Reader, out io.Writer, o Options) {
	br := bufio.NewReader(in)
	var wmu sync.Mutex
	send := func(v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		fmt.Fprintf(out, "Content-Length: %d\r\n\r\n%s", len(b), b)
	}
	if o.Junk != "" {
		fmt.Fprint(out, o.Junk)
	}
	docs := map[string]string{}
	root := ""
	startingLeft := o.Starting
outer:
	for {
		var n int
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
				n, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(body, &m) != nil {
			continue
		}
		if m.Method == "" { // a response to our workspace/configuration request
			if o.ConfigReply != nil {
				o.ConfigReply(m.Result)
			}
			continue
		}
		reply := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result}) }
		fail := func(code int, msg string) {
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": code, "message": msg}})
		}
		switch m.Method {
		case "initialize":
			caps := map[string]any{"textDocumentSync": 1}
			if !o.NoDocSymbol {
				caps["documentSymbolProvider"] = true
			}
			if o.Refs {
				caps["referencesProvider"] = true
			}
			if o.CallHier {
				caps["callHierarchyProvider"] = true
			}
			var ip struct {
				RootPath string `json:"rootPath"`
			}
			json.Unmarshal(m.Params, &ip)
			root = ip.RootPath
			if !o.NoHover {
				caps["hoverProvider"] = map[string]any{}
			}
			reply(map[string]any{"capabilities": caps, "serverInfo": map[string]any{"name": "fake-lsp", "version": "0"}})
		case "initialized":
			if o.AskConfig > 0 {
				items := make([]any, o.AskConfig)
				for i := range items {
					items[i] = map[string]any{"section": "s" + strconv.Itoa(i)}
				}
				send(map[string]any{"jsonrpc": "2.0", "id": 9001, "method": "workspace/configuration", "params": map[string]any{"items": items}})
			}
		case "textDocument/didOpen":
			var p struct {
				TextDocument struct{ URI, Text string } `json:"textDocument"`
			}
			json.Unmarshal(m.Params, &p)
			docs[p.TextDocument.URI] = p.TextDocument.Text
			if o.OpenCalls != nil {
				*o.OpenCalls = append(*o.OpenCalls, p.TextDocument.URI)
			}
		case "textDocument/didClose":
			var p struct {
				TextDocument struct{ URI string } `json:"textDocument"`
			}
			json.Unmarshal(m.Params, &p)
			delete(docs, p.TextDocument.URI)
		case "textDocument/documentSymbol", "textDocument/hover":
			if startingLeft > 0 {
				startingLeft--
				fail(-32803, "server is starting")
				continue
			}
			var p struct {
				TextDocument struct{ URI string } `json:"textDocument"`
				Position     pos                  `json:"position"`
			}
			json.Unmarshal(m.Params, &p)
			text, open := docs[p.TextDocument.URI]
			if !open {
				fail(-32602, "document not open")
				continue
			}
			syms := parse(text)
			if m.Method == "textDocument/documentSymbol" && o.DiePath != "" && strings.Contains(p.TextDocument.URI, o.DiePath) {
				return
			}
			if m.Method == "textDocument/hover" {
				for _, s := range syms {
					if s.sel.Start == p.Position {
						val := "```powershell\nfunction " + s.name + "\n```"
						if s.doc != "" {
							val += "\n---\n" + s.doc
						}
						reply(map[string]any{"contents": map[string]any{"kind": "markdown", "value": val}})
						continue outer
					}
				}
				reply(nil)
				continue
			}
			if o.FailPath != "" && strings.Contains(p.TextDocument.URI, o.FailPath) {
				fail(-32603, "internal error in fake server")
				continue
			}
			if o.EmptyFirst > 0 {
				o.EmptyFirst--
				reply([]any{})
				continue
			}
			if o.Flat {
				var flat []map[string]any
				for _, s := range syms {
					e := map[string]any{"name": s.name, "kind": s.kind, "location": map[string]any{"uri": p.TextDocument.URI, "range": s.r}}
					if s.childrenOf >= 0 {
						e["containerName"] = syms[s.childrenOf].name
					}
					flat = append(flat, e)
				}
				reply(flat)
			} else {
				reply(hierarchical(syms, -1))
			}
		case "textDocument/prepareCallHierarchy":
			var p struct {
				TextDocument struct{ URI string } `json:"textDocument"`
				Position     pos                  `json:"position"`
			}
			json.Unmarshal(m.Params, &p)
			if o.DiePath != "" && strings.Contains(p.TextDocument.URI, o.DiePath) {
				return
			}
			text, open := docs[p.TextDocument.URI]
			if !open {
				fail(-32602, "document not open")
				continue
			}
			items := []map[string]any{}
			for _, s := range parse(text) {
				if s.sel.Start == p.Position {
					items = append(items, callItem(p.TextDocument.URI, s))
				}
			}
			if len(items) == 0 {
				reply(nil)
				continue
			}
			reply(items)
		case "callHierarchy/incomingCalls":
			if o.CallHierFail {
				fail(-32603, "incomingCalls failed in fake server")
				continue
			}
			var p struct {
				Item struct {
					Name           string `json:"name"`
					URI            string `json:"uri"`
					SelectionRange rng    `json:"selectionRange"`
				} `json:"item"`
			}
			json.Unmarshal(m.Params, &p)
			reply(incomingCalls(root, p.Item.URI, p.Item.Name, p.Item.SelectionRange.Start))
		case "textDocument/references":
			if o.DiePath != "" && strings.Contains(string(m.Params), o.DiePath) {
				return
			}
			if o.RefsFail {
				fail(-32603, "references failed in fake server")
				continue
			}
			var p struct {
				TextDocument struct{ URI string } `json:"textDocument"`
				Position     pos                  `json:"position"`
			}
			json.Unmarshal(m.Params, &p)
			text, open := docs[p.TextDocument.URI]
			if !open {
				fail(-32602, "document not open")
				continue
			}
			reply(references(root, p.TextDocument.URI, text, p.Position))
		case "shutdown":
			reply(nil)
		case "exit":
			return
		default:
			if len(m.ID) > 0 {
				reply(nil)
			}
		}
	}
}

var declBeforeRe = regexp.MustCompile(`\b(function|class)\s+$`)

var wordRe = regexp.MustCompile(`[\w-]+`)

// occurrence is one whole-word hit of the workspace scan.
type occurrence struct {
	uri       string
	path      string
	line      int
	start     int
	end       int
	fileLines []string // the whole file, for finding the enclosing symbol
}

// wordAt returns the identifier at the position, "" if there is none.
func wordAt(text string, at pos) string {
	lines := strings.Split(text, "\n")
	if at.Line >= len(lines) {
		return ""
	}
	var word string
	for _, m := range wordRe.FindAllStringIndex(lines[at.Line], -1) {
		if m[0] <= at.Character && at.Character <= m[1] {
			word = lines[at.Line][m[0]:m[1]]
		}
	}
	return word
}

// scan walks root and returns every whole-word occurrence of word except the
// occurrence at the position at in declURI (the declaration asked about) and
// the ones that follow "function " or "class " (other declarations of the name).
func scan(root, declURI, word string, at pos) []occurrence {
	var out []occurrence
	if word == "" || root == "" {
		return out
	}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == ".Wammy" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		u := lsp.PathToURI(p)
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			for _, m := range wordRe.FindAllStringIndex(l, -1) {
				if l[m[0]:m[1]] != word || (u == declURI && i == at.Line && m[0] == at.Character) || declBeforeRe.MatchString(l[:m[0]]) {
					continue // not the word, the declaration asked about, or another declaration of the name
				}
				out = append(out, occurrence{uri: u, path: p, line: i, start: m[0], end: m[1], fileLines: lines})
			}
		}
		return nil
	})
	return out
}

// references answers textDocument/references the way a naive server would:
// every whole-word occurrence of the word at the position in any file under
// root, except the occurrence at the position itself (the declaration).
func references(root, uri, text string, at pos) []map[string]any {
	out := []map[string]any{}
	for _, o := range scan(root, uri, wordAt(text, at), at) {
		out = append(out, map[string]any{"uri": o.uri, "range": rng{pos{o.line, o.start}, pos{o.line, o.end}}})
	}
	return out
}

// callItem is the CallHierarchyItem of a symbol.
func callItem(uri string, s sym) map[string]any {
	return map[string]any{
		"name": s.name, "kind": s.kind, "uri": uri, "range": s.r, "selectionRange": s.sel,
		"data": map[string]any{"fake": s.name},
	}
}

// incomingCalls answers callHierarchy/incomingCalls with the same scan as
// references, grouped by the symbol that encloses each occurrence (a file
// item when none does). The declaration of the item is never a call.
func incomingCalls(root, uri, name string, at pos) []map[string]any {
	type group struct {
		item   map[string]any
		ranges []rng
	}
	var order []string
	groups := map[string]*group{}
	for _, o := range scan(root, uri, name, at) {
		var encl *sym
		syms := parse(strings.Join(o.fileLines, "\n"))
		for i := range syms {
			s := syms[i]
			if s.r.Start.Line <= o.line && o.line <= s.r.End.Line && (encl == nil || s.r.Start.Line >= encl.r.Start.Line) {
				encl = &syms[i]
			}
		}
		var item map[string]any
		key := o.uri
		if encl != nil {
			item = callItem(o.uri, *encl)
			key += "#" + encl.name
		} else {
			item = map[string]any{"name": filepath.Base(o.path), "kind": 1, "uri": o.uri, "range": rng{}, "selectionRange": rng{}}
		}
		g := groups[key]
		if g == nil {
			g = &group{item: item}
			groups[key] = g
			order = append(order, key)
		}
		g.ranges = append(g.ranges, rng{pos{o.line, o.start}, pos{o.line, o.end}})
	}
	out := []map[string]any{}
	for _, k := range order {
		out = append(out, map[string]any{"from": groups[k].item, "fromRanges": groups[k].ranges})
	}
	return out
}
