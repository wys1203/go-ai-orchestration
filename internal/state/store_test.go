package state

import (
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	k := Key("o/r", 7)
	if err := s.Set(k, Entry{UpdatedAt: "t1", Status: StatusFailed, Error: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(k, Entry{UpdatedAt: "t2", Status: StatusDone}); err != nil {
		t.Fatal(err)
	}
	re, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := re.Get(k)
	if !ok || e.Status != StatusDone || e.UpdatedAt != "t2" || e.Attempts != 2 || e.ProcessedAt.IsZero() {
		t.Fatalf("got %+v %v", e, ok)
	}
}

func TestInMemory(t *testing.T) {
	s, _ := Open("")
	_ = s.Set("k", Entry{})
	if s.Len() != 1 {
		t.Fatal("expected 1")
	}
}
