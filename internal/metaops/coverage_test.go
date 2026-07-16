package metaops

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// metaServer is a configurable fake meta.audiosilo.app.
type metaServer struct {
	lookup   map[string]string // asin/isbn -> work id ("" => 404)
	missing  map[string][]string
	hasMiss  bool // include a missing[] list in /coverage
	work     map[string]struct{ c, r bool }
	requests map[string]int
}

func (s *metaServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/lookup", func(w http.ResponseWriter, r *http.Request) {
		s.requests["lookup"]++
		key := r.URL.Query().Get("asin")
		if key == "" {
			key = r.URL.Query().Get("isbn")
		}
		id, ok := s.lookup[key]
		if !ok || id == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"work":{"id":"` + id + `"},"recording_id":"rec"}`))
	})
	mux.HandleFunc("/api/v1/coverage", func(w http.ResponseWriter, r *http.Request) {
		s.requests["coverage"]++
		w.Header().Set("Content-Type", "application/json")
		if !s.hasMiss {
			_, _ = w.Write([]byte(`{"totals":{"works":10}}`))
			return
		}
		body := `{"totals":{"works":10},"missing":[`
		first := true
		for id, dims := range s.missing {
			if !first {
				body += ","
			}
			first = false
			body += `{"id":"` + id + `","title":"t","authors":[],"missing":[`
			for i, d := range dims {
				if i > 0 {
					body += ","
				}
				body += `"` + d + `"`
			}
			body += `]}`
		}
		body += `]}`
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v1/works/", func(w http.ResponseWriter, r *http.Request) {
		s.requests["works"]++
		id := r.URL.Path[len("/api/v1/works/"):]
		wk, ok := s.work[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := `{"id":"` + id + `","title":"t"`
		if wk.c {
			body += `,"characters":[{"id":"x","name":"X"}]`
		}
		if wk.r {
			body += `,"recaps":[{"through":{"chapter":1},"text":"t"}]`
		}
		body += `}`
		_, _ = w.Write([]byte(body))
	})
	return mux
}

func newMeta(t *testing.T, s *metaServer) (*Client, *httptest.Server) {
	t.Helper()
	if s.requests == nil {
		s.requests = map[string]int{}
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return NewClient(srv.URL), srv
}

func TestCoverageFromMissingFeed(t *testing.T) {
	s := &metaServer{
		lookup:  map[string]string{"B-COVERED": "w-covered", "B-CHARS": "w-chars"},
		hasMiss: true,
		// w-chars is missing characters+recaps; w-covered is absent from the list
		// (= fully covered). "some-other" is missing to prove filtering.
		missing: map[string][]string{
			"w-chars":    {"characters", "recaps"},
			"some-other": {"recaps"},
		},
	}
	c, _ := newMeta(t, s)
	ctx := context.Background()

	covered, _ := c.CoverageFor(ctx, "B-COVERED", "")
	if !covered.Available || !covered.Known || !covered.HasCharacters || !covered.HasRecaps {
		t.Fatalf("fully-covered work: %+v", covered)
	}
	chars, _ := c.CoverageFor(ctx, "B-CHARS", "")
	if !chars.Known || chars.HasCharacters || chars.HasRecaps || chars.WorkID != "w-chars" {
		t.Fatalf("missing-both work: %+v", chars)
	}
	// The work-detail endpoint must NOT be consulted when the feed carries missing[].
	if s.requests["works"] != 0 {
		t.Errorf("work detail called %d times despite missing feed", s.requests["works"])
	}
	// Second call is served from cache (no extra lookup requests beyond the 2).
	_, _ = c.CoverageFor(ctx, "B-COVERED", "")
	if s.requests["lookup"] != 2 {
		t.Errorf("lookup requests = %d, want 2 (cached)", s.requests["lookup"])
	}
}

func TestCoverageFallsBackToWorkDetail(t *testing.T) {
	// Older server: /coverage omits missing[], so per-work detail decides.
	s := &metaServer{
		lookup:  map[string]string{"B1": "w1", "B2": "w2"},
		hasMiss: false,
		work: map[string]struct{ c, r bool }{
			"w1": {c: true, r: false},
			"w2": {c: false, r: false},
		},
	}
	c, _ := newMeta(t, s)
	ctx := context.Background()

	got1, _ := c.CoverageFor(ctx, "B1", "")
	if !got1.Known || !got1.HasCharacters || got1.HasRecaps {
		t.Fatalf("w1: %+v", got1)
	}
	got2, _ := c.CoverageFor(ctx, "B2", "")
	if !got2.Known || got2.HasCharacters || got2.HasRecaps {
		t.Fatalf("w2: %+v", got2)
	}
	if s.requests["works"] == 0 {
		t.Error("expected the work-detail fallback to be used")
	}
}

func TestCoverageLookupMissAndNoIdentity(t *testing.T) {
	s := &metaServer{lookup: map[string]string{}, hasMiss: true}
	c, _ := newMeta(t, s)
	ctx := context.Background()

	// No identity at all: available, but unknown.
	none, _ := c.CoverageFor(ctx, "", "")
	if !none.Available || none.Known {
		t.Fatalf("no identity: %+v", none)
	}
	// Identity present but the work is not in the DB (404).
	miss, _ := c.CoverageFor(ctx, "B-UNKNOWN", "")
	if !miss.Available || miss.Known {
		t.Fatalf("lookup miss: %+v", miss)
	}
}

func TestCoverageDisabledAndUnreachable(t *testing.T) {
	ctx := context.Background()
	// Disabled: no base URL.
	off := NewClient("")
	if got, _ := off.CoverageFor(ctx, "B1", ""); got.Available {
		t.Fatalf("disabled client should be unavailable: %+v", got)
	}
	// Unreachable upstream degrades to unavailable, never an error.
	dead := NewClient("http://127.0.0.1:0")
	got, err := dead.CoverageFor(ctx, "B1", "")
	if err != nil {
		t.Fatalf("unreachable should not error: %v", err)
	}
	if got.Available {
		t.Fatalf("unreachable should be unavailable: %+v", got)
	}
}

func TestSearch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/search", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"kind":"work","id":"w1","title":"Hedge Wizard"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(srv.URL)
	res, err := c.Search(context.Background(), "hedge wizard")
	if err != nil || len(res) != 1 || res[0].ID != "w1" {
		t.Fatalf("Search = %+v, %v", res, err)
	}
	if _, err := NewClient("").Search(context.Background(), "x"); err != ErrDisabled {
		t.Fatalf("disabled Search should return ErrDisabled, got %v", err)
	}
}
