package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wys1203/go-ai-orchestration/internal/config"
)

// fakeServer replays scripted JSON bodies and records requests.
type fakeServer struct {
	*httptest.Server
	script   []string
	statuses []int
	calls    int
	bodies   []map[string]any
	headers  []http.Header
}

func newFakeServer(t *testing.T, script ...string) *fakeServer {
	f := &fakeServer{script: script}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		f.bodies = append(f.bodies, m)
		f.headers = append(f.headers, r.Header.Clone())
		i := f.calls
		f.calls++
		if i >= len(f.script) {
			http.Error(w, "script exhausted", 500)
			return
		}
		if i < len(f.statuses) && f.statuses[i] != 0 {
			w.WriteHeader(f.statuses[i])
		}
		_, _ = w.Write([]byte(f.script[i]))
	}))
	t.Cleanup(f.Close)
	return f
}

const toolTurn = `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"looking","reasoning_content":"thinking...",
 "tool_calls":[{"id":"t1","type":"function","function":{"name":"echo","arguments":"{\"v\":\"one\"}"}},
               {"id":"t2","type":"function","function":{"name":"boom","arguments":""}}]}}],
 "usage":{"prompt_tokens":10,"completion_tokens":5}}`

const endTurn = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"all done"}}],"usage":{"prompt_tokens":20,"completion_tokens":7}}`

func testCfg(url string) config.LLM {
	return config.LLM{Model: "m", BaseURL: url + "/v1", APIKey: "k", Headers: map[string]string{"X-Title": "gao"},
		MaxTokens: 100, Effort: "high", MaxTurns: 5, RequestTimeout: 5 * time.Second}
}

func TestRunToolLoop(t *testing.T) {
	fs := newFakeServer(t, toolTurn, endTurn)
	r := New(testCfg(fs.URL), nil)
	var executed int32
	exec := ExecutorFunc(func(_ context.Context, name string, input json.RawMessage) (string, bool, error) {
		atomic.AddInt32(&executed, 1)
		switch name {
		case "echo":
			return "echo:" + string(input), false, nil
		case "boom":
			if string(input) != "{}" {
				t.Errorf("empty arguments should become {}: %q", input)
			}
			return "", false, fmt.Errorf("transport down")
		}
		return "", true, nil
	})
	tools := []ToolDef{{Name: "echo", Description: "e", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"v": map[string]any{"type": "string"}}, "required": []string{"v"}}}, {Name: "boom"}}
	res, err := r.Run(context.Background(), "sys", "hi", tools, exec)
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText != "all done" || res.StopReason != "stop" || res.Turns != 2 || res.ToolCalls != 2 || res.InputTokens != 30 || res.OutputTokens != 12 {
		t.Fatalf("result: %+v", res)
	}
	if res.Calls["echo"] != 1 || res.Calls["boom"] != 0 {
		t.Fatalf("successful call counts: %v", res.Calls)
	}
	if executed != 2 {
		t.Fatalf("executed %d", executed)
	}
	first, _ := json.Marshal(fs.bodies[0])
	for _, want := range []string{`"model":"m"`, `"max_completion_tokens":100`, `"reasoning_effort":"high"`, `"type":"function"`, `"name":"echo"`, `"required":["v"]`, `"parameters":{"properties":{}`} {
		if !strings.Contains(string(first), want) {
			t.Errorf("first request missing %q: %s", want, first)
		}
	}
	if h := fs.headers[0]; h.Get("Authorization") != "Bearer k" || h.Get("X-Title") != "gao" {
		t.Errorf("headers: %v", h)
	}
	// second request history: system, user, assistant(tool_calls), tool, tool
	msgs := fs.bodies[1]["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("history len %d", len(msgs))
	}
	second, _ := json.Marshal(msgs)
	for _, want := range []string{`"reasoning_content":"thinking..."`, `"tool_call_id":"t1"`, `echo:{\"v\":\"one\"}`, `"tool_call_id":"t2"`, "tool execution error: transport down"} {
		if !strings.Contains(string(second), want) {
			t.Errorf("history missing %q: %s", want, second)
		}
	}
}

func TestRunMaxTurns(t *testing.T) {
	fs := newFakeServer(t, toolTurn, toolTurn, toolTurn)
	cfg := testCfg(fs.URL)
	cfg.MaxTurns = 2
	r := New(cfg, nil)
	_, err := r.Run(context.Background(), "s", "p", nil, ExecutorFunc(func(context.Context, string, json.RawMessage) (string, bool, error) { return "ok", false, nil }))
	if !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("got %v", err)
	}
}

func TestRunRefusal(t *testing.T) {
	refusal := `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"","refusal":"I can't help with that"}}]}`
	fs := newFakeServer(t, refusal)
	_, err := New(testCfg(fs.URL), nil).Run(context.Background(), "s", "p", nil, nil)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "can't help") {
		t.Fatalf("got %v", err)
	}
}

func TestRunLengthContinues(t *testing.T) {
	cut := `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"partial"}}]}`
	fs := newFakeServer(t, cut, endTurn)
	res, err := New(testCfg(fs.URL), nil).Run(context.Background(), "s", "p", nil, nil)
	if err != nil || res.FinalText != "all done" || res.Turns != 2 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRetryOn429ThenFailOn400(t *testing.T) {
	fs := newFakeServer(t, `{"error":{"message":"slow down"}}`, endTurn)
	fs.statuses = []int{429, 200}
	res, err := New(testCfg(fs.URL), nil).Run(context.Background(), "s", "p", nil, nil)
	if err != nil || res.FinalText != "all done" || fs.calls != 2 {
		t.Fatalf("%+v %v calls=%d", res, err, fs.calls)
	}

	fs2 := newFakeServer(t, `{"error":{"message":"bad"}}`)
	fs2.statuses = []int{400}
	_, err = New(testCfg(fs2.URL), nil).Run(context.Background(), "s", "p", nil, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 || fs2.calls != 1 {
		t.Fatalf("got %v calls=%d", err, fs2.calls)
	}
}

func TestLegacyMaxTokensParam(t *testing.T) {
	fs := newFakeServer(t, endTurn)
	cfg := testCfg(fs.URL)
	cfg.MaxTokensParam = "max_tokens"
	cfg.Effort = ""
	if _, err := New(cfg, nil).Run(context.Background(), "s", "p", nil, nil); err != nil {
		t.Fatal(err)
	}
	b := fs.bodies[0]
	if b["max_tokens"] != float64(100) || b["max_completion_tokens"] != nil || b["reasoning_effort"] != nil {
		t.Fatalf("body: %v", b)
	}
}

type nudgingExec struct {
	ExecutorFunc
	msgs []string
}

func (n *nudgingExec) Nudge(_ Result, calls map[string]int, attempt int) string {
	if calls["echo"] == 0 && attempt <= 1 {
		n.msgs = append(n.msgs, "call echo")
		return "call echo"
	}
	return ""
}

func TestNudgeContinuesLoop(t *testing.T) {
	// stop without tools -> nudge -> tool turn -> stop
	fs := newFakeServer(t, endTurn, toolTurn, endTurn)
	r := New(testCfg(fs.URL), nil)
	ne := &nudgingExec{ExecutorFunc: func(context.Context, string, json.RawMessage) (string, bool, error) { return "ok", false, nil }}
	res, err := r.Run(context.Background(), "s", "p", nil, ne)
	if err != nil || res.Turns != 3 || res.ToolCalls != 2 || len(ne.msgs) != 1 {
		t.Fatalf("%+v %v nudges=%v", res, err, ne.msgs)
	}
	msgs := fs.bodies[1]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" || last["content"] != "call echo" {
		t.Fatalf("nudge not sent: %v", last)
	}
	// nudge gives up after attempt 1 when echo still not called
	fs2 := newFakeServer(t, endTurn, endTurn)
	ne2 := &nudgingExec{ExecutorFunc: ne.ExecutorFunc}
	res, err = New(testCfg(fs2.URL), nil).Run(context.Background(), "s", "p", nil, ne2)
	if err != nil || res.Turns != 2 || fs2.calls != 2 {
		t.Fatalf("%+v %v", res, err)
	}
}
