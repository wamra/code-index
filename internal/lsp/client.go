package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Spec describes how to launch a language server.
type Spec struct {
	Command string
	Args    []string
	Dir     string
	Env     []string // extra KEY=VALUE entries
	// RequestTimeout bounds each request attempt (default 30 s).
	RequestTimeout time.Duration
	// StartupTimeout bounds initialize and the retries while the server
	// reports it is still starting (default 120 s).
	StartupTimeout time.Duration
}

// Capabilities are the server capabilities the indexer relies on.
type Capabilities struct {
	DocumentSymbol bool
	Hover          bool
}

// Client talks to one language server process.
type Client struct {
	c    *conn
	spec Spec
	cmd  *exec.Cmd
	caps Capabilities
	// ServerName and ServerVersion come from initialize's serverInfo.
	ServerName, ServerVersion string

	stderr *tailBuffer
	closer io.Closer
	once   sync.Once
}

// tailBuffer keeps the last bytes written to it (the server's stderr).
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4096 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(bytes.ToValidUTF8(t.buf, nil)))
}

// Start launches the server process and returns an uninitialized client.
func Start(spec Spec) (*Client, error) {
	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tail := &tailBuffer{}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.Command, err)
	}
	cl := newClient(stdout, stdin, spec)
	cl.cmd = cmd
	cl.stderr = tail
	cl.closer = stdin
	return cl, nil
}

// NewClient builds a client over an existing reader/writer pair (tests, pipes).
func NewClient(r io.Reader, w io.Writer, spec Spec) *Client {
	cl := newClient(r, w, spec)
	if c, ok := w.(io.Closer); ok {
		cl.closer = c
	}
	return cl
}

func newClient(r io.Reader, w io.Writer, spec Spec) *Client {
	if spec.RequestTimeout <= 0 {
		spec.RequestTimeout = 30 * time.Second
	}
	if spec.StartupTimeout <= 0 {
		spec.StartupTimeout = 120 * time.Second
	}
	cl := &Client{spec: spec, stderr: &tailBuffer{}}
	cl.c = newConn(r, w)
	cl.c.onRequest = answerServerRequest
	return cl
}

// answerServerRequest handles server-to-client requests. workspace/configuration
// gets an array of nulls with one entry per requested item; every other request
// (client/registerCapability, window/workDoneProgress/create, ...) gets null.
func answerServerRequest(method string, params json.RawMessage) (any, *RPCError) {
	if method == "workspace/configuration" {
		var p struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(params, &p)
		return make([]any, len(p.Items)), nil
	}
	return nil, nil
}

// Stderr returns the tail of the server's stderr (empty for NewClient).
func (cl *Client) Stderr() string { return cl.stderr.String() }

// Capabilities returns what the server advertised in initialize.
func (cl *Client) Capabilities() Capabilities { return cl.caps }

// PathToURI converts an absolute file path to a file:// URI.
func PathToURI(p string) string {
	p = filepath.ToSlash(p)
	if runtime.GOOS == "windows" && !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

var startingRe = regexp.MustCompile(`(?i)(is starting|still starting|not (yet )?(initialized|ready)|initializing|loading|indexing)`)

// isStarting reports an error the server uses to say it is not ready yet:
// ServerNotInitialized (-32002), ContentModified (-32801), or a message that
// says so.
func isStarting(err error) bool {
	var re *RPCError
	if !errors.As(err, &re) {
		return false
	}
	return re.Code == -32002 || re.Code == -32801 || startingRe.MatchString(re.Message)
}

// Call sends a request. A response that says the server is starting is retried
// with a short backoff until the startup budget is spent; every attempt gets
// its own per-request timeout.
func (cl *Client) Call(ctx context.Context, method string, params any, result any) error {
	deadline := time.Now().Add(cl.spec.StartupTimeout)
	delay := 200 * time.Millisecond
	for {
		actx, cancel := context.WithTimeout(ctx, cl.spec.RequestTimeout)
		raw, err := cl.c.call(actx, method, params)
		cancel()
		if err == nil {
			if result != nil && len(raw) > 0 {
				return json.Unmarshal(raw, result)
			}
			return nil
		}
		if !isStarting(err) || time.Now().Add(delay).After(deadline) {
			return cl.wrap(method, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func (cl *Client) wrap(method string, err error) error {
	if errors.Is(err, ErrClosed) {
		if s := cl.Stderr(); s != "" {
			return fmt.Errorf("%s: %w (stderr: %s)", method, err, tailLine(s))
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: timed out after %s", method, cl.spec.RequestTimeout)
	}
	return fmt.Errorf("%s: %w", method, err)
}

func tailLine(s string) string {
	lines := strings.Split(s, "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Notify sends a notification.
func (cl *Client) Notify(method string, params any) error { return cl.c.notify(method, params) }

// Initialize performs the initialize / initialized handshake for root.
func (cl *Client) Initialize(ctx context.Context, root string) error {
	rootURI := PathToURI(root)
	params := map[string]any{
		"processId":  os.Getpid(),
		"clientInfo": map[string]any{"name": "code-index"},
		"rootUri":    rootURI,
		"rootPath":   root,
		"workspaceFolders": []map[string]any{
			{"uri": rootURI, "name": filepath.Base(root)},
		},
		"capabilities": map[string]any{
			"workspace": map[string]any{"configuration": true, "workspaceFolders": true},
			"textDocument": map[string]any{
				"synchronization": map[string]any{"didSave": false},
				"documentSymbol":  map[string]any{"hierarchicalDocumentSymbolSupport": true},
				"hover":           map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
			},
		},
	}
	ictx, cancel := context.WithTimeout(ctx, cl.spec.StartupTimeout)
	defer cancel()
	raw, err := cl.c.call(ictx, "initialize", params)
	if err != nil {
		return cl.wrap("initialize", err)
	}
	var res struct {
		Capabilities struct {
			DocumentSymbolProvider json.RawMessage `json:"documentSymbolProvider"`
			HoverProvider          json.RawMessage `json:"hoverProvider"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("initialize result: %w", err)
	}
	cl.caps = Capabilities{
		DocumentSymbol: provided(res.Capabilities.DocumentSymbolProvider),
		Hover:          provided(res.Capabilities.HoverProvider),
	}
	cl.ServerName, cl.ServerVersion = res.ServerInfo.Name, res.ServerInfo.Version
	return cl.Notify("initialized", map[string]any{})
}

// provided interprets a capability value: true or an options object.
func provided(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "false"
}

// DidOpen opens a document.
func (cl *Client) DidOpen(uri, languageID string, version int, text string) error {
	return cl.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": languageID, "version": version, "text": text},
	})
}

// DidClose closes a document.
func (cl *Client) DidClose(uri string) error {
	return cl.Notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": uri}})
}

// ErrNotAdvertised means the server did not advertise the capability.
var ErrNotAdvertised = errors.New("capability not advertised by the server")

// DocumentSymbols requests textDocument/documentSymbol.
func (cl *Client) DocumentSymbols(ctx context.Context, uri string) ([]Symbol, error) {
	if !cl.caps.DocumentSymbol {
		return nil, fmt.Errorf("documentSymbol: %w", ErrNotAdvertised)
	}
	var raw json.RawMessage
	err := cl.Call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}, &raw)
	if err != nil {
		return nil, err
	}
	return ParseDocumentSymbols(raw)
}

// Hover requests textDocument/hover and returns its flattened text.
func (cl *Client) Hover(ctx context.Context, uri string, pos Position) (string, error) {
	if !cl.caps.Hover {
		return "", fmt.Errorf("hover: %w", ErrNotAdvertised)
	}
	var raw json.RawMessage
	err := cl.Call(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
	}, &raw)
	if err != nil {
		return "", err
	}
	return HoverText(raw), nil
}

// Close shuts the server down: shutdown request, exit notification, then the
// process is waited for (and killed after a few seconds).
func (cl *Client) Close() {
	cl.once.Do(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = cl.c.call(sctx, "shutdown", nil)
		cancel()
		_ = cl.c.notify("exit", nil)
		if cl.closer != nil {
			_ = cl.closer.Close()
		}
		if cl.cmd != nil {
			done := make(chan struct{})
			go func() { _ = cl.cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cl.cmd.Process.Kill()
				<-done
			}
		}
	})
}
