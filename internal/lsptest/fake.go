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
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	OpenCalls      *[]string // records didOpen URIs
	ConfigRepliesN *int
}

// ParseOptions reads the CODE_INDEX_FAKE_LSP value: comma-separated flags
// "flat", "junk", "starting=N", "nohover", "nodocsymbol", "failpath=TEXT", "config=N".
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
