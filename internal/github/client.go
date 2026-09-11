// Package github is a minimal REST client for the handful of calls the
// watcher needs. The agent itself acts through the GitHub MCP server; this
// client only exists for deterministic polling and lifecycle labelling.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Issue is the subset of the GitHub issue object the agent needs.
type Issue struct {
	Repo      string    `json:"-"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"created_at"`
	Comments  int       `json:"comments"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *json.RawMessage `json:"pull_request,omitempty"`
}

// LabelNames returns the issue's labels as strings.
func (i Issue) LabelNames() []string {
	out := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		out = append(out, l.Name)
	}
	return out
}

// HasLabel reports whether the issue carries label (case-insensitive).
func (i Issue) HasLabel(label string) bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l.Name, label) {
			return true
		}
	}
	return false
}

// Client talks to the GitHub REST API v3.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New creates a client. base defaults to https://api.github.com.
func New(base, token string) *Client {
	if base == "" {
		base = "https://api.github.com"
	}
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// ListOptions filters ListIssues.
type ListOptions struct {
	Labels []string
	Since  time.Time
	State  string // open|closed|all; default open
}

// ListIssues returns open issues (pull requests excluded), newest updated first.
func (c *Client) ListIssues(ctx context.Context, repo string, opt ListOptions) ([]Issue, error) {
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("sort", "updated")
	q.Set("direction", "desc")
	q.Set("state", firstNonEmpty(opt.State, "open"))
	if len(opt.Labels) > 0 {
		q.Set("labels", strings.Join(opt.Labels, ","))
	}
	if !opt.Since.IsZero() {
		q.Set("since", opt.Since.UTC().Format(time.RFC3339))
	}
	var all []Issue
	for page := 1; page <= 10; page++ {
		q.Set("page", fmt.Sprint(page))
		var batch []Issue
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues?%s", repo, q.Encode()), nil, &batch); err != nil {
			return nil, err
		}
		for _, is := range batch {
			if is.PullRequest != nil {
				continue
			}
			is.Repo = repo
			all = append(all, is)
		}
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

// GetIssue fetches a single issue.
func (c *Client) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	var is Issue
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, &is)
	is.Repo = repo
	return is, err
}

// AddLabels appends labels to an issue.
func (c *Client) AddLabels(ctx context.Context, repo string, number int, labels ...string) error {
	if len(labels) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/labels", repo, number), map[string]any{"labels": labels}, nil)
}

// RemoveLabel removes one label; a 404 (label absent) is not an error.
func (c *Client) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	if label == "" {
		return nil
	}
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/%d/labels/%s", repo, number, url.PathEscape(label)), nil, nil)
	var se *StatusError
	if errorsAs(err, &se) && se.Code == http.StatusNotFound {
		return nil
	}
	return err
}

// CreateComment posts a comment on the issue.
func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), map[string]any{"body": body}, nil)
}

// StatusError is a non-2xx response.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.Code, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gao")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Code: resp.StatusCode, Body: truncate(string(raw), 512)}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
