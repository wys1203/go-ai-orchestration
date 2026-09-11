package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListIssuesFiltersPRsAndPaginates(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.String())
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing auth header")
		}
		page := r.URL.Query().Get("page")
		var out []map[string]any
		if page == "1" {
			for i := 0; i < 100; i++ {
				m := map[string]any{"number": i + 1, "title": "t", "state": "open"}
				if i == 0 {
					m["pull_request"] = map[string]any{"url": "x"}
				}
				out = append(out, m)
			}
		} else {
			out = []map[string]any{{"number": 101, "title": "last", "labels": []map[string]any{{"name": "ai"}}}}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	issues, err := c.ListIssues(context.Background(), "o/r", ListOptions{Labels: []string{"ai"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 100 || issues[0].Number != 2 || issues[99].Number != 101 || issues[99].Repo != "o/r" {
		t.Fatalf("got %d issues, first=%d last=%d", len(issues), issues[0].Number, issues[len(issues)-1].Number)
	}
	if !issues[99].HasLabel("AI") {
		t.Fatal("label lookup should be case-insensitive")
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 pages, got %v", calls)
	}
}

func TestRemoveLabelIgnores404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Error(w, `{"message":"Label does not exist"}`, http.StatusNotFound)
			return
		}
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	if err := c.RemoveLabel(context.Background(), "o/r", 1, "x"); err != nil {
		t.Fatalf("404 should be ignored: %v", err)
	}
	err := c.AddLabels(context.Background(), "o/r", 1, "x")
	var se *StatusError
	if !errorsAs(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("got %v", err)
	}
}
