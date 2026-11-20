package lsp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wamra/code-index/internal/lsp"
	"github.com/wamra/code-index/internal/lsptest"
)

const sample = `# Returns a widget.
function Get-Widget {
    param($n)
}

class Box {
    function Open {
    }
}
`

func pipeClient(t *testing.T, o lsptest.Options) *lsp.Client {
	t.Helper()
	cr, sw := io.Pipe() // server -> client
	sr, cw := io.Pipe() // client -> server
	go lsptest.Serve(sr, sw, o)
	cl := lsp.NewClient(cr, cw, lsp.Spec{RequestTimeout: 5 * time.Second, StartupTimeout: 10 * time.Second})
	t.Cleanup(cl.Close)
	return cl
}

func TestHierarchicalSymbolsAndHover(t *testing.T) {
	cl := pipeClient(t, lsptest.Options{})
	ctx := context.Background()
	if err := cl.Initialize(ctx, "/ws"); err != nil {
		t.Fatal(err)
	}
	if c := cl.Capabilities(); !c.DocumentSymbol || !c.Hover {
		t.Fatalf("caps = %+v", c)
	}
	uri := "file:///ws/a.ps1"
	cl.DidOpen(uri, "powershell", 1, sample)
	syms, err := cl.DocumentSymbols(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 2 || syms[0].Name != "Get-Widget" || syms[0].Kind != "function" {
		t.Fatalf("syms = %+v", syms)
	}
	if syms[1].Name != "Box" || len(syms[1].Children) != 1 || syms[1].Children[0].Name != "Open" {
		t.Fatalf("hierarchy lost: %+v", syms[1])
	}
	h, err := cl.Hover(ctx, uri, syms[0].SelectionRange.Start)
	if err != nil {
		t.Fatal(err)
	}
	sig, doc := lsp.ParseHover(h)
	if sig != "function Get-Widget" || doc != "Returns a widget." {
		t.Fatalf("sig=%q doc=%q (hover %q)", sig, doc, h)
	}
}

func TestFlatSymbols(t *testing.T) {
	cl := pipeClient(t, lsptest.Options{Flat: true})
	ctx := context.Background()
	if err := cl.Initialize(ctx, "/ws"); err != nil {
		t.Fatal(err)
	}
	uri := "file:///ws/a.ps1"
	cl.DidOpen(uri, "powershell", 1, sample)
	syms, err := cl.DocumentSymbols(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 3 {
		t.Fatalf("want 3 flat symbols, got %+v", syms)
	}
	if syms[2].Name != "Open" || syms[2].ContainerName != "Box" || len(syms[2].Children) != 0 {
		t.Fatalf("flat symbol = %+v", syms[2])
	}
	if syms[0].Range.End.Line < 2 {
		t.Fatalf("range lost: %+v", syms[0].Range)
	}
}

func TestJunkBeforeHeaderStartingRetryAndConfiguration(t *testing.T) {
	var mu sync.Mutex
	var replies []json.RawMessage
	o := lsptest.Options{
		Junk: "banner line without header\nanother line\n", Starting: 2, AskConfig: 3,
		ConfigReply: func(raw json.RawMessage) { mu.Lock(); replies = append(replies, raw); mu.Unlock() },
	}
	cl := pipeClient(t, o)
	ctx := context.Background()
	if err := cl.Initialize(ctx, "/ws"); err != nil {
		t.Fatalf("initialize after junk: %v", err)
	}
	uri := "file:///ws/a.ps1"
	cl.DidOpen(uri, "powershell", 1, sample)
	start := time.Now()
	syms, err := cl.DocumentSymbols(ctx, uri) // first two attempts: "server is starting"
	if err != nil || len(syms) != 2 {
		t.Fatalf("retry failed: %v %+v", err, syms)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Errorf("expected backoff between retries, took %v", time.Since(start))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(replies)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(replies) != 1 || string(replies[0]) != "[null,null,null]" {
		t.Fatalf("workspace/configuration reply = %q, want [null,null,null]", replies)
	}
}

func TestCapabilitiesNotAdvertised(t *testing.T) {
	cl := pipeClient(t, lsptest.Options{NoHover: true, NoDocSymbol: true})
	if err := cl.Initialize(context.Background(), "/ws"); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.DocumentSymbols(context.Background(), "file:///x"); !errors.Is(err, lsp.ErrNotAdvertised) {
		t.Fatalf("documentSymbol err = %v", err)
	}
	if _, err := cl.Hover(context.Background(), "file:///x", lsp.Position{}); !errors.Is(err, lsp.ErrNotAdvertised) {
		t.Fatalf("hover err = %v", err)
	}
}

func TestRequestTimeout(t *testing.T) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go io.Copy(io.Discard, sr) // a server that never answers
	_ = sw
	cl := lsp.NewClient(cr, cw, lsp.Spec{RequestTimeout: 200 * time.Millisecond})
	err := cl.Call(context.Background(), "x", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
	t.Log(err)
}

func TestServerExitFailsPending(t *testing.T) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go func() { io.Copy(io.Discard, sr) }()
	cl := lsp.NewClient(cr, cw, lsp.Spec{RequestTimeout: 5 * time.Second})
	go func() { time.Sleep(100 * time.Millisecond); sw.Close() }()
	err := cl.Call(context.Background(), "x", nil, nil)
	if !errors.Is(err, lsp.ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestParseHoverVariants(t *testing.T) {
	cases := []struct{ raw, sig, doc string }{
		{`{"contents":"plain text\nsecond"}`, "plain text", "second"},
		{`{"contents":{"language":"bash","value":"build_release()"}}`, "build_release()", ""},
		{`{"contents":["intro",{"language":"ps","value":"function A"}]}`, "function A", "intro"},
		{`{"contents":{"kind":"markdown","value":"Run it\n\n` + "```sh\\nfoo --bar\\n```" + `\n\nmore"}}`, "foo --bar", "Run it\n\nmore"},
	}
	for _, c := range cases {
		sig, doc := lsp.ParseHover(lsp.HoverText(json.RawMessage(c.raw)))
		if sig != c.sig || doc != c.doc {
			t.Errorf("%s\n got sig=%q doc=%q\nwant sig=%q doc=%q", c.raw, sig, doc, c.sig, c.doc)
		}
	}
	if lsp.HoverText(json.RawMessage("null")) != "" {
		t.Error("null hover must be empty")
	}
}

func TestOtherServerRequestsGetNull(t *testing.T) {
	// answerServerRequest is internal; exercise it through a raw exchange.
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	_ = lsp.NewClient(cr, cw, lsp.Spec{})
	go func() {
		msg := `{"jsonrpc":"2.0","id":"abc","method":"client/registerCapability","params":{}}`
		io.WriteString(sw, "Content-Length: "+itoa(len(msg))+"\r\n\r\n"+msg)
	}()
	buf := make([]byte, 256)
	n, _ := sr.Read(buf)
	got := string(buf[:n])
	if want := `"id":"abc"`; !contains(got, want) || !contains(got, `"result":null`) {
		t.Fatalf("reply = %q", got)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestReferences(t *testing.T) {
	root := t.TempDir()
	src := "function Alpha {\n  Beta\n}\nfunction Beta {\n}\n"
	file := filepath.Join(root, "a.ps1")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	cl := pipeClient(t, lsptest.Options{})
	if err := cl.Initialize(ctx, root); err != nil {
		t.Fatal(err)
	}
	if cl.Capabilities().References {
		t.Fatal("references must not be advertised by default")
	}
	if _, err := cl.References(ctx, lsp.PathToURI(file), lsp.Position{Line: 3, Character: 9}); !errors.Is(err, lsp.ErrNotAdvertised) {
		t.Fatalf("References without capability: %v", err)
	}

	cl = pipeClient(t, lsptest.Options{Refs: true})
	if err := cl.Initialize(ctx, root); err != nil {
		t.Fatal(err)
	}
	if !cl.Capabilities().References {
		t.Fatal("referencesProvider not read")
	}
	uri := lsp.PathToURI(file)
	cl.DidOpen(uri, "powershell", 1, src)
	locs, err := cl.References(ctx, uri, lsp.Position{Line: 3, Character: 9}) // Beta in its declaration
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) != 1 || locs[0].URI != uri || locs[0].Range.Start != (lsp.Position{Line: 1, Character: 2}) {
		t.Fatalf("locations = %+v", locs)
	}
}
