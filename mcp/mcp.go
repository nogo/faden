// Package mcp serves one read-only tool, read_thread, to Claude Code over MCP
// (JSON-RPC over HTTP POST) on loopback. Use it so a Claude turn can read the
// events a handover left out.
//
// Each session has its own token. A request carries it as a bearer token and
// can read only that session. The token never goes into a file or an argument:
// Config returns a config file whose header names an environment variable, and
// Grant returns the value for the host to put in Claude's environment.
package mcp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/nogo/faden/handover"
)

// TokenEnv is the environment variable that holds the token for Claude.
const TokenEnv = "FADEN_MCP_TOKEN"

const (
	maxBody  = 1 << 20
	protocol = "2025-03-26"
)

// Events is the part of a faden.Store the server reads.
type Events = handover.Events

// Server serves read_thread for the sessions it has granted.
type Server struct {
	events   Events
	listener net.Listener
	http     *http.Server

	mu     sync.Mutex
	tokens map[string]string // session -> token
}

// Listen starts a server on a free loopback port. Stop it with Close.
func Listen(events Events) (*Server, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcp: listen: %w", err)
	}
	s := &Server{events: events, listener: l, tokens: map[string]string{}}
	s.http = &http.Server{Handler: s}
	go s.http.Serve(l)
	return s, nil
}

// URL is the address of the MCP endpoint.
func (s *Server) URL() string { return "http://" + s.listener.Addr().String() + "/mcp" }

// Close stops the server.
func (s *Server) Close() error { return s.http.Close() }

// Grant returns a new token for the session. An earlier token of the session stops working.
func (s *Server) Grant(session string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mcp: token: %w", err)
	}
	token := hex.EncodeToString(b[:])
	s.mu.Lock()
	s.tokens[session] = token
	s.mu.Unlock()
	return token, nil
}

// Revoke ends the session's token.
func (s *Server) Revoke(session string) {
	s.mu.Lock()
	delete(s.tokens, session)
	s.mu.Unlock()
}

// Config returns the content of an --mcp-config file for this server. Its
// header refers to TokenEnv; it holds no token.
func (s *Server) Config() []byte {
	cfg := map[string]any{"mcpServers": map[string]any{"faden": map[string]any{
		"type":    "http",
		"url":     s.URL(),
		"headers": map[string]string{"Authorization": "Bearer ${" + TokenEnv + "}"},
	}}}
	data, _ := json.Marshal(cfg)
	return data
}

func (s *Server) sessionOf(r *http.Request) (string, bool) {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || got == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := ""
	for session, token := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1 {
			found = session
		}
	}
	return found, found != ""
}

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	session, ok := s.sessionOf(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var req request
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		reply(w, nil, nil, &rpcError{-32700, "parse error"})
		return
	}
	if len(req.ID) == 0 { // a notification expects no answer
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		reply(w, req.ID, map[string]any{
			"protocolVersion": protocol,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "faden", "version": "1"},
		}, nil)
	case "ping":
		reply(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		reply(w, req.ID, map[string]any{"tools": []any{map[string]any{
			"name":        handover.ThreadTool,
			"description": handover.ThreadToolDescription,
			"inputSchema": json.RawMessage(handover.ThreadToolSchema),
			"annotations": map[string]any{"readOnlyHint": true},
		}}}, nil)
	case "tools/call":
		reply(w, req.ID, s.call(r.Context(), session, req.Params), nil)
	default:
		reply(w, req.ID, nil, &rpcError{-32601, "method not found"})
	}
}

func reply(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	if id == nil {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// call runs a tool call and returns an MCP tool result. A failure is a result
// with isError set, so the model can read it.
func (s *Server) call(ctx context.Context, session string, params json.RawMessage) map[string]any {
	text, err := s.readThread(ctx, session, params)
	if err != nil {
		return map[string]any{"isError": true, "content": []any{map[string]string{"type": "text", "text": err.Error()}}}
	}
	return map[string]any{"content": []any{map[string]string{"type": "text", "text": text}}}
}

func (s *Server) readThread(ctx context.Context, own string, params json.RawMessage) (string, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	if p.Name != handover.ThreadTool {
		return "", errors.New("unknown tool " + p.Name)
	}
	if len(p.Arguments) == 0 {
		p.Arguments = json.RawMessage("{}")
	}
	return handover.ReadThread(ctx, s.events, own, p.Arguments)
}
