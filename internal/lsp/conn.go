// Package lsp is a small Language Server Protocol client over stdio JSON-RPC
// with Content-Length framing.
package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// RPCError is a JSON-RPC error response.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message) }

// ErrClosed is returned for requests in flight when the server's stdout ends.
var ErrClosed = errors.New("language server closed the connection")

var contentLengthRe = regexp.MustCompile(`(?i)content-length:\s*(\d+)`)

// readMessage reads one framed message. Lines that carry no Content-Length
// header (a banner or log text printed on stdout before or between messages)
// are skipped.
func readMessage(r *bufio.Reader) ([]byte, error) {
	for {
		line, err := r.ReadString('\n')
		if m := contentLengthRe.FindStringSubmatch(line); m != nil {
			n, perr := strconv.Atoi(m[1])
			if perr != nil || n < 0 || n > 1<<28 {
				return nil, fmt.Errorf("bad Content-Length %q", m[1])
			}
			// remaining header lines up to the blank separator
			for {
				h, herr := r.ReadString('\n')
				if herr != nil {
					return nil, herr
				}
				if strings.TrimRight(h, "\r\n") == "" {
					break
				}
			}
			body := make([]byte, n)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, err
			}
			return body, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func writeMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Content-Length: %d\r\n\r\n", len(b))
	buf.Write(b)
	_, err = w.Write(buf.Bytes())
	return err
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type response struct {
	result json.RawMessage
	err    error
}

// conn multiplexes requests over one reader/writer pair.
type conn struct {
	w       io.Writer
	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[string]chan response
	closed  error
	done    chan struct{}
	// onRequest answers server-to-client requests; the result is sent back.
	onRequest func(method string, params json.RawMessage) (any, *RPCError)
	// onNotify receives server notifications (may be nil).
	onNotify func(method string, params json.RawMessage)
}

func newConn(r io.Reader, w io.Writer) *conn {
	c := &conn{w: w, pending: map[string]chan response{}, done: make(chan struct{})}
	br := bufio.NewReaderSize(r, 1<<16)
	go c.readLoop(br)
	return c
}

func (c *conn) readLoop(br *bufio.Reader) {
	defer close(c.done)
	for {
		body, err := readMessage(br)
		if err != nil {
			if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
				err = ErrClosed
			}
			c.fail(err)
			return
		}
		var m message
		if err := json.Unmarshal(body, &m); err != nil {
			continue // not JSON: ignore like any other stray output
		}
		switch {
		case m.Method != "" && len(m.ID) > 0 && string(m.ID) != "null":
			go c.answer(m)
		case m.Method != "":
			if c.onNotify != nil {
				c.onNotify(m.Method, m.Params)
			}
		case len(m.ID) > 0:
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				if m.Error != nil {
					ch <- response{err: m.Error}
				} else {
					ch <- response{result: m.Result}
				}
			}
		}
	}
}

func (c *conn) answer(m message) {
	var result any
	var rerr *RPCError
	if c.onRequest != nil {
		result, rerr = c.onRequest(m.Method, m.Params)
	}
	reply := map[string]any{"jsonrpc": "2.0", "id": m.ID}
	if rerr != nil {
		reply["error"] = rerr
	} else {
		reply["result"] = result // nil marshals as null
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = writeMessage(c.w, reply)
}

func (c *conn) fail(err error) {
	c.mu.Lock()
	c.closed = err
	pend := c.pending
	c.pending = map[string]chan response{}
	c.mu.Unlock()
	for _, ch := range pend {
		ch <- response{err: err}
	}
}

// call sends a request and waits for its response, ctx expiry or connection loss.
func (c *conn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed != nil {
		err := c.closed
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	ch := make(chan response, 1)
	c.pending[key] = ch
	c.mu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	c.wmu.Lock()
	err := writeMessage(c.w, req)
	c.wmu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		// best effort: tell the server we no longer care
		c.notify("$/cancelRequest", map[string]any{"id": id})
		return nil, ctx.Err()
	}
}

func (c *conn) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return writeMessage(c.w, msg)
}
