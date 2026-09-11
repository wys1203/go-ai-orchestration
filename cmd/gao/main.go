// Command gao is a headless AI agent that watches GitHub issues and acts on
// them with an OpenAI-compatible LLM, Agent Skills and MCP tools.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/wys1203/go-ai-orchestration/internal/agent"
	"github.com/wys1203/go-ai-orchestration/internal/config"
	"github.com/wys1203/go-ai-orchestration/internal/github"
	"github.com/wys1203/go-ai-orchestration/internal/llm"
	"github.com/wys1203/go-ai-orchestration/internal/mcp"
	"github.com/wys1203/go-ai-orchestration/internal/orchestrator"
	"github.com/wys1203/go-ai-orchestration/internal/skill"
	"github.com/wys1203/go-ai-orchestration/internal/state"
)

var version = "dev"

const usage = `gao - headless GitHub issue agent

Usage:
  gao run    [-config FILE]                      watch configured repos forever
  gao once   [-config FILE] -repo OWNER/NAME -issue N   process one issue and exit
  gao poll   [-config FILE]                      poll each repo once, run what is due, exit
  gao ask    [-config FILE] -prompt TEXT          run the agent loop on an ad-hoc prompt (skills + MCP tools, no GitHub labels)
  gao skills [-config FILE]                      list loaded skills
  gao tools  [-config FILE]                      connect MCP servers and list tools
  gao prompt [-config FILE]                      print the rendered system prompt
  gao version

Environment:
  OPENAI_API_KEY      API key for the OpenAI-compatible endpoint (llm.api_key)
  OPENAI_BASE_URL     API root, default https://api.openai.com/v1 (llm.base_url)
  GITHUB_TOKEN        GitHub token used for polling and labels
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if cmd == "version" {
		fmt.Println("gao", version)
		return
	}
	if err := run(cmd, args); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", envOr("GAO_CONFIG", "config.yaml"), "config file")
	logFmt := fs.String("log-format", envOr("GAO_LOG_FORMAT", "text"), "text|json")
	logLevel := fs.String("log-level", envOr("GAO_LOG_LEVEL", "info"), "debug|info|warn|error")
	repo := fs.String("repo", "", "owner/name (once)")
	issue := fs.Int("issue", 0, "issue number (once)")
	prompt := fs.String("prompt", "", "prompt text (ask)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger(*logFmt, *logLevel)
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go handleSignals(cancel, log)

	switch cmd {
	case "skills":
		return cmdSkills(cfg)
	case "tools":
		return cmdTools(ctx, cfg, log)
	case "prompt":
		return cmdPrompt(ctx, cfg, log)
	case "run", "poll", "once", "ask":
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}

	skills, err := skill.Load(cfg.Skills.Dir)
	if err != nil {
		return err
	}
	log.Info("skills loaded", "count", skills.Len(), "dir", cfg.Skills.Dir)

	var source agent.ToolSource
	if len(cfg.MCP.Servers) > 0 {
		hub, err := mcp.Connect(ctx, cfg.MCP.Servers, log)
		if err != nil {
			return err
		}
		defer hub.Close()
		source = agent.HubSource{Hub: hub}
		log.Info("mcp tools available", "count", len(hub.Tools()))
	} else {
		log.Warn("no MCP servers configured; the agent can only read skills")
	}

	runner := llm.New(cfg.LLM, log)
	ag, err := agent.New(cfg.Agent, runner, skills, source, log)
	if err != nil {
		return err
	}
	if cmd == "ask" {
		if *prompt == "" {
			return errors.New("ask requires -prompt")
		}
		res, err := ag.Ask(ctx, *prompt)
		fmt.Println("---")
		fmt.Println(res.FinalText)
		fmt.Printf("--- stop=%s turns=%d tool_calls=%d in=%d out=%d took=%s\n",
			res.StopReason, res.Turns, res.ToolCalls, res.InputTokens, res.OutputTokens, res.Duration.Round(1e9))
		return err
	}
	store, err := state.Open(cfg.Agent.StateFile)
	if err != nil {
		return err
	}
	gh := github.New(cfg.GitHub.APIBase, cfg.GitHub.Token)
	orch := orchestrator.New(cfg, gh, ag, store, log)

	switch cmd {
	case "run":
		log.Info("gao starting", "version", version, "repos", cfg.GitHub.Repos, "model", cfg.LLM.Model,
			"max_concurrent", cfg.Agent.MaxConcurrent, "poll", cfg.GitHub.PollInterval)
		return orch.Run(ctx)
	case "poll":
		var errs []error
		for _, r := range cfg.GitHub.Repos {
			n, err := orch.PollOnce(ctx, r)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r, err))
				continue
			}
			log.Info("poll", "repo", r, "dispatched", n)
		}
		// Wait for dispatched work by running with an already-cancelled poller.
		done, cancel := context.WithCancel(ctx)
		cancel()
		_ = orch.Run(done)
		return errors.Join(errs...)
	case "once":
		if *repo == "" || *issue == 0 {
			return errors.New("once requires -repo and -issue")
		}
		is, err := gh.GetIssue(ctx, *repo, *issue)
		if err != nil {
			return err
		}
		res, err := orch.Process(ctx, is)
		fmt.Println("---")
		fmt.Println(res.FinalText)
		fmt.Printf("--- stop=%s turns=%d tool_calls=%d in=%d out=%d took=%s\n",
			res.StopReason, res.Turns, res.ToolCalls, res.InputTokens, res.OutputTokens, res.Duration.Round(1e9))
		return err
	}
	return nil
}

func cmdSkills(cfg config.Config) error {
	reg, err := skill.Load(cfg.Skills.Dir)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tLABELS\tDESCRIPTION\tPATH")
	for _, s := range reg.List() {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, strings.Join(s.Labels, ","), strings.TrimSpace(s.Description), s.Path)
	}
	return tw.Flush()
}

func cmdTools(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if len(cfg.MCP.Servers) == 0 {
		return errors.New("no MCP servers configured")
	}
	hub, err := mcp.Connect(ctx, cfg.MCP.Servers, log)
	if err != nil {
		return err
	}
	defer hub.Close()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tSERVER\tDESCRIPTION")
	for _, t := range hub.Tools() {
		d := strings.SplitN(strings.TrimSpace(t.Description), "\n", 2)[0]
		if len(d) > 90 {
			d = d[:90] + "…"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t.Name, t.Server, d)
	}
	return tw.Flush()
}

func cmdPrompt(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	skills, err := skill.Load(cfg.Skills.Dir)
	if err != nil {
		return err
	}
	ag, err := agent.New(cfg.Agent, nil, skills, nil, log)
	if err != nil {
		return err
	}
	fmt.Println(ag.SystemPrompt())
	return nil
}

// handleSignals makes the first SIGINT/SIGTERM a graceful drain and the
// second an immediate exit.
func handleSignals(cancel context.CancelFunc, log *slog.Logger) {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Info("shutting down: draining in-flight issues; send the signal again to force exit")
	cancel()
	<-sig
	log.Warn("forced exit")
	os.Exit(130)
}

func newLogger(format, level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
