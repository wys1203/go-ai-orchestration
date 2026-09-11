// Package orchestrator polls GitHub repositories and fans issues out to a
// bounded pool of concurrent agent runs.
//
// Concurrency model:
//   - one poller goroutine per repository
//   - a global semaphore of size MaxConcurrent bounds simultaneous agent runs
//   - an in-flight set prevents the same issue from being dispatched twice
//   - a state store remembers finished issues across restarts
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/wys1203/go-ai-orchestration/internal/config"
	"github.com/wys1203/go-ai-orchestration/internal/github"
	"github.com/wys1203/go-ai-orchestration/internal/llm"
	"github.com/wys1203/go-ai-orchestration/internal/state"
)

// IssueSource is the GitHub surface the orchestrator needs.
type IssueSource interface {
	ListIssues(ctx context.Context, repo string, opt github.ListOptions) ([]github.Issue, error)
	AddLabels(ctx context.Context, repo string, number int, labels ...string) error
	RemoveLabel(ctx context.Context, repo string, number int, label string) error
	CreateComment(ctx context.Context, repo string, number int, body string) error
}

// Handler processes one issue.
type Handler interface {
	HandleIssue(ctx context.Context, is github.Issue) (llm.Result, error)
}

// Orchestrator runs the watch loop.
type Orchestrator struct {
	cfg     config.Config
	gh      IssueSource
	handler Handler
	store   *state.Store
	log     *slog.Logger

	sem      chan struct{}
	inflight map[string]struct{}
	mu       sync.Mutex
	wg       sync.WaitGroup
}

// New constructs an Orchestrator.
func New(cfg config.Config, gh IssueSource, h Handler, store *state.Store, log *slog.Logger) *Orchestrator {
	if log == nil {
		log = slog.Default()
	}
	return &Orchestrator{
		cfg: cfg, gh: gh, handler: h, store: store, log: log,
		sem:      make(chan struct{}, cfg.Agent.MaxConcurrent),
		inflight: map[string]struct{}{},
	}
}

// Run polls until ctx is cancelled, then drains queued and in-flight issues.
// Each issue runs under its own timeout detached from ctx.
func (o *Orchestrator) Run(ctx context.Context) error {
	if len(o.cfg.GitHub.Repos) == 0 {
		return errors.New("no repositories configured")
	}
	var pollers sync.WaitGroup
	for _, repo := range o.cfg.GitHub.Repos {
		pollers.Add(1)
		go func(repo string) {
			defer pollers.Done()
			o.pollLoop(ctx, repo)
		}(repo)
	}
	pollers.Wait()
	o.log.Info("pollers stopped, waiting for in-flight issues", "inflight", o.Inflight())
	o.wg.Wait()
	return nil
}

// Inflight reports the number of issues currently being processed.
func (o *Orchestrator) Inflight() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.inflight)
}

func (o *Orchestrator) pollLoop(ctx context.Context, repo string) {
	t := time.NewTicker(o.cfg.GitHub.PollInterval)
	defer t.Stop()
	for {
		if n, err := o.PollOnce(ctx, repo); err != nil {
			o.log.Error("poll failed", "repo", repo, "err", err)
		} else if n > 0 {
			o.log.Info("dispatched issues", "repo", repo, "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PollOnce lists candidate issues for repo and dispatches new ones. It
// returns how many were dispatched.
func (o *Orchestrator) PollOnce(ctx context.Context, repo string) (int, error) {
	opt := github.ListOptions{}
	if !o.cfg.GitHub.ProcessAll && o.cfg.GitHub.TriggerLabel != "" {
		opt.Labels = []string{o.cfg.GitHub.TriggerLabel}
	}
	issues, err := o.gh.ListIssues(ctx, repo, opt)
	if err != nil {
		return 0, err
	}
	dispatched := 0
	for _, is := range issues {
		if o.Dispatch(ctx, is) {
			dispatched++
		}
	}
	return dispatched, nil
}

// ShouldProcess decides whether an issue needs a run.
func (o *Orchestrator) ShouldProcess(is github.Issue) (bool, string) {
	gh := o.cfg.GitHub
	if is.State != "" && is.State != "open" {
		return false, "not open"
	}
	if gh.InProgressLabel != "" && is.HasLabel(gh.InProgressLabel) {
		return false, "in progress label"
	}
	if gh.FailedLabel != "" && is.HasLabel(gh.FailedLabel) {
		return false, "failed label"
	}
	if gh.DoneLabel != "" && is.HasLabel(gh.DoneLabel) {
		return false, "done label"
	}
	if !gh.ProcessAll && gh.TriggerLabel != "" && !is.HasLabel(gh.TriggerLabel) {
		return false, "no trigger label"
	}
	if e, ok := o.store.Get(state.Key(is.Repo, is.Number)); ok {
		if e.Status == state.StatusFailed {
			return false, "previously failed"
		}
		if e.UpdatedAt == is.UpdatedAt.UTC().Format(time.RFC3339) {
			return false, "already processed at this update"
		}
		// A done issue that still carries the trigger label and was updated
		// since is re-run only in process_all mode; in label mode the done
		// label check above already excludes it.
	}
	return true, ""
}

// Dispatch schedules is if it should be processed and is not already in
// flight. It never blocks; the issue waits for a free worker slot in its own
// goroutine.
func (o *Orchestrator) Dispatch(ctx context.Context, is github.Issue) bool {
	ok, reason := o.ShouldProcess(is)
	key := state.Key(is.Repo, is.Number)
	if !ok {
		o.log.Debug("skip", "issue", key, "reason", reason)
		return false
	}
	o.mu.Lock()
	if _, busy := o.inflight[key]; busy {
		o.mu.Unlock()
		return false
	}
	o.inflight[key] = struct{}{}
	o.mu.Unlock()

	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer func() {
			o.mu.Lock()
			delete(o.inflight, key)
			o.mu.Unlock()
		}()
		// Wait for a slot even if ctx is cancelled: a queued issue has no
		// label or state yet, so finishing it is cheaper than re-discovering
		// it on the next start. Run waits for this goroutine.
		o.sem <- struct{}{}
		defer func() { <-o.sem }()
		o.process(ctx, is)
	}()
	return true
}

// Process runs one issue synchronously (used by `gao once`).
func (o *Orchestrator) Process(ctx context.Context, is github.Issue) (llm.Result, error) {
	return o.process(ctx, is)
}

func (o *Orchestrator) process(parent context.Context, is github.Issue) (llm.Result, error) {
	key := state.Key(is.Repo, is.Number)
	log := o.log.With("issue", key)
	gh := o.cfg.GitHub

	// Detach from the poller context so a shutdown lets the issue finish
	// within its own timeout instead of aborting mid-write.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), o.cfg.Agent.IssueTimeout)
	defer cancel()

	if gh.InProgressLabel != "" {
		if err := o.gh.AddLabels(ctx, is.Repo, is.Number, gh.InProgressLabel); err != nil {
			log.Warn("add in-progress label", "err", err)
		}
	}
	log.Info("processing", "title", is.Title)
	res, err := o.handler.HandleIssue(ctx, is)
	entry := state.Entry{UpdatedAt: is.UpdatedAt.UTC().Format(time.RFC3339)}
	if err != nil {
		entry.Status = state.StatusFailed
		entry.Error = err.Error()
		log.Error("failed", "err", err, "turns", res.Turns, "tool_calls", res.ToolCalls)
		o.finish(ctx, is, gh.FailedLabel)
		if gh.CommentOnFailure {
			msg := fmt.Sprintf("🤖 %s could not complete this issue automatically.\n\n```\n%s\n```\nRemove the `%s` label and re-add `%s` to retry.",
				o.cfg.Agent.Name, truncate(err.Error(), 800), gh.FailedLabel, gh.TriggerLabel)
			if cerr := o.gh.CreateComment(ctx, is.Repo, is.Number, msg); cerr != nil {
				log.Warn("comment on failure", "err", cerr)
			}
		}
	} else {
		entry.Status = state.StatusDone
		log.Info("done", "turns", res.Turns, "tool_calls", res.ToolCalls,
			"in_tokens", res.InputTokens, "out_tokens", res.OutputTokens, "took", res.Duration.Round(time.Second))
		o.finish(ctx, is, gh.DoneLabel)
	}
	if serr := o.store.Set(key, entry); serr != nil {
		log.Error("persist state", "err", serr)
	}
	return res, err
}

func (o *Orchestrator) finish(ctx context.Context, is github.Issue, terminal string) {
	gh := o.cfg.GitHub
	if gh.InProgressLabel != "" {
		if err := o.gh.RemoveLabel(ctx, is.Repo, is.Number, gh.InProgressLabel); err != nil {
			o.log.Warn("remove in-progress label", "issue", is.Number, "err", err)
		}
	}
	if terminal != "" {
		if err := o.gh.AddLabels(ctx, is.Repo, is.Number, terminal); err != nil {
			o.log.Warn("add terminal label", "issue", is.Number, "label", terminal, "err", err)
		}
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
