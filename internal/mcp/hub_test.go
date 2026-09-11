package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wys1203/go-ai-orchestration/internal/config"
)

func TestQualifiedName(t *testing.T) {
	if got := QualifiedName("github", "list issues"); got != "github__list_issues" {
		t.Fatalf("got %q", got)
	}
	long := QualifiedName("srv", strings.Repeat("x", 100))
	if len(long) != MaxToolNameLen {
		t.Fatalf("len %d", len(long))
	}
}

func TestFlatten(t *testing.T) {
	res := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "a"}, &sdk.TextContent{Text: "b"}}}
	if got := Flatten(res); got != "a\nb" {
		t.Fatalf("got %q", got)
	}
	res = &sdk.CallToolResult{StructuredContent: map[string]any{"k": 1}}
	if got := Flatten(res); got != `{"k":1}` {
		t.Fatalf("got %q", got)
	}
}

func TestToMap(t *testing.T) {
	m, err := toMap(nil)
	if err != nil || m["type"] != "object" {
		t.Fatalf("%v %v", m, err)
	}
	type schema struct {
		Type string `json:"type"`
	}
	m, err = toMap(schema{Type: "object"})
	if err != nil || m["type"] != "object" {
		t.Fatalf("%v %v", m, err)
	}
}

// TestHubInMemory runs a real go-sdk server in-process and drives it
// through the Hub, covering namespacing, allow/deny filters and Call.
func TestHubInMemory(t *testing.T) {
	ctx := context.Background()
	server := sdk.NewServer(&sdk.Implementation{Name: "fake", Version: "0"}, nil)
	type in struct {
		Name string `json:"name" jsonschema:"who to greet"`
	}
	sdk.AddTool(server, &sdk.Tool{Name: "greet", Description: "say hi"}, func(_ context.Context, _ *sdk.CallToolRequest, args in) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "hi " + args.Name}}}, nil, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "fail", Description: "always errors"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "boom"}}}, nil, nil
	})
	sdk.AddTool(server, &sdk.Tool{Name: "hidden", Description: "denied"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{}, nil, nil
	})
	ct, st := sdk.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, st) }()

	h := newHub(nil)
	h.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := h.add(ctx, config.MCPServer{Name: "gh", DenyTools: []string{"hidden"}}, ct); err != nil {
		t.Fatal(err)
	}
	h.sortTools()
	defer h.Close()

	tools := h.Tools()
	if len(tools) != 2 || tools[0].Name != "gh__fail" || tools[1].Name != "gh__greet" {
		t.Fatalf("tools: %+v", tools)
	}
	if _, ok := tools[1].InputSchema["properties"]; !ok {
		t.Fatalf("schema not converted: %v", tools[1].InputSchema)
	}
	out, isErr, err := h.Call(ctx, "gh__greet", json.RawMessage(`{"name":"bob"}`))
	if err != nil || isErr || out != "hi bob" {
		t.Fatalf("greet: %q %v %v", out, isErr, err)
	}
	out, isErr, err = h.Call(ctx, "gh__fail", nil)
	if err != nil || !isErr || out != "boom" {
		t.Fatalf("fail: %q %v %v", out, isErr, err)
	}
	if _, _, err := h.Call(ctx, "gh__hidden", nil); err == nil {
		t.Fatal("denied tool should be unknown")
	}
	if _, _, err := h.Call(ctx, "gh__greet", json.RawMessage(`[1]`)); err == nil {
		t.Fatal("non-object args should error")
	}
}
