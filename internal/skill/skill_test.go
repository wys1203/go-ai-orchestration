package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	raw := []byte("---\nname: triage\ndescription: Sort issues.\nlabels: [bug, Question]\n---\n\n# Body\n\nDo things.\n")
	s, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "triage" || s.Description != "Sort issues." {
		t.Fatalf("unexpected header: %+v", s)
	}
	if len(s.Labels) != 2 {
		t.Fatalf("labels: %v", s.Labels)
	}
	if s.Body != "# Body\n\nDo things." {
		t.Fatalf("body: %q", s.Body)
	}
}

func TestParseNoFrontmatter(t *testing.T) {
	s, err := Parse([]byte("just text"))
	if err != nil || s.Body != "just text" || s.Name != "" {
		t.Fatalf("got %+v, %v", s, err)
	}
}

func TestParseUnterminated(t *testing.T) {
	if _, err := Parse([]byte("---\nname: x\n")); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadAndMatch(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("b-skill", "---\ndescription: B\nlabels: [bug]\n---\nbody b")
	write("a-skill", "---\nname: a-skill\ndescription: A\n---\nbody a")
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Len() != 2 {
		t.Fatalf("want 2 skills, got %d", reg.Len())
	}
	if got := reg.List()[0].Name; got != "a-skill" {
		t.Fatalf("expected sorted order, first=%s", got)
	}
	if s, ok := reg.Get("b-skill"); !ok || s.Body != "body b" {
		t.Fatalf("dir name fallback failed: %+v %v", s, ok)
	}
	m := reg.Matching([]string{"BUG"})
	if len(m) != 1 || m[0].Name != "b-skill" {
		t.Fatalf("matching: %+v", m)
	}
	if reg.Index() == "" {
		t.Fatal("index empty")
	}
}

func TestLoadMissingDir(t *testing.T) {
	reg, err := Load(filepath.Join(t.TempDir(), "nope"))
	if err != nil || reg.Len() != 0 {
		t.Fatalf("got %v %d", err, reg.Len())
	}
}
