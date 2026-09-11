package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wys1203/go-ai-orchestration/internal/config"
	"github.com/wys1203/go-ai-orchestration/internal/github"
	"github.com/wys1203/go-ai-orchestration/internal/llm"
	"github.com/wys1203/go-ai-orchestration/internal/state"
)

type fakeGH struct {
	mu       sync.Mutex
	issues   []github.Issue
	labels   map[int][]string
	comments map[int][]string
}

func newFakeGH(issues ...github.Issue) *fakeGH {
	return &fakeGH{issues: issues, labels: map[int][]string{}, comments: map[int][]string{}}
}

func (f *fakeGH) ListIssues(context.Context, string, github.ListOptions) ([]github.Issue, error) {
	return f.issues, nil
}
func (f *fakeGH) AddLabels(_ context.Context, _ string, n int, labels ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels[n] = append(f.labels[n], labels...)
	return nil
}
func (f *fakeGH) RemoveLabel(_ context.Context, _ string, n int, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels[n] = append(f.labels[n], "-"+label)
	return nil
}
func (f *fakeGH) CreateComment(_ context.Context, _ string, n int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[n] = append(f.comments[n], body)
	return nil
}

type fakeHandler struct {
	running, peak int32
	fail          map[int]bool
	delay         time.Duration
}

func (h *fakeHandler) HandleIssue(_ context.Context, is github.Issue) (llm.Result, error) {
	cur := atomic.AddInt32(&h.running, 1)
	for {
		p := atomic.LoadInt32(&h.peak)
		if cur <= p || atomic.CompareAndSwapInt32(&h.peak, p, cur) {
			break
		}
	}
	time.Sleep(h.delay)
	atomic.AddInt32(&h.running, -1)
	if h.fail[is.Number] {
		return llm.Result{}, errors.New("nope")
	}
	return llm.Result{FinalText: "ok", Turns: 1}, nil
}

func issue(n int, labels ...string) github.Issue {
	is := github.Issue{Repo: "o/r", Number: n, State: "open", Title: "t", UpdatedAt: time.Now()}
	for _, l := range labels {
		is.Labels = append(is.Labels, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	return is
}

func testCfg() config.Config {
	cfg := config.Default()
	cfg.GitHub.Repos = []string{"o/r"}
	cfg.Agent.MaxConcurrent = 2
	cfg.Agent.IssueTimeout = time.Minute
	cfg.Agent.StateFile = ""
	return cfg
}

func TestPollDispatchesAndRespectsConcurrency(t *testing.T) {
	gh := newFakeGH(issue(1, "ai"), issue(2, "ai"), issue(3, "ai"), issue(4, "ai", "ai:done"), issue(5))
	h := &fakeHandler{delay: 50 * time.Millisecond, fail: map[int]bool{3: true}}
	store, _ := state.Open("")
	o := New(testCfg(), gh, h, store, nil)

	ctx, cancel := context.WithCancel(context.Background())
	n, err := o.PollOnce(ctx, "o/r")
	if err != nil || n != 3 {
		t.Fatalf("dispatched %d err %v", n, err)
	}
	// second poll while in flight must not double-dispatch
	if n, _ := o.PollOnce(ctx, "o/r"); n != 0 {
		t.Fatalf("double dispatch: %d", n)
	}
	cancel()
	_ = o.Run(ctx) // returns after queued and in-flight work drains
	if o.Inflight() != 0 {
		t.Fatalf("inflight after drain: %d", o.Inflight())
	}

	if h.peak > 2 {
		t.Fatalf("concurrency exceeded: peak %d", h.peak)
	}
	e1, _ := store.Get(state.Key("o/r", 1))
	e2, _ := store.Get(state.Key("o/r", 2))
	e3, _ := store.Get(state.Key("o/r", 3))
	if e1.Status != state.StatusDone || e2.Status != state.StatusDone || e3.Status != state.StatusFailed {
		t.Fatalf("state: %+v %+v %+v", e1, e2, e3)
	}
	if got := gh.labels[1]; len(got) != 3 || got[0] != "ai:working" || got[1] != "-ai:working" || got[2] != "ai:done" {
		t.Fatalf("labels for #1: %v", got)
	}
	if got := gh.labels[3]; got[len(got)-1] != "ai:failed" {
		t.Fatalf("labels for #3: %v", got)
	}
	if len(gh.comments[3]) != 1 {
		t.Fatalf("expected failure comment: %v", gh.comments)
	}
	// after completion, the same updated_at is not reprocessed
	if n, _ := o.PollOnce(context.Background(), "o/r"); n != 0 {
		t.Fatalf("reprocessed finished issues: %d", n)
	}
}

func TestShouldProcess(t *testing.T) {
	store, _ := state.Open("")
	o := New(testCfg(), newFakeGH(), &fakeHandler{}, store, nil)
	cases := []struct {
		name string
		is   github.Issue
		want bool
	}{
		{"trigger", issue(1, "ai"), true},
		{"no trigger", issue(2), false},
		{"done", issue(3, "ai", "ai:done"), false},
		{"working", issue(4, "ai", "ai:working"), false},
		{"closed", func() github.Issue { i := issue(5, "ai"); i.State = "closed"; return i }(), false},
	}
	for _, c := range cases {
		if got, why := o.ShouldProcess(c.is); got != c.want {
			t.Errorf("%s: got %v (%s)", c.name, got, why)
		}
	}
	o.cfg.GitHub.ProcessAll = true
	if ok, _ := o.ShouldProcess(issue(6)); !ok {
		t.Error("process_all should accept unlabeled issue")
	}
}
