// Package agent wires skills, MCP tools and the LLM runner into a single
// "handle this issue" operation.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/wys1203/go-ai-orchestration/internal/config"
	"github.com/wys1203/go-ai-orchestration/internal/github"
	"github.com/wys1203/go-ai-orchestration/internal/llm"
	"github.com/wys1203/go-ai-orchestration/internal/skill"
)

// LoadSkillTool is the built-in tool the model uses to read a skill body.
const LoadSkillTool = "load_skill"

// ToolSource exposes tools to the agent; the MCP hub implements it.
type ToolSource interface {
	Tools() []ToolSpec
	Call(ctx context.Context, name string, args json.RawMessage) (text string, isError bool, err error)
}

// ToolSpec mirrors llm.ToolDef but keeps the agent package decoupled.
type ToolSpec struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// Runner is the piece of llm.Runner the agent depends on.
type Runner interface {
	Run(ctx context.Context, system, prompt string, tools []llm.ToolDef, exec llm.Executor) (llm.Result, error)
}

// Agent handles issues.
type Agent struct {
	cfg    config.Agent
	runner Runner
	skills *skill.Registry
	tools  ToolSource
	log    *slog.Logger
	system string
}

// New builds an Agent. tools may be nil when no MCP server is configured.
func New(cfg config.Agent, runner Runner, skills *skill.Registry, tools ToolSource, log *slog.Logger) (*Agent, error) {
	if log == nil {
		log = slog.Default()
	}
	base := defaultSystemPrompt
	if cfg.SystemPromptFile != "" {
		raw, err := os.ReadFile(cfg.SystemPromptFile)
		if err != nil {
			return nil, fmt.Errorf("system prompt: %w", err)
		}
		base = string(raw)
	}
	return &Agent{cfg: cfg, runner: runner, skills: skills, tools: tools, log: log, system: base}, nil
}

// ErrRequiredTools is returned when the run ended without a successful call
// to every configured required tool.
var ErrRequiredTools = errors.New("agent: required tools were not called successfully")

// HandleIssue runs the agent loop for one issue. The run counts as failed
// when a required tool was never called successfully, even if the model
// reported completion.
func (a *Agent) HandleIssue(ctx context.Context, is github.Issue) (llm.Result, error) {
	res, err := a.runner.Run(ctx, a.systemPrompt(), a.issuePrompt(is), a.toolDefs(), a.executorFor(is))
	if err != nil {
		return res, err
	}
	if missing := a.missingRequired(res.Calls); len(missing) > 0 {
		return res, fmt.Errorf("%w: %s (model said: %s)", ErrRequiredTools, strings.Join(missing, ", "), truncate(res.FinalText, 300))
	}
	return res, nil
}

func (a *Agent) missingRequired(calls map[string]int) []string {
	var missing []string
	for _, t := range a.cfg.RequiredTools {
		if calls[t] == 0 {
			missing = append(missing, t)
		}
	}
	return missing
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Ask runs the agent loop on an arbitrary prompt with the same system
// prompt, skills and tools as HandleIssue. Used by `gao ask` for dry runs.
// Required-tool nudges are not applied.
func (a *Agent) Ask(ctx context.Context, prompt string) (llm.Result, error) {
	return a.runner.Run(ctx, a.systemPrompt(), prompt, a.toolDefs(), llm.ExecutorFunc(a.execute))
}

// executor bundles tool execution with the repository/issue guard and the
// required-tools nudge.
type executor struct {
	a      *Agent
	repo   string // owner/name of the issue being processed
	number int
}

func (a *Agent) executorFor(is github.Issue) llm.Executor {
	return executor{a: a, repo: is.Repo, number: is.Number}
}

func (e executor) Execute(ctx context.Context, name string, input json.RawMessage) (string, bool, error) {
	if msg := e.guard(name, input); msg != "" {
		e.a.log.Warn("tool call rejected by guard", "tool", name, "reason", msg)
		return msg, true, nil
	}
	return e.a.execute(ctx, name, input)
}

// guard returns a rejection message when a tool call targets a different
// repository, or when an issue-scoped write tool targets a different issue.
func (e executor) guard(name string, input json.RawMessage) string {
	if name == LoadSkillTool || e.repo == "" {
		return ""
	}
	var args struct {
		Owner       string `json:"owner"`
		Repo        string `json:"repo"`
		IssueNumber int    `json:"issue_number"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}
	owner, repo, _ := strings.Cut(e.repo, "/")
	if !e.a.cfg.AllowOtherRepos && (args.Owner != "" || args.Repo != "") {
		// Models sometimes pass repo as "owner/name"; accept that spelling.
		if o, r, ok := strings.Cut(args.Repo, "/"); ok && (args.Owner == "" || strings.EqualFold(args.Owner, o)) {
			args.Owner, args.Repo = o, r
		}
		if !strings.EqualFold(args.Owner, owner) || !strings.EqualFold(args.Repo, repo) {
			return fmt.Sprintf("rejected: this run is restricted to repository %s, but the call targeted %s/%s. Use owner=%q repo=%q.",
				e.repo, args.Owner, args.Repo, owner, repo)
		}
	}
	for _, scoped := range e.a.cfg.IssueScopedTools {
		if scoped == name && args.IssueNumber != 0 && args.IssueNumber != e.number {
			return fmt.Sprintf("rejected: %s may only target issue #%d in this run, but the call targeted #%d.", name, e.number, args.IssueNumber)
		}
	}
	return ""
}

// Nudge implements llm.Nudger: remind the model of required tools it has
// not called yet, up to MaxNudges times.
func (e executor) Nudge(_ llm.Result, calls map[string]int, attempt int) string {
	if attempt > e.a.cfg.MaxNudges {
		return ""
	}
	missing := e.a.missingRequired(calls)
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("You stopped before finishing. There is no human here to answer questions. "+
		"You have not yet successfully called the required tool(s): %s. If an earlier call failed, read the error message, fix the arguments and call it again. "+
		"Do the remaining work now using tools, then reply with your final summary.",
		strings.Join(missing, ", "))
}

// SystemPrompt returns the rendered system prompt (for inspection).
func (a *Agent) SystemPrompt() string { return a.systemPrompt() }

func (a *Agent) systemPrompt() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(a.system))
	b.WriteString("\n\n# Skills\n\n")
	b.WriteString("Skills are reusable playbooks. Only their names and descriptions are listed here. ")
	b.WriteString("Before acting on an issue, call `" + LoadSkillTool + "` for every skill whose description matches the task and follow its instructions. ")
	b.WriteString("Skills whose body is already included in the task message do not need to be loaded again.\n\n")
	b.WriteString(a.skills.Index())
	if a.tools != nil {
		b.WriteString("\n# Tools\n\n")
		b.WriteString("Tool names are prefixed with the MCP server name and `__`. Prefer read-only tools first; make changes only when the skill or the issue calls for them. ")
		b.WriteString("An empty search result is information, not a dead end: read the issue and the repository files directly instead of stopping.\n")
	}
	if len(a.cfg.RequiredTools) > 0 {
		b.WriteString("\n# Required actions\n\nBefore you finish you must call each of these tools at least once: ")
		b.WriteString(strings.Join(a.cfg.RequiredTools, ", "))
		b.WriteString(".\n")
	}
	return b.String()
}

func (a *Agent) toolDefs() []llm.ToolDef {
	defs := []llm.ToolDef{{
		Name:        LoadSkillTool,
		Description: "Load the full instructions of a skill by name. Returns the skill's markdown body.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Skill name exactly as listed in the Skills section."},
			},
			"required":             []string{"name"},
			"additionalProperties": false,
		},
	}}
	if a.tools != nil {
		for _, t := range a.tools.Tools() {
			defs = append(defs, llm.ToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		}
	}
	return defs
}

func (a *Agent) execute(ctx context.Context, name string, input json.RawMessage) (string, bool, error) {
	if name == LoadSkillTool {
		var in struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "invalid input: " + err.Error(), true, nil
		}
		s, ok := a.skills.Get(in.Name)
		if !ok {
			return fmt.Sprintf("unknown skill %q; available: %s", in.Name, strings.Join(a.skillNames(), ", ")), true, nil
		}
		return s.Body, false, nil
	}
	if a.tools == nil {
		return "no tool source configured", true, nil
	}
	return a.tools.Call(ctx, name, input)
}

func (a *Agent) skillNames() []string {
	var names []string
	for _, s := range a.skills.List() {
		names = append(names, s.Name)
	}
	return names
}

func (a *Agent) issuePrompt(is github.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Handle the following GitHub issue.\n\n")
	fmt.Fprintf(&b, "Repository: %s\nIssue: #%d\nURL: %s\nTitle: %s\nAuthor: %s\nState: %s\nLabels: %s\nCreated: %s\nUpdated: %s\nComments: %d\n\n",
		is.Repo, is.Number, is.HTMLURL, is.Title, is.User.Login, is.State,
		strings.Join(is.LabelNames(), ", "), is.CreatedAt.Format(time.RFC3339), is.UpdatedAt.Format(time.RFC3339), is.Comments)
	b.WriteString("## Issue body\n\n")
	body := strings.TrimSpace(is.Body)
	if body == "" {
		body = "(empty)"
	}
	b.WriteString(body)
	b.WriteString("\n\n")
	if matched := a.skills.Matching(is.LabelNames()); len(matched) > 0 {
		b.WriteString("## Skills attached because of the issue labels\n\n")
		for _, s := range matched {
			fmt.Fprintf(&b, "### %s\n\n%s\n\n", s.Name, s.Body)
		}
	}
	b.WriteString("Work through the steps with tools until the issue has been handled. When you are finished, reply with a short summary of what you did and what, if anything, needs a human.")
	return b.String()
}

const defaultSystemPrompt = `You are an autonomous, headless software maintenance agent operating on GitHub issues without human supervision.

This is not a chat. Nobody will read your replies or answer questions; your final message is only written to a log. Everything you want a person to see must be posted to the issue with a tool. Never end with a question or an offer of options.

Operating rules:
- Work only on the issue you were given. Read the issue and its comments before acting.
- Use tools to gather facts; do not guess about repository contents or history.
- Be conservative with write actions: comment, label, and open pull requests when appropriate, but never close issues, delete branches, or force-push unless a skill explicitly instructs you to.
- Never post secrets, tokens, or private data in comments.
- If the issue is unclear, ask a concise clarifying question in a comment instead of guessing.
- When a task cannot be completed, say so plainly in your final summary.
- Keep comments short and useful for the humans who will read them.`
