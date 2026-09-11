package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseExpandsEnv(t *testing.T) {
	t.Setenv("GAO_TEST_TOKEN", "sekret")
	t.Setenv("GAO_TEST_UNSET", "")
	cfg := Default()
	raw := []byte(`
llm:
  base_url: ${GAO_TEST_UNSET:-http://fallback/v1}
  model: ${GAO_TEST_TOKEN:-ignored}
github:
  token: ${GAO_TEST_TOKEN}
  repos: [a/b]
  poll_interval: 5s
mcp:
  servers:
    - name: gh
      url: https://example.com/mcp
      headers: {Authorization: "Bearer ${GAO_TEST_TOKEN}"}
agent:
  max_concurrent: 2
`)
	if err := Parse(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.Token != "sekret" || cfg.MCP.Servers[0].Headers["Authorization"] != "Bearer sekret" {
		t.Fatalf("env not expanded: %+v", cfg.GitHub)
	}
	if cfg.LLM.BaseURL != "http://fallback/v1" || cfg.LLM.Model != "sekret" {
		t.Fatalf("default expansion: %+v", cfg.LLM)
	}
	if cfg.GitHub.PollInterval != 5*time.Second || cfg.Agent.MaxConcurrent != 2 {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
	if cfg.LLM.MaxTokensParam != "max_completion_tokens" {
		t.Fatalf("default lost: %+v", cfg.LLM)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	cfg := Default()
	cfg.GitHub.Repos = []string{"bad"}
	cfg.GitHub.TriggerLabel = ""
	cfg.LLM.MaxTokensParam = "tokens"
	cfg.MCP.Servers = []MCPServer{{Name: "x", Command: "a", URL: "b"}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"owner/name", "trigger_label", "max_tokens_param", "exactly one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
