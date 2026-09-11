// Package mcp aggregates tools from one or more MCP servers behind a single
// namespace so the agent sees one flat tool list.
//
// Tool names are exposed as "<server>__<tool>" to avoid collisions between
// servers. One session per server is shared by every concurrent worker; the
// go-sdk client session multiplexes JSON-RPC requests safely.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wys1203/go-ai-orchestration/internal/config"
)

// Separator joins server and tool names.
const Separator = "__"

// MaxToolNameLen is the Anthropic API limit for tool names.
const MaxToolNameLen = 64

// Tool is an MCP tool re-exposed under a namespaced name.
type Tool struct {
	Name        string
	Server      string
	Original    string
	Description string
	InputSchema map[string]any
}

// Hub holds live sessions and the merged tool catalogue.
type Hub struct {
	log      *slog.Logger
	sessions map[string]*sdk.ClientSession
	tools    []Tool
	byName   map[string]Tool
	mu       sync.RWMutex
}

// Connect opens every configured server and lists its tools. A server that
// fails to connect aborts the whole hub so misconfiguration is loud.
func Connect(ctx context.Context, servers []config.MCPServer, log *slog.Logger) (*Hub, error) {
	if log == nil {
		log = slog.Default()
	}
	h := newHub(log)
	for _, srv := range servers {
		transport, err := newTransport(srv)
		if err != nil {
			h.Close()
			return nil, fmt.Errorf("mcp %s: %w", srv.Name, err)
		}
		if err := h.add(ctx, srv, transport); err != nil {
			h.Close()
			return nil, err
		}
	}
	h.sortTools()
	return h, nil
}

func newHub(log *slog.Logger) *Hub {
	return &Hub{log: log, sessions: map[string]*sdk.ClientSession{}, byName: map[string]Tool{}}
}

// add connects one server over transport and merges its tools.
func (h *Hub) add(ctx context.Context, srv config.MCPServer, transport sdk.Transport) error {
	client := sdk.NewClient(&sdk.Implementation{Name: "gao", Version: "0.1.0"}, nil)
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("mcp %s: connect: %w", srv.Name, err)
	}
	h.mu.Lock()
	h.sessions[srv.Name] = sess
	h.mu.Unlock()
	if err := h.loadTools(ctx, srv, sess); err != nil {
		return fmt.Errorf("mcp %s: list tools: %w", srv.Name, err)
	}
	return nil
}

func (h *Hub) sortTools() {
	h.mu.Lock()
	defer h.mu.Unlock()
	sort.Slice(h.tools, func(i, j int) bool { return h.tools[i].Name < h.tools[j].Name })
}

func newTransport(srv config.MCPServer) (sdk.Transport, error) {
	switch {
	case srv.Command != "":
		cmd := exec.Command(srv.Command, srv.Args...)
		cmd.Env = os.Environ()
		for k, v := range srv.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stderr = os.Stderr
		return &sdk.CommandTransport{Command: cmd}, nil
	case srv.URL != "":
		hc := &http.Client{Transport: &headerTransport{base: http.DefaultTransport, headers: srv.Headers}}
		return &sdk.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: hc}, nil
	default:
		return nil, errors.New("neither command nor url set")
	}
}

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.base.RoundTrip(r)
}

func (h *Hub) loadTools(ctx context.Context, srv config.MCPServer, sess *sdk.ClientSession) error {
	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	allow := toSet(srv.AllowTools)
	deny := toSet(srv.DenyTools)
	for _, t := range res.Tools {
		if len(allow) > 0 && !allow[t.Name] {
			continue
		}
		if deny[t.Name] {
			continue
		}
		schema, err := toMap(t.InputSchema)
		if err != nil {
			return fmt.Errorf("tool %s schema: %w", t.Name, err)
		}
		name := QualifiedName(srv.Name, t.Name)
		if _, dup := h.byName[name]; dup {
			h.log.Warn("duplicate tool name after namespacing, skipping", "tool", name)
			continue
		}
		tool := Tool{Name: name, Server: srv.Name, Original: t.Name, Description: t.Description, InputSchema: schema}
		h.tools = append(h.tools, tool)
		h.byName[name] = tool
	}
	h.log.Info("mcp server ready", "server", srv.Name, "tools", len(res.Tools))
	return nil
}

// QualifiedName builds the namespaced tool name, clamped to the API limit.
func QualifiedName(server, tool string) string {
	name := sanitize(server) + Separator + sanitize(tool)
	if len(name) > MaxToolNameLen {
		name = name[:MaxToolNameLen]
	}
	return name
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func toSet(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func toMap(v any) (map[string]any, error) {
	if v == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Tools returns the merged catalogue sorted by name.
func (h *Hub) Tools() []Tool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]Tool(nil), h.tools...)
}

// Has reports whether name is a hub tool.
func (h *Hub) Has(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.byName[name]
	return ok
}

// Call invokes a namespaced tool and flattens the result to text.
// isError mirrors the MCP result flag; err is a transport-level failure.
func (h *Hub) Call(ctx context.Context, name string, args json.RawMessage) (text string, isError bool, err error) {
	h.mu.RLock()
	t, ok := h.byName[name]
	sess := h.sessions[t.Server]
	h.mu.RUnlock()
	if !ok || sess == nil {
		return "", true, fmt.Errorf("unknown tool %q", name)
	}
	var arguments map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return "", true, fmt.Errorf("tool %s: arguments are not an object: %w", name, err)
		}
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	res, err := sess.CallTool(ctx, &sdk.CallToolParams{Name: t.Original, Arguments: arguments})
	if err != nil {
		return "", true, err
	}
	return Flatten(res), res.IsError, nil
}

// Flatten renders tool result content as a single string.
func Flatten(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch v := c.(type) {
		case *sdk.TextContent:
			parts = append(parts, v.Text)
		case *sdk.ImageContent:
			parts = append(parts, fmt.Sprintf("[image %s, %d bytes]", v.MIMEType, len(v.Data)))
		case *sdk.EmbeddedResource:
			if v.Resource != nil && v.Resource.Text != "" {
				parts = append(parts, v.Resource.Text)
			} else if v.Resource != nil {
				parts = append(parts, fmt.Sprintf("[resource %s]", v.Resource.URI))
			}
		default:
			if raw, err := json.Marshal(c); err == nil {
				parts = append(parts, string(raw))
			}
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(raw))
		}
	}
	return strings.Join(parts, "\n")
}

// Close terminates every session.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, s := range h.sessions {
		if err := s.Close(); err != nil {
			h.log.Warn("mcp close", "server", name, "err", err)
		}
		delete(h.sessions, name)
	}
}
