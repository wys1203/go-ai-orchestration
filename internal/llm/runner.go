// Package llm drives the tool-use loop against any OpenAI-compatible Chat
// Completions endpoint (OpenAI, Azure-style gateways, OpenRouter, vLLM,
// Ollama, LM Studio, ...). It uses net/http only.
//
// It owns the message history so assistant tool_calls and provider-specific
// fields such as reasoning_content round-trip unchanged, executes every tool
// call in one assistant turn concurrently, and appends one tool message per
// call as the protocol requires.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wys1203/go-ai-orchestration/internal/config"
)

// ToolDef is a provider-neutral tool definition.
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// Executor runs a tool call. isError marks the result as a tool failure the
// model should see; a returned error is also surfaced to the model unless
// the context is done.
type Executor interface {
	Execute(ctx context.Context, name string, input json.RawMessage) (content string, isError bool, err error)
}

// ExecutorFunc adapts a function to Executor.
type ExecutorFunc func(ctx context.Context, name string, input json.RawMessage) (string, bool, error)

// Execute implements Executor.
func (f ExecutorFunc) Execute(ctx context.Context, name string, input json.RawMessage) (string, bool, error) {
	return f(ctx, name, input)
}

// Nudger is an optional Executor extension. After the model stops calling
// tools, Nudge is asked whether the run is really complete. A non-empty
// return is sent back as a user message and the loop continues; empty ends
// the run. calls counts successful (non-error) tool invocations by name so
// far. attempt starts at 1.
type Nudger interface {
	Nudge(res Result, calls map[string]int, attempt int) string
}

// Result summarises a completed run.
type Result struct {
	FinalText  string
	StopReason string
	Turns      int
	ToolCalls  int
	// Calls counts successful (non-error) tool invocations by name.
	Calls        map[string]int
	InputTokens  int64
	OutputTokens int64
	Duration     time.Duration
}

// ErrMaxTurns is returned when the loop hits llm.max_turns.
var ErrMaxTurns = errors.New("llm: max turns exceeded")

// ErrRefused is returned when the model declined the request.
var ErrRefused = errors.New("llm: request refused")

// Runner executes agent loops.
type Runner struct {
	cfg  config.LLM
	http *http.Client
	log  *slog.Logger
}

// New builds a Runner from config.
func New(cfg config.LLM, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{cfg: cfg, http: &http.Client{Timeout: cfg.RequestTimeout}, log: log}
}

// --- wire types (OpenAI Chat Completions) ---

type message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type request struct {
	Model           string           `json:"model"`
	Messages        []message        `json:"messages"`
	Tools           []map[string]any `json:"tools,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	extra           map[string]any
}

func (r request) MarshalJSON() ([]byte, error) {
	type plain request
	raw, err := json.Marshal(plain(r))
	if err != nil {
		return nil, err
	}
	if len(r.extra) == 0 {
		return raw, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k, v := range r.extra {
		m[k] = v
	}
	return json.Marshal(m)
}

type response struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			message
			Refusal string `json:"refusal,omitempty"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

// Run drives the loop until the model stops calling tools.
func (r *Runner) Run(ctx context.Context, system, prompt string, tools []ToolDef, exec Executor) (Result, error) {
	start := time.Now()
	res := Result{}
	finish := func(err error) (Result, error) {
		res.Duration = time.Since(start)
		return res, err
	}
	history := []message{
		{Role: "system", Content: system},
		{Role: "user", Content: prompt},
	}
	toolSpecs := toWire(tools)
	res.Calls = map[string]int{}
	nudger, _ := exec.(Nudger)
	nudges := 0

	for turn := 1; turn <= r.cfg.MaxTurns; turn++ {
		res.Turns = turn
		resp, err := r.complete(ctx, history, toolSpecs)
		if err != nil {
			return finish(fmt.Errorf("llm turn %d: %w", turn, err))
		}
		res.InputTokens += resp.Usage.PromptTokens
		res.OutputTokens += resp.Usage.CompletionTokens
		if len(resp.Choices) == 0 {
			return finish(errors.New("llm: response has no choices"))
		}
		choice := resp.Choices[0]
		res.StopReason = choice.FinishReason
		if txt := strings.TrimSpace(choice.Message.Content); txt != "" {
			res.FinalText = txt
		}
		if choice.Message.Refusal != "" || choice.FinishReason == "content_filter" {
			return finish(fmt.Errorf("%w: %s", ErrRefused, firstNonEmpty(choice.Message.Refusal, choice.FinishReason)))
		}

		assistant := choice.Message.message
		assistant.Role = "assistant"
		history = append(history, assistant)
		calls := assistant.ToolCalls

		if len(calls) == 0 {
			if choice.FinishReason == "length" {
				r.log.Warn("llm hit output limit, continuing", "turn", turn)
				history = append(history, message{Role: "user", Content: "Your previous message was cut off by the output limit. Continue from where you stopped."})
				continue
			}
			if nudger != nil {
				nudges++
				if msg := nudger.Nudge(res, res.Calls, nudges); msg != "" {
					r.log.Info("nudging model", "attempt", nudges)
					history = append(history, message{Role: "user", Content: msg})
					continue
				}
			}
			return finish(nil)
		}

		results, okCounts := r.executeAll(ctx, calls, exec)
		if ctx.Err() != nil {
			return finish(ctx.Err())
		}
		res.ToolCalls += len(calls)
		for name, n := range okCounts {
			res.Calls[name] += n
		}
		history = append(history, results...)
	}
	return finish(ErrMaxTurns)
}

func (r *Runner) complete(ctx context.Context, history []message, tools []map[string]any) (*response, error) {
	req := request{Model: r.cfg.Model, Messages: history, Tools: tools, ReasoningEffort: r.cfg.Effort}
	if r.cfg.MaxTokens > 0 {
		req.extra = map[string]any{firstNonEmpty(r.cfg.MaxTokensParam, "max_completion_tokens"): r.cfg.MaxTokens}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(r.cfg.BaseURL, "/") + "/chat/completions"

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1))*time.Second + time.Duration(rand.Intn(500))*time.Millisecond
			r.log.Warn("llm request retry", "attempt", attempt, "delay", delay, "err", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		resp, retry, err := r.doOnce(ctx, url, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retry || ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// StatusError is a non-2xx response from the provider.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("llm: HTTP %d: %s", e.Code, e.Body) }

func (r *Runner) doOnce(ctx context.Context, url string, body []byte) (resp *response, retry bool, err error) {
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if r.cfg.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)
	}
	for k, v := range r.cfg.Headers {
		hr.Header.Set(k, v)
	}
	res, err := r.http.Do(hr)
	if err != nil {
		return nil, true, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, true, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		retry = res.StatusCode == 408 || res.StatusCode == 409 || res.StatusCode == 429 || res.StatusCode >= 500
		return nil, retry, &StatusError{Code: res.StatusCode, Body: truncate(string(raw), 512)}
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false, fmt.Errorf("decode response: %w", err)
	}
	if out.Error != nil {
		return nil, false, fmt.Errorf("llm: %s: %s", out.Error.Type, out.Error.Message)
	}
	return &out, false, nil
}

func toWire(tools []ToolDef) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if _, ok := schema["type"]; !ok {
			schema["type"] = "object"
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  schema,
			},
		})
	}
	return out
}

// executeAll runs every call concurrently and returns one tool message per
// call, in the original order, plus a count of successful calls by name.
func (r *Runner) executeAll(ctx context.Context, calls []toolCall, exec Executor) ([]message, map[string]int) {
	outs := make([]message, len(calls))
	okFlags := make([]bool, len(calls))
	var wg sync.WaitGroup
	for i, c := range calls {
		wg.Add(1)
		go func(i int, c toolCall) {
			defer wg.Done()
			args := strings.TrimSpace(c.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			t0 := time.Now()
			content, isErr, err := exec.Execute(ctx, c.Function.Name, json.RawMessage(args))
			switch {
			case err != nil:
				content = "tool execution error: " + err.Error()
				isErr = true
			case isErr:
				content = "tool error: " + content
			}
			if content == "" {
				content = "(empty result)"
			}
			if isErr {
				r.log.Warn("tool call failed", "tool", c.Function.Name, "input", truncate(args, 300), "result", truncate(content, 300), "took", time.Since(t0))
			} else {
				r.log.Debug("tool call", "tool", c.Function.Name, "input", truncate(args, 200), "took", time.Since(t0))
			}
			okFlags[i] = !isErr
			outs[i] = message{Role: "tool", ToolCallID: c.ID, Content: content}
		}(i, c)
	}
	wg.Wait()
	ok := map[string]int{}
	for i, c := range calls {
		if okFlags[i] {
			ok[c.Function.Name]++
		}
	}
	return outs, ok
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
