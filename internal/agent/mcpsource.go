package agent

import (
	"context"
	"encoding/json"

	"github.com/wys1203/go-ai-orchestration/internal/mcp"
)

// HubSource adapts *mcp.Hub to ToolSource.
type HubSource struct{ Hub *mcp.Hub }

// Tools implements ToolSource.
func (h HubSource) Tools() []ToolSpec {
	var out []ToolSpec
	for _, t := range h.Hub.Tools() {
		out = append(out, ToolSpec{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

// Call implements ToolSource.
func (h HubSource) Call(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	return h.Hub.Call(ctx, name, args)
}
