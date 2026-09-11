package agent

import (
	"context"
	"encoding/json"
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
}

func (f *fakeRunner) Run(_ context.Context, system, prompt string, tools []llm.ToolDef, exec llm.Executor) (llm.Result, error) {
	f.system, f.prompt, f.tools, f.exec = system, prompt, tools, exec
	return llm.Result{FinalText: "done"}, nil
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
