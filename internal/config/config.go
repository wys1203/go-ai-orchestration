// Package config loads the YAML configuration for gao.
//
// Every string value in the file is passed through os.ExpandEnv before
// parsing, so secrets can be referenced as ${GITHUB_TOKEN} instead of being
// written into the file.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration.
type Config struct {
	LLM    LLM    `yaml:"llm"`
	GitHub GitHub `yaml:"github"`
	MCP    MCP    `yaml:"mcp"`
	Skills Skills `yaml:"skills"`
	Agent  Agent  `yaml:"agent"`
}

// LLM configures the OpenAI-compatible endpoint used by the agent.
type LLM struct {
	// Model is the model id sent in the request, e.g. "gpt-5" or "qwen3:32b".
	Model string `yaml:"model"`
	// BaseURL is the API root, e.g. https://api.openai.com/v1 or
	// http://localhost:11434/v1. "/chat/completions" is appended.
	BaseURL string `yaml:"base_url"`
	// APIKey is sent as a Bearer token. Empty sends no Authorization header.
	APIKey string `yaml:"api_key"`
	// Headers are extra request headers (e.g. OpenRouter attribution).
	Headers map[string]string `yaml:"headers"`
	// MaxTokens caps the output tokens per request; 0 omits the field.
	MaxTokens int64 `yaml:"max_tokens"`
	// MaxTokensParam is the wire name for MaxTokens: "max_completion_tokens"
	// (OpenAI, default) or "max_tokens" (older servers, some local runtimes).
	MaxTokensParam string `yaml:"max_tokens_param"`
	// Effort is sent as reasoning_effort when non-empty (e.g. low|medium|high).
	Effort string `yaml:"effort"`
	// MaxTurns bounds the number of model round-trips per issue.
	MaxTurns int `yaml:"max_turns"`
	// RequestTimeout is the per-request HTTP timeout.
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// GitHub configures the issue watcher.
type GitHub struct {
	Token   string `yaml:"token"`
	APIBase string `yaml:"api_base"`
	// Repos is a list of "owner/name".
	Repos        []string      `yaml:"repos"`
	PollInterval time.Duration `yaml:"poll_interval"`
	// TriggerLabel selects which issues are picked up. Empty with ProcessAll
	// false means nothing is processed.
	TriggerLabel string `yaml:"trigger_label"`
	// ProcessAll picks up every open issue regardless of TriggerLabel.
	ProcessAll bool `yaml:"process_all"`
	// Labels applied by the orchestrator to track lifecycle.
	InProgressLabel string `yaml:"in_progress_label"`
	DoneLabel       string `yaml:"done_label"`
	FailedLabel     string `yaml:"failed_label"`
	// CommentOnFailure posts a short comment when the agent fails.
	CommentOnFailure bool `yaml:"comment_on_failure"`
}

// MCP lists the MCP servers whose tools are exposed to the agent.
type MCP struct {
	Servers []MCPServer `yaml:"servers"`
}

// MCPServer is either a stdio subprocess (Command set) or a streamable HTTP
// endpoint (URL set).
type MCPServer struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	// AllowTools restricts the exposed tools to this list (original names).
	AllowTools []string `yaml:"allow_tools"`
	// DenyTools removes these tools (original names).
	DenyTools []string `yaml:"deny_tools"`
}

// Skills configures the SKILL.md loader.
type Skills struct {
	Dir string `yaml:"dir"`
}

// Agent configures the orchestrator.
type Agent struct {
	Name             string        `yaml:"name"`
	MaxConcurrent    int           `yaml:"max_concurrent"`
	IssueTimeout     time.Duration `yaml:"issue_timeout"`
	StateFile        string        `yaml:"state_file"`
	SystemPromptFile string        `yaml:"system_prompt_file"`
	// RequiredTools lists tool names the model must call at least once per
	// issue (e.g. github__add_issue_comment). If the model stops without
	// calling them it is reminded up to MaxNudges times.
	RequiredTools []string `yaml:"required_tools"`
	MaxNudges     int      `yaml:"max_nudges"`
	// AllowOtherRepos lifts the guard that rejects any tool call whose
	// owner/repo arguments differ from the issue's repository. Off by default:
	// models copy example arguments from tool descriptions.
	AllowOtherRepos bool `yaml:"allow_other_repos"`
	// IssueScopedTools are write tools whose issue_number argument must equal
	// the issue being processed.
	IssueScopedTools []string `yaml:"issue_scoped_tools"`
}

// Default returns a configuration with sensible defaults applied.
func Default() Config {
	return Config{
		LLM: LLM{
			Model:          "gpt-5",
			BaseURL:        firstNonEmpty(os.Getenv("OPENAI_BASE_URL"), "https://api.openai.com/v1"),
			APIKey:         os.Getenv("OPENAI_API_KEY"),
			MaxTokens:      16000,
			MaxTokensParam: "max_completion_tokens",
			Effort:         "",
			MaxTurns:       40,
			RequestTimeout: 10 * time.Minute,
		},
		GitHub: GitHub{
			Token:            os.Getenv("GITHUB_TOKEN"),
			APIBase:          "https://api.github.com",
			PollInterval:     60 * time.Second,
			TriggerLabel:     "ai",
			InProgressLabel:  "ai:working",
			DoneLabel:        "ai:done",
			FailedLabel:      "ai:failed",
			CommentOnFailure: true,
		},
		Skills: Skills{Dir: "./skills"},
		Agent: Agent{
			Name:          "gao",
			MaxConcurrent: 4,
			IssueTimeout:  30 * time.Minute,
			StateFile:     "./gao-state.json",
			MaxNudges:     2,
			IssueScopedTools: []string{
				"github__add_issue_comment", "github__issue_write", "github__sub_issue_write",
			},
		},
	}
}

// Load reads path, expands environment variables and applies defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := Parse(raw, &cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

// Parse decodes YAML bytes into cfg after environment expansion. Both
// ${VAR} and ${VAR:-default} are supported.
func Parse(raw []byte, cfg *Config) error {
	expanded := os.Expand(string(raw), func(key string) string {
		name, def, hasDef := strings.Cut(key, ":-")
		if v := os.Getenv(name); v != "" || !hasDef {
			return v
		}
		return def
	})
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	return nil
}

// Validate checks required fields.
func (c Config) Validate() error {
	var errs []error
	if c.LLM.Model == "" {
		errs = append(errs, errors.New("llm.model is required"))
	}
	if c.LLM.MaxTurns <= 0 {
		errs = append(errs, errors.New("llm.max_turns must be > 0"))
	}
	if c.LLM.BaseURL == "" {
		errs = append(errs, errors.New("llm.base_url is required"))
	}
	switch c.LLM.MaxTokensParam {
	case "", "max_tokens", "max_completion_tokens":
	default:
		errs = append(errs, fmt.Errorf("llm.max_tokens_param %q must be max_tokens or max_completion_tokens", c.LLM.MaxTokensParam))
	}
	for _, r := range c.GitHub.Repos {
		if strings.Count(r, "/") != 1 {
			errs = append(errs, fmt.Errorf("github.repos entry %q must be owner/name", r))
		}
	}
	if c.GitHub.TriggerLabel == "" && !c.GitHub.ProcessAll {
		errs = append(errs, errors.New("github.trigger_label is empty and process_all is false: nothing would be processed"))
	}
	if c.Agent.MaxConcurrent <= 0 {
		errs = append(errs, errors.New("agent.max_concurrent must be > 0"))
	}
	for i, s := range c.MCP.Servers {
		if s.Name == "" {
			errs = append(errs, fmt.Errorf("mcp.servers[%d].name is required", i))
		}
		if (s.Command == "") == (s.URL == "") {
			errs = append(errs, fmt.Errorf("mcp.servers[%d] must set exactly one of command or url", i))
		}
	}
	return errors.Join(errs...)
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
