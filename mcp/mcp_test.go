package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nogo/faden"
)

type fakeEvents map[string][]faden.Event

func (f fakeEvents) NextEvent(_ context.Context, session string, after int64) (*faden.Event, error) {
	for _, e := range f[session] {
		if e.Seq > after {
			return &e, nil
		}
	}
	return nil, nil
}

func start(t *testing.T) (*Server, string) {
	t.Helper()
	s, err := Listen(fakeEvents{
		"a": {{Session: "a", Seq: 1, Kind: faden.UserMessage, Text: "alpha"}, {Session: "a", Seq: 2, Kind: faden.AssistantMessage, Text: "beta"}, {Session: "a", Seq: 3, Kind: faden.AssistantMessage, Text: "gamma"}},
		"b": {{Session: "b", Seq: 1, Kind: faden.UserMessage, Text: "secret of b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	token, err := s.Grant("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Grant("b"); err != nil {
		t.Fatal(err)
	}
	return s, token
}

func post(t *testing.T, s *Server, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", s.URL(), strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var out map[string]any
	json.Unmarshal(data, &out)
	return res.StatusCode, out
}

func toolText(t *testing.T, out map[string]any) (string, bool) {
	t.Helper()
	result := out["result"].(map[string]any)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	isError, _ := result["isError"].(bool)
	return text, isError
}

func TestInitializeAndList(t *testing.T) {
	s, token := start(t)
	code, out := post(t, s, token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	info := out["result"].(map[string]any)
	if code != 200 || info["serverInfo"].(map[string]any)["name"] != "faden" || info["protocolVersion"] == "" {
		t.Fatalf("initialize: %d %v", code, out)
	}
	code, _ = post(t, s, token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	_, out = post(t, s, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := out["result"].(map[string]any)["tools"].([]any)
	tool := tools[0].(map[string]any)
	if len(tools) != 1 || tool["name"] != "read_thread" || tool["annotations"].(map[string]any)["readOnlyHint"] != true {
		t.Fatalf("tools: %v", tools)
	}
}

func TestCallReadsRange(t *testing.T) {
	s, token := start(t)
	_, out := post(t, s, token, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_thread","arguments":{"from":2,"to":3}}}`)
	text, isError := toolText(t, out)
	if isError || strings.Contains(text, "alpha") || !strings.Contains(text, "beta") || !strings.Contains(text, "gamma") {
		t.Fatalf("range: %q", text)
	}
}

func TestWrongTokenRefused(t *testing.T) {
	s, token := start(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, bad := range []string{"", "nope", token + "x"} {
		if code, _ := post(t, s, bad, body); code != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d", bad, code)
		}
	}
	s.Revoke("a")
	if code, _ := post(t, s, token, body); code != http.StatusUnauthorized {
		t.Fatalf("revoked: status %d", code)
	}
}

func TestOtherSessionRefused(t *testing.T) {
	s, token := start(t)
	_, out := post(t, s, token, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_thread","arguments":{"session":"b","from":1,"to":1}}}`)
	text, isError := toolText(t, out)
	if !isError || strings.Contains(text, "secret") {
		t.Fatalf("other session: %q error=%v", text, isError)
	}
}

func TestConfigHoldsNoToken(t *testing.T) {
	s, token := start(t)
	cfg := string(s.Config())
	if strings.Contains(cfg, token) || !strings.Contains(cfg, "Bearer ${"+TokenEnv+"}") || !strings.Contains(cfg, s.URL()) {
		t.Fatalf("config: %s", cfg)
	}
}
