package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wys1203/go-ai-orchestration/internal/config"
	"github.com/wys1203/go-ai-orchestration/internal/github"
	"github.com/wys1203/go-ai-orchestration/internal/llm"
	"github.com/wys1203/go-ai-orchestration/internal/skill"
)

type fakeRunner struct {
	system, prompt string
	tools          []llm.ToolDef
	exec           llm.Executor
	calls          map[string]int
}

func (f *fakeRunner) Run(_ context.Context, system, prompt string, tools []llm.ToolDef, exec llm.Executor) (llm.Result, error) {
	f.system, f.prompt, f.tools, f.exec = system, prompt, tools, exec
	return llm.Result{FinalText: "done", Calls: f.calls}, nil
}

type fakeTools struct{ called string }

func (f *fakeTools) Tools() []ToolSpec {
	return []ToolSpec{{Name: "gh__get", Description: "get", InputSchema: map[string]any{"type": "object"}}}
}
func (f *fakeTools) Call(_ context.Context, name string, _ json.RawMessage) (string, bool, error) {
	f.called = name
	return "result", false, nil
}

func loadSkills(t *testing.T) *skill.Registry {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "triage"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "triage", "SKILL.md"), []byte("---\ndescription: Triage.\nlabels: [bug]\n---\nTRIAGE BODY"), 0o644)
	reg, err := skill.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestHandleIssue(t *testing.T) {
	r := &fakeRunner{}
	ft := &fakeTools{}
	ag, err := New(config.Agent{}, r, loadSkills(t), ft, nil)
	if err != nil {
		t.Fatal(err)
	}
	is := github.Issue{Repo: "o/r", Number: 3, Title: "Crash", Body: "it crashes"}
	is.Labels = append(is.Labels, struct {
		Name string `json:"name"`
	}{Name: "bug"})
	if _, err := ag.HandleIssue(context.Background(), is); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.system, "- triage: Triage.") {
		t.Fatalf("skill index missing from system prompt:\n%s", r.system)
	}
	if !strings.Contains(r.prompt, "TRIAGE BODY") || !strings.Contains(r.prompt, "it crashes") {
		t.Fatalf("label-matched skill or body missing from prompt:\n%s", r.prompt)
	}
	if len(r.tools) != 2 || r.tools[0].Name != LoadSkillTool || r.tools[1].Name != "gh__get" {
		t.Fatalf("tools: %+v", r.tools)
	}
	out, isErr, _ := r.exec.Execute(context.Background(), LoadSkillTool, json.RawMessage(`{"name":"triage"}`))
	if isErr || out != "TRIAGE BODY" {
		t.Fatalf("load_skill: %q %v", out, isErr)
	}
	out, isErr, _ = r.exec.Execute(context.Background(), LoadSkillTool, json.RawMessage(`{"name":"nope"}`))
	if !isErr || !strings.Contains(out, "triage") {
		t.Fatalf("unknown skill: %q %v", out, isErr)
	}
	if out, _, _ := r.exec.Execute(context.Background(), "gh__get", nil); out != "result" || ft.called != "gh__get" {
		t.Fatalf("mcp passthrough failed: %q", out)
	}
}

func TestCustomSystemPrompt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.md")
	_ = os.WriteFile(p, []byte("CUSTOM"), 0o644)
	ag, err := New(config.Agent{SystemPromptFile: p}, nil, loadSkills(t), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ag.SystemPrompt(), "CUSTOM") {
		t.Fatal(ag.SystemPrompt())
	}
	if _, err := New(config.Agent{SystemPromptFile: "/nonexistent"}, nil, loadSkills(t), nil, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestNudgeRequiredTools(t *testing.T) {
	r := &fakeRunner{calls: map[string]int{"gh__comment": 1}}
	ag, err := New(config.Agent{RequiredTools: []string{"gh__comment"}, MaxNudges: 2}, r, loadSkills(t), &fakeTools{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag.HandleIssue(context.Background(), github.Issue{Repo: "o/r", Number: 1}); err != nil {
		t.Fatal(err)
	}
	r.calls = map[string]int{}
	if _, err := ag.HandleIssue(context.Background(), github.Issue{Repo: "o/r", Number: 1}); !errors.Is(err, ErrRequiredTools) {
		t.Fatalf("expected ErrRequiredTools, got %v", err)
	}
	if !strings.Contains(r.system, "Required actions") || !strings.Contains(r.system, "gh__comment") {
		t.Fatalf("required tools missing from system prompt")
	}
	n, ok := r.exec.(llm.Nudger)
	if !ok {
		t.Fatal("executor should implement Nudger")
	}
	if msg := n.Nudge(llm.Result{}, map[string]int{}, 1); !strings.Contains(msg, "gh__comment") {
		t.Fatalf("expected nudge, got %q", msg)
	}
	if msg := n.Nudge(llm.Result{}, map[string]int{"gh__comment": 1}, 1); msg != "" {
		t.Fatalf("expected no nudge, got %q", msg)
	}
	if msg := n.Nudge(llm.Result{}, map[string]int{}, 3); msg != "" {
		t.Fatalf("expected give-up after max nudges, got %q", msg)
	}
}

func TestGuardRejectsOtherRepoAndIssue(t *testing.T) {
	r := &fakeRunner{calls: map[string]int{}}
	ft := &fakeTools{}
	cfg := config.Agent{IssueScopedTools: []string{"gh__add_issue_comment"}}
	ag, err := New(cfg, r, loadSkills(t), ft, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = ag.HandleIssue(context.Background(), github.Issue{Repo: "Wys1203/Go-AI", Number: 7})
	ctx := context.Background()
	cases := []struct {
		name, tool, input string
		rejected          bool
	}{
		{"other repo", "gh__get", `{"owner":"octocat","repo":"Hello-World"}`, true},
		{"same repo case-insensitive", "gh__get", `{"owner":"wys1203","repo":"go-ai"}`, false},
		{"no repo args", "gh__get", `{"query":"x"}`, false},
		{"repo given as owner/name", "gh__get", `{"owner":"wys1203","repo":"wys1203/go-ai"}`, false},
		{"repo owner/name without owner", "gh__get", `{"repo":"WYS1203/go-ai"}`, false},
		{"repo owner/name wrong owner", "gh__get", `{"repo":"octocat/go-ai"}`, true},
		{"scoped tool other issue", "gh__add_issue_comment", `{"owner":"wys1203","repo":"go-ai","issue_number":15}`, true},
		{"scoped tool same issue", "gh__add_issue_comment", `{"owner":"wys1203","repo":"go-ai","issue_number":7}`, false},
		{"unscoped read other issue", "gh__get", `{"owner":"wys1203","repo":"go-ai","issue_number":15}`, false},
		{"load_skill unaffected", LoadSkillTool, `{"name":"triage"}`, false},
	}
	for _, c := range cases {
		ft.called = ""
		out, isErr, _ := r.exec.Execute(ctx, c.tool, json.RawMessage(c.input))
		if isErr != c.rejected {
			t.Errorf("%s: rejected=%v want %v (%s)", c.name, isErr, c.rejected, out)
		}
		if c.rejected && ft.called != "" {
			t.Errorf("%s: tool was executed despite rejection", c.name)
		}
	}
	// guard is off in Ask mode (no target issue)
	_, _ = ag.Ask(ctx, "x")
	if _, isErr, _ := r.exec.Execute(ctx, "gh__get", json.RawMessage(`{"owner":"octocat","repo":"x"}`)); isErr {
		t.Error("ask mode should not guard")
	}
}
