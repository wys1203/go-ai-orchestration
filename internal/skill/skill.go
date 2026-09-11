// Package skill loads Agent Skills from SKILL.md files.
//
// A skill is a directory containing SKILL.md with YAML frontmatter:
//
//	---
//	name: issue-triage
//	description: Classify and label a new issue.
//	labels: [bug, question]   # optional: auto-attach when the issue has one of these
//	---
//	<markdown body with instructions>
//
// Skills follow progressive disclosure: only name and description are put in
// the system prompt; the body is fetched on demand through the load_skill
// tool, or inlined automatically when an issue label matches.
package skill

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill is a parsed SKILL.md.
type Skill struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Labels      []string `yaml:"labels"`
	Body        string   `yaml:"-"`
	Path        string   `yaml:"-"`
}

// Registry is an immutable set of skills.
type Registry struct {
	skills map[string]Skill
	order  []string
}

// Load walks dir for */SKILL.md files. A missing dir yields an empty registry.
func Load(dir string) (*Registry, error) {
	reg := &Registry{skills: map[string]Skill{}}
	if dir == "" {
		return reg, nil
	}
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return reg, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("skills dir %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), "SKILL.md")
		raw, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		s, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if s.Name == "" {
			s.Name = e.Name()
		}
		s.Path = p
		if _, dup := reg.skills[s.Name]; dup {
			return nil, fmt.Errorf("%s: duplicate skill name %q", p, s.Name)
		}
		reg.skills[s.Name] = s
		reg.order = append(reg.order, s.Name)
	}
	sort.Strings(reg.order)
	return reg, nil
}

// Parse splits frontmatter from body.
func Parse(raw []byte) (Skill, error) {
	var s Skill
	trimmed := bytes.TrimLeft(raw, "\xef\xbb\xbf \t\r\n")
	if !bytes.HasPrefix(trimmed, []byte("---")) {
		s.Body = strings.TrimSpace(string(raw))
		return s, nil
	}
	rest := trimmed[3:]
	// frontmatter ends at the next line that is exactly "---"
	idx := bytes.Index(rest, []byte("\n---"))
	if idx < 0 {
		return s, errors.New("unterminated frontmatter")
	}
	front := rest[:idx]
	body := rest[idx+4:]
	if err := yaml.Unmarshal(front, &s); err != nil {
		return s, fmt.Errorf("frontmatter: %w", err)
	}
	s.Body = strings.TrimSpace(string(body))
	return s, nil
}

// Get returns a skill by name.
func (r *Registry) Get(name string) (Skill, bool) {
	s, ok := r.skills[name]
	return s, ok
}

// List returns skills sorted by name.
func (r *Registry) List() []Skill {
	out := make([]Skill, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.skills[n])
	}
	return out
}

// Len reports the number of skills.
func (r *Registry) Len() int { return len(r.order) }

// Matching returns skills whose Labels intersect labels.
func (r *Registry) Matching(labels []string) []Skill {
	set := map[string]bool{}
	for _, l := range labels {
		set[strings.ToLower(l)] = true
	}
	var out []Skill
	for _, s := range r.List() {
		for _, l := range s.Labels {
			if set[strings.ToLower(l)] {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// Index renders the name/description catalogue for the system prompt.
func (r *Registry) Index() string {
	if r.Len() == 0 {
		return "(no skills installed)"
	}
	var b strings.Builder
	for _, s := range r.List() {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, strings.TrimSpace(s.Description))
	}
	return b.String()
}
