package contrib

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-sidecars/internal/state"
	"github.com/kodestar/audiosilo-sidecars/internal/store"
)

const corePendingMsg = "waiting for the metadata PR to merge"

// --- test doubles ---

// fakeTokenResolver is a deterministic TokenResolver (the real gh-auth fallback
// would make tests machine-dependent).
type fakeTokenResolver struct {
	token string
	err   error
}

func (f fakeTokenResolver) Resolve(context.Context) (string, string, error) {
	if f.err != nil {
		return "", "", f.err
	}
	return f.token, FromPAT, nil
}

// capture records published contrib.update events for assertions.
type capture struct {
	mu     sync.Mutex
	events []ContribUpdate
}

func (c *capture) publish(u ContribUpdate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, u)
}

func (c *capture) all() []ContribUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ContribUpdate, len(c.events))
	copy(out, c.events)
	return out
}

// readmitSpy records the ids passed to a service's Readmit hook.
type readmitSpy struct {
	mu  sync.Mutex
	ids []int64
}

func (r *readmitSpy) readmit(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return nil
}

func (r *readmitSpy) called() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, len(r.ids))
	copy(out, r.ids)
	return out
}

// --- store helpers ---

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// makeBook inserts a book and, when parkCode is non-empty, parks it (needs_attention)
// at the contributing stage with that code.
func makeBook(t *testing.T, db *store.DB, title, parkCode string) store.Book {
	t.Helper()
	b, err := db.CreateBook(context.Background(), store.NewBook{
		SourcePath: "/lib/" + title, WorkDir: t.TempDir(), Title: title,
	})
	if err != nil {
		t.Fatalf("create book: %v", err)
	}
	if parkCode != "" {
		if err := db.SetBookState(context.Background(), b.ID,
			string(state.Contributing), string(state.StatusNeedsAttention), "parked", parkCode); err != nil {
			t.Fatalf("park book: %v", err)
		}
		nb, err := db.GetBook(context.Background(), b.ID)
		if err != nil {
			t.Fatalf("reload book: %v", err)
		}
		return nb
	}
	return b
}

func addRow(t *testing.T, db *store.DB, bookID int64, kind, mode string, number int, status string) store.Contribution {
	t.Helper()
	c, err := db.UpsertContribution(context.Background(), store.Contribution{
		BookID: bookID, Kind: kind, Mode: mode, Repo: testRepo,
		Number: number, URL: "https://gh/issues/" + strconv.Itoa(number), Status: status,
	})
	if err != nil {
		t.Fatalf("upsert contribution: %v", err)
	}
	return c
}

func newService(t *testing.T, db *store.DB, ghURL string, tok TokenResolver, cap *capture, spy *readmitSpy, resolve func(context.Context, string) (string, error)) *Service {
	t.Helper()
	deps := ServiceDeps{
		DB: db, CoreRepo: testRepo, BaseURL: ghURL, Tokens: tok,
		Publish: cap.publish, CorePendingMsg: corePendingMsg, ResolveWork: resolve,
	}
	if spy != nil {
		deps.Readmit = spy.readmit
	}
	return NewService(deps)
}

func getRow(t *testing.T, db *store.DB, bookID int64, kind string) store.Contribution {
	t.Helper()
	rows, err := db.ListContributionsByBook(context.Background(), bookID)
	if err != nil {
		t.Fatalf("list contributions: %v", err)
	}
	for _, r := range rows {
		if r.Kind == kind {
			return r
		}
	}
	t.Fatalf("no %s row for book %d", kind, bookID)
	return store.Contribution{}
}

// --- poller: issue-mode transitions submitted -> pr_open -> merged ---

func TestPollIssueModeTransitions(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "A Book", "")
	addRow(t, db, b.ID, store.ContribKindCharacters, store.ContribModeIssue, 10, store.ContribStatusSubmitted)

	var merged bool
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == http.MethodGet:
			// FindIntakePR -> one open PR (#20).
			io.WriteString(w, `[{"number":20,"html_url":"https://gh/pull/20","state":"open","merged":false}]`)
		case strings.HasSuffix(r.URL.Path, "/pulls/20"):
			if merged {
				io.WriteString(w, `{"number":20,"html_url":"https://gh/pull/20","state":"closed","merged":true,"merged_at":"2026-07-17T00:00:00Z"}`)
			} else {
				io.WriteString(w, `{"number":20,"html_url":"https://gh/pull/20","state":"open","merged":false}`)
			}
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer gh.Close()

	cap := &capture{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, cap, nil, nil)

	// Tick 1: discover the intake PR -> pr_open.
	svc.Poll(context.Background())
	if row := getRow(t, db, b.ID, store.ContribKindCharacters); row.Status != store.ContribStatusPROpen || row.PRNumber != 20 {
		t.Fatalf("after tick1 = %s pr=%d, want pr_open pr=20", row.Status, row.PRNumber)
	}
	if len(cap.all()) != 1 {
		t.Fatalf("tick1 publishes = %d, want 1", len(cap.all()))
	}

	// Tick 2: the PR merges -> merged.
	merged = true
	svc.Poll(context.Background())
	if row := getRow(t, db, b.ID, store.ContribKindCharacters); row.Status != store.ContribStatusMerged {
		t.Fatalf("after tick2 = %s, want merged", row.Status)
	}
	if len(cap.all()) != 2 {
		t.Fatalf("tick2 publishes = %d, want 2", len(cap.all()))
	}

	// Tick 3: steady state (merged is terminal, not in the open set) -> no new publish.
	svc.Poll(context.Background())
	if len(cap.all()) != 2 {
		t.Fatalf("tick3 publishes = %d, want 2 (deduped)", len(cap.all()))
	}
}

// --- poller: issue closed with no merged PR -> closed ---

func TestPollIssueClosedWithoutMerge(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Closed Book", "")
	addRow(t, db, b.ID, store.ContribKindRecaps, store.ContribModeIssue, 11, store.ContribStatusSubmitted)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == http.MethodGet:
			io.WriteString(w, `[]`) // no intake PR
		case strings.HasSuffix(r.URL.Path, "/issues/11"):
			io.WriteString(w, `{"number":11,"html_url":"u","state":"closed","labels":[]}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer gh.Close()

	cap := &capture{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, cap, nil, nil)
	svc.Poll(context.Background())

	if row := getRow(t, db, b.ID, store.ContribKindRecaps); row.Status != store.ContribStatusClosed {
		t.Fatalf("status = %s, want closed", row.Status)
	}
	if len(cap.all()) != 1 {
		t.Fatalf("publishes = %d, want 1", len(cap.all()))
	}
}

// --- poller: an open issue that never produces a PR becomes visibly stalled ---

func TestPollIssueWithoutPRMarksAndClearsStaleNote(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Stalled Book", "")
	created := addRow(t, db, b.ID, store.ContribKindCharacters, store.ContribModeIssue, 12, store.ContribStatusSubmitted)
	if err := db.SetContributionStatus(context.Background(), created.ID, created.Status, 0, "", "audit passed"); err != nil {
		t.Fatal(err)
	}

	var hasPR bool
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == http.MethodGet:
			if hasPR {
				io.WriteString(w, `[{"number":22,"html_url":"https://gh/pull/22","state":"open","merged":false}]`)
			} else {
				io.WriteString(w, `[]`)
			}
		case strings.HasSuffix(r.URL.Path, "/issues/12"):
			io.WriteString(w, `{"number":12,"html_url":"u","state":"open","labels":[{"name":"data:characters"}]}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer gh.Close()

	cap := &capture{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, cap, nil, nil)
	row := getRow(t, db, b.ID, store.ContribKindCharacters)
	updatedAt, err := time.Parse(time.RFC3339Nano, row.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	// An otherwise identical tick inside the grace period changes no persisted
	// state and emits no accounting event.
	svc.now = func() time.Time { return updatedAt.Add(intakePRGracePeriod - time.Minute) }
	svc.Poll(context.Background())
	row = getRow(t, db, b.ID, store.ContribKindCharacters)
	if row.Note != "audit passed" || len(cap.all()) != 0 {
		t.Fatalf("fresh row = %+v, publishes = %d", row, len(cap.all()))
	}

	// The first stale tick adds one actionable note without losing the audit note.
	svc.now = func() time.Time { return updatedAt.Add(intakePRGracePeriod + time.Minute) }
	svc.Poll(context.Background())
	row = getRow(t, db, b.ID, store.ContribKindCharacters)
	if row.Status != store.ContribStatusSubmitted || row.Note != "audit passed; "+store.ContribNoteIntakePRStale {
		t.Fatalf("stale row = %+v", row)
	}
	if len(cap.all()) != 1 {
		t.Fatalf("stale tick publishes = %d, want 1", len(cap.all()))
	}

	// A steady stale tick is deduplicated.
	svc.Poll(context.Background())
	if len(cap.all()) != 1 {
		t.Fatalf("repeat stale tick publishes = %d, want 1", len(cap.all()))
	}

	// Once the intake PR appears, the warning clears and the original note remains.
	hasPR = true
	svc.Poll(context.Background())
	row = getRow(t, db, b.ID, store.ContribKindCharacters)
	if row.Status != store.ContribStatusPROpen || row.PRNumber != 22 || row.Note != "audit passed" {
		t.Fatalf("recovered row = %+v", row)
	}
	if len(cap.all()) != 2 {
		t.Fatalf("recovery tick publishes = %d, want 2", len(cap.all()))
	}
}

// --- poller: core merged -> slug learned from the pack files, work_id set, book
// re-admitted once the work is live ---

// packJSON renders a works pack holding the given entry keys (the entry bodies are
// irrelevant to the key diff).
func packJSON(keys ...string) string {
	var b strings.Builder
	b.WriteString(`{"entries":{`)
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `%q:{"id":%q}`, k, k)
	}
	b.WriteString("}}\n")
	return b.String()
}

// coreRepoFake stands in for the core repository around ONE merged add-work PR
// (#40, merge commit "m3rg3", first parent "p4r3nt"): the PR's changed-file list and
// every pack's content at the two revisions ("" = absent at that revision).
type coreRepoFake struct {
	files  string                                        // the /pulls/40/files JSON body
	before map[string]string                             // path -> pack JSON at the parent
	after  map[string]string                             // path -> pack JSON at the merge commit
	extra  func(http.ResponseWriter, *http.Request) bool // earlier routes (FindIntakePR, ...)
}

func (f coreRepoFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.extra != nil && f.extra(w, r) {
			return
		}
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/pulls/40"):
			io.WriteString(w, `{"number":40,"html_url":"https://gh/pull/40","state":"closed","merged":true,"merge_commit_sha":"m3rg3"}`)
		case strings.HasSuffix(p, "/pulls/40/files"):
			io.WriteString(w, f.files)
		case strings.HasSuffix(p, "/git/commits/m3rg3"):
			io.WriteString(w, `{"sha":"m3rg3","tree":{"sha":"t"},"parents":[{"sha":"p4r3nt"}]}`)
		case strings.Contains(p, "/contents/"):
			path := p[strings.Index(p, "/contents/")+len("/contents/"):]
			side := f.before
			if r.URL.Query().Get("ref") == "m3rg3" {
				side = f.after
			}
			body, ok := side[path]
			if !ok || body == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, body)
		default:
			http.Error(w, "unexpected "+r.Method+" "+p, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mergedCoreRow records a merged core row whose intake PR is #40.
func mergedCoreRow(t *testing.T, db *store.DB, bookID int64) store.Contribution {
	t.Helper()
	row := addRow(t, db, bookID, store.ContribKindCore, store.ContribModeIssue, 30, store.ContribStatusSubmitted)
	if err := db.SetContributionStatus(context.Background(), row.ID, store.ContribStatusMerged, 40, "https://gh/pull/40", ""); err != nil {
		t.Fatal(err)
	}
	return getRow(t, db, bookID, store.ContribKindCore)
}

func TestPollCoreMergedResolvesSlug(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Core Book", string(state.ParkCorePending))
	addRow(t, db, b.ID, store.ContribKindCore, store.ContribModeIssue, 30, store.ContribStatusSubmitted)

	gh := coreRepoFake{
		files:  `[{"filename":"data/works/m/mo.json","status":"modified"}]`,
		before: map[string]string{"data/works/m/mo.json": packJSON("mo-a", "mo-z")},
		after:  map[string]string{"data/works/m/mo.json": packJSON("mo-a", "my-work", "mo-z")},
		extra: func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == http.MethodGet {
				// FindIntakePR for the core issue -> the merged PR (#40).
				io.WriteString(w, `[{"number":40,"html_url":"https://gh/pull/40","state":"closed","merged":true,"merged_at":"t"}]`)
				return true
			}
			return false
		},
	}.server(t)

	cap := &capture{}
	spy := &readmitSpy{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, cap, spy, nil)
	svc.Poll(context.Background())

	nb, err := db.GetBook(context.Background(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if nb.WorkID != "my-work" {
		t.Fatalf("work_id = %q, want my-work", nb.WorkID)
	}
	if got := spy.called(); len(got) != 1 || got[0] != b.ID {
		t.Fatalf("readmit called with %v, want [%d]", got, b.ID)
	}
}

// TestPollCoreMergedLearnsSlugAcrossASplit: the add-work write SPLIT its pack, so the
// PR renames/removes the old file and adds new ones - entries MOVED between files.
// Taken as one key set per side, only the genuinely new key survives the diff.
func TestPollCoreMergedLearnsSlugAcrossASplit(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Split Book", string(state.ParkCorePending))
	mergedCoreRow(t, db, b.ID)

	gh := coreRepoFake{
		files: `[{"filename":"data/works/m/m.json","status":"modified"},` +
			`{"filename":"data/works/m/n.json","status":"added"},` +
			`{"filename":"data/works/m/p.json","status":"renamed","previous_filename":"data/works/m/o.json"}]`,
		before: map[string]string{
			"data/works/m/m.json": packJSON("ma", "mb", "nc", "nd"),
			"data/works/m/o.json": packJSON("oa", "ob", "pa"),
		},
		after: map[string]string{
			"data/works/m/m.json": packJSON("ma", "mb"),
			"data/works/m/n.json": packJSON("nc", "nd", "new-work", "oa", "ob"),
			"data/works/m/p.json": packJSON("pa"),
		},
	}.server(t)

	spy := &readmitSpy{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, spy, nil)
	svc.Poll(context.Background())

	if nb, _ := db.GetBook(context.Background(), b.ID); nb.WorkID != "new-work" {
		t.Fatalf("work_id = %q, want new-work (moved keys must cancel out)", nb.WorkID)
	}
	if got := spy.called(); len(got) != 1 {
		t.Fatalf("readmit = %v, want one", got)
	}
}

// TestPollCoreMergedAmbiguousNoGuess: a PR adding two work entries (or none) names no
// single work - the poller records why on the core row and guesses nothing, and a
// later tick does not re-read the same PR.
func TestPollCoreMergedAmbiguousNoGuess(t *testing.T) {
	for name, after := range map[string]string{
		"several": packJSON("a-one", "b-two", "old"),
		"none":    packJSON("old"),
	} {
		t.Run(name, func(t *testing.T) {
			db := openDB(t)
			b := makeBook(t, db, "Ambiguous", string(state.ParkCorePending))
			mergedCoreRow(t, db, b.ID)
			var reads atomic.Int32
			fake := coreRepoFake{
				files:  `[{"filename":"data/works/0/0.json","status":"modified"}]`,
				before: map[string]string{"data/works/0/0.json": packJSON("old")},
				after:  map[string]string{"data/works/0/0.json": after},
				extra: func(_ http.ResponseWriter, r *http.Request) bool {
					if strings.HasSuffix(r.URL.Path, "/pulls/40/files") {
						reads.Add(1)
					}
					return false
				},
			}
			gh := fake.server(t)
			spy := &readmitSpy{}
			svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, spy, nil)
			svc.Poll(context.Background())

			if nb, _ := db.GetBook(context.Background(), b.ID); nb.WorkID != "" {
				t.Fatalf("work_id = %q, want none (no guess)", nb.WorkID)
			}
			if len(spy.called()) != 0 {
				t.Fatal("an unresolved book must not be re-admitted")
			}
			row := getRow(t, db, b.ID, store.ContribKindCore)
			if !strings.Contains(row.Note, store.ContribNoteCoreSlugUnresolvedPrefix) {
				t.Fatalf("core note = %q, want the unresolved-slug note", row.Note)
			}
			svc.Poll(context.Background())
			if n := reads.Load(); n != 1 {
				t.Fatalf("PR files read %d times, want 1 (a settled answer is not re-read)", n)
			}
		})
	}
}

// --- poller: a merged core row resolves the slug regardless of park state, but only a
// core_pending book is re-admitted ---

func TestPollCoreMergedResolvesRegardlessOfPark(t *testing.T) {
	db := openDB(t)
	// A book that already LEFT core_pending (no park) but whose core PR merged and whose
	// work_id is still empty (a manual retry raced the poller).
	b := makeBook(t, db, "Moved-on Book", "")
	mergedCoreRow(t, db, b.ID)

	gh := coreRepoFake{
		files:  `[{"filename":"data/works/m/mo.json","status":"modified"}]`,
		before: map[string]string{"data/works/m/mo.json": packJSON("mo")},
		after:  map[string]string{"data/works/m/mo.json": packJSON("mo", "my-work")},
	}.server(t)

	spy := &readmitSpy{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, spy, nil)
	svc.Poll(context.Background())

	nb, _ := db.GetBook(context.Background(), b.ID)
	if nb.WorkID != "my-work" {
		t.Fatalf("work_id = %q, want my-work (set regardless of park state)", nb.WorkID)
	}
	if got := spy.called(); len(got) != 0 {
		t.Fatalf("readmit called %v, want none (book is not parked core_pending)", got)
	}
}

// --- poller: the release gate ---

// TestPollReleaseGate: a merged add-work PR is not yet in any data release. The book
// learns its slug but WAITS (parked core_pending, the release-wait message) while the
// catalogue 404s the work, and is re-admitted by a later tick once it is live - under
// the survivor slug when the catalogue redirects.
func TestPollReleaseGate(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Gated Book", string(state.ParkCorePending))
	mergedCoreRow(t, db, b.ID)
	gh := coreRepoFake{
		files:  `[{"filename":"data/works/m/mo.json","status":"modified"}]`,
		before: map[string]string{"data/works/m/mo.json": packJSON("mo")},
		after:  map[string]string{"data/works/m/mo.json": packJSON("mo", "my-work")},
	}.server(t)

	var live atomic.Bool
	var asked atomic.Int32
	resolve := func(_ context.Context, id string) (string, error) {
		asked.Add(1)
		if id != "my-work" && id != "my-work-survivor" {
			t.Errorf("resolve asked for %q", id)
		}
		if !live.Load() {
			return "", ErrWorkNotFound
		}
		return "my-work-survivor", nil
	}
	spy := &readmitSpy{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, spy, resolve)

	// Tick 1: slug learned, not released -> waits with the release message.
	svc.Poll(context.Background())
	nb, _ := db.GetBook(context.Background(), b.ID)
	if nb.WorkID != "my-work" || nb.ParkCode != string(state.ParkCorePending) || nb.Error != ReleaseWaitMsg {
		t.Fatalf("after tick1 book = work %q park %q msg %q, want my-work / core_pending / the release wait", nb.WorkID, nb.ParkCode, nb.Error)
	}
	if len(spy.called()) != 0 {
		t.Fatal("an unreleased work must not re-admit the book")
	}
	if asked.Load() != 1 {
		t.Fatalf("resolve asked %d times in one tick, want 1", asked.Load())
	}

	// Tick 2: still unreleased - the release pass re-checks, nothing changes.
	svc.Poll(context.Background())
	if len(spy.called()) != 0 {
		t.Fatal("still unreleased: no re-admit")
	}

	// Tick 3: released (under a survivor slug) -> adopted and re-admitted.
	live.Store(true)
	svc.Poll(context.Background())
	nb, _ = db.GetBook(context.Background(), b.ID)
	if nb.WorkID != "my-work-survivor" {
		t.Fatalf("work_id = %q, want the survivor adopted", nb.WorkID)
	}
	if got := spy.called(); len(got) != 1 || got[0] != b.ID {
		t.Fatalf("readmit = %v, want [%d]", got, b.ID)
	}
}

// TestPollReleaseGateTransientLeavesBook: a transport failure checking the work is
// neither a release nor a 404 - nothing changes and the next tick retries.
func TestPollReleaseGateTransientLeavesBook(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Flaky", string(state.ParkCorePending))
	mergedCoreRow(t, db, b.ID)
	if err := db.SetBookWorkID(context.Background(), b.ID, "known-work"); err != nil {
		t.Fatal(err)
	}
	spy := &readmitSpy{}
	svc := newService(t, db, "http://127.0.0.1:0", fakeTokenResolver{err: ErrNoCredential}, &capture{}, spy,
		func(context.Context, string) (string, error) { return "", fmt.Errorf("upstream down") })
	svc.Poll(context.Background())
	nb, _ := db.GetBook(context.Background(), b.ID)
	if len(spy.called()) != 0 || nb.Error != "parked" || nb.WorkID != "known-work" {
		t.Fatalf("transient failure changed the book: readmit=%v msg=%q work=%q", spy.called(), nb.Error, nb.WorkID)
	}
}

// --- poller: intake verdicts ---

// verdictFake serves one open issue (#12) with the given labels, no intake PR, and
// the given comments.
func verdictFake(t *testing.T, issueState string, labels []string, comments string, commentReads *atomic.Int32) *httptest.Server {
	t.Helper()
	ls := make([]string, 0, len(labels))
	for _, l := range labels {
		ls = append(ls, fmt.Sprintf(`{"name":%q}`, l))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == http.MethodGet:
			io.WriteString(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/issues/12/comments"):
			if commentReads != nil {
				commentReads.Add(1)
			}
			io.WriteString(w, comments)
		case strings.HasSuffix(r.URL.Path, "/issues/12"):
			fmt.Fprintf(w, `{"number":12,"html_url":"https://gh/issues/12","state":%q,"labels":[%s]}`, issueState, strings.Join(ls, ","))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const botComment = `[{"body":"a human says hi","user":{"login":"someone"}},` +
	`{"body":"Thanks! This needs a **maintainer** to finish - it can't be applied mechanically.\n\n- a characters.json sidecar already exists at data/works-community/0/0.json: characters; replacing it needs a maintainer\n\n_Posted by the intake bot._","user":{"login":"github-actions[bot]"}}]`

// TestPollSurfacesNeedsHumanVerdict: a needs-human verdict is surfaced on the row
// (status stays submitted, an actionable note carrying the bot's own message)
// instead of "intake PR overdue", and the comments are read once, not every tick.
func TestPollSurfacesNeedsHumanVerdict(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Refused Book", "")
	created := addRow(t, db, b.ID, store.ContribKindCharacters, store.ContribModeIssue, 12, store.ContribStatusSubmitted)
	if err := db.SetContributionStatus(context.Background(), created.ID, created.Status, 0, "", "audit passed"); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	gh := verdictFake(t, "open", []string{"data", "data:characters", "data:needs-human"}, botComment, &reads)
	cap := &capture{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, cap, nil, nil)
	// Well past the grace window: without the verdict this would be "overdue".
	svc.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	svc.Poll(context.Background())

	row := getRow(t, db, b.ID, store.ContribKindCharacters)
	if row.Status != store.ContribStatusSubmitted {
		t.Fatalf("status = %s, want submitted (the issue is open; an edit re-runs the bot)", row.Status)
	}
	if !strings.HasPrefix(row.Note, "audit passed; "+store.ContribNoteIntakeNeedsHuman) ||
		!strings.Contains(row.Note, "already exists at data/works-community/0/0.json") {
		t.Fatalf("note = %q, want the audit note then the needs-human verdict with the bot's message", row.Note)
	}
	if strings.Contains(row.Note, store.ContribNoteIntakePRStale) {
		t.Fatalf("note = %q: a verdict must replace the overdue warning", row.Note)
	}
	if !store.ContributionNeedsAttention([]store.Contribution{row}) {
		t.Fatal("a needs-human verdict must flag the chip for attention")
	}
	svc.Poll(context.Background())
	if reads.Load() != 1 || len(cap.all()) != 1 {
		t.Fatalf("steady verdict tick: comment reads=%d publishes=%d, want 1/1", reads.Load(), len(cap.all()))
	}
}

// TestPollDuplicateVerdictIsDone: a duplicate means the thing is in the database
// already - recorded already_covered (done by someone else), not an error, and it
// counts as landed coverage.
func TestPollDuplicateVerdictIsDone(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Dup Book", "")
	addRow(t, db, b.ID, store.ContribKindRecaps, store.ContribModeIssue, 12, store.ContribStatusSubmitted)
	gh := verdictFake(t, "closed", []string{"data", "data:recaps", "data:duplicate"},
		`[{"body":"This looks like it's **already in the database**.\n\n- the recaps are already there\n\n_Posted by the intake bot._"}]`, nil)
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, nil, nil)
	svc.Poll(context.Background())

	row := getRow(t, db, b.ID, store.ContribKindRecaps)
	if row.Status != store.ContribStatusAlreadyCovered || !strings.HasPrefix(row.Note, store.ContribNoteIntakeDuplicate) {
		t.Fatalf("row = %+v, want already_covered with the duplicate note", row)
	}
	if store.ContributionNeedsAttention([]store.Contribution{row}) {
		t.Fatal("a duplicate is done, not an attention state")
	}
	if _, hasRecaps := store.LandedCoverage([]store.Contribution{row}); !hasRecaps {
		t.Fatal("a duplicate verdict means the recaps are upstream (landed)")
	}
}

// TestPollInvalidVerdictClosedIssue: an invalid verdict on an issue the submitter
// closed is closed, keeping the verdict as the reason.
func TestPollInvalidVerdictClosedIssue(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Invalid Book", "")
	addRow(t, db, b.ID, store.ContribKindCharacters, store.ContribModeIssue, 12, store.ContribStatusSubmitted)
	gh := verdictFake(t, "closed", []string{"data:invalid"}, `[]`, nil)
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, nil, nil)
	svc.Poll(context.Background())
	row := getRow(t, db, b.ID, store.ContribKindCharacters)
	if row.Status != store.ContribStatusClosed || row.Note != store.ContribNoteIntakeInvalid {
		t.Fatalf("row = %+v, want closed with the bare invalid verdict (no bot comment)", row)
	}
}

// TestPollVerdictClearedByIntakePR: a later intake PR (a maintainer fixed the issue
// and the bot re-ran) replaces the verdict.
func TestPollVerdictClearedByIntakePR(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Recovered", "")
	created := addRow(t, db, b.ID, store.ContribKindCharacters, store.ContribModeIssue, 12, store.ContribStatusSubmitted)
	if err := db.SetContributionStatus(context.Background(), created.ID, created.Status, 0, "",
		"audit passed; "+store.ContribNoteIntakeNeedsHuman+" - stuff; more"); err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"number":22,"html_url":"https://gh/pull/22","state":"open","merged":false}]`)
	}))
	defer gh.Close()
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, nil, nil)
	svc.Poll(context.Background())
	row := getRow(t, db, b.ID, store.ContribKindCharacters)
	if row.Status != store.ContribStatusPROpen || row.Note != "audit passed" {
		t.Fatalf("row = %+v, want pr_open with only the audit note left", row)
	}
}

// TestPollCoreDuplicateReadmits: the add-work issue answered duplicate means the work
// exists; the core_pending book is re-admitted so the contributing stage re-resolves
// it instead of waiting on a PR that is never coming. A needs-human answer re-words
// the book's park message to point at the issue.
func TestPollCoreVerdictMovesBookOn(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		db := openDB(t)
		b := makeBook(t, db, "Core Dup", string(state.ParkCorePending))
		addRow(t, db, b.ID, store.ContribKindCore, store.ContribModeIssue, 12, store.ContribStatusSubmitted)
		gh := verdictFake(t, "closed", []string{"data:duplicate"}, `[]`, nil)
		spy := &readmitSpy{}
		svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, spy, nil)
		svc.Poll(context.Background())
		if got := spy.called(); len(got) != 1 || got[0] != b.ID {
			t.Fatalf("readmit = %v, want [%d]", got, b.ID)
		}
		svc.Poll(context.Background())
		if got := spy.called(); len(got) != 1 {
			t.Fatalf("readmit = %v, want exactly one (a settled verdict does not re-act)", got)
		}
	})
	t.Run("needs-human", func(t *testing.T) {
		db := openDB(t)
		b := makeBook(t, db, "Core Human", string(state.ParkCorePending))
		addRow(t, db, b.ID, store.ContribKindCore, store.ContribModeIssue, 12, store.ContribStatusSubmitted)
		gh := verdictFake(t, "open", []string{"data:needs-human"}, `[]`, nil)
		spy := &readmitSpy{}
		svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, spy, nil)
		svc.Poll(context.Background())
		nb, _ := db.GetBook(context.Background(), b.ID)
		if nb.ParkCode != string(state.ParkCorePending) || !strings.Contains(nb.Error, "needs a maintainer") ||
			!strings.Contains(nb.Error, "https://gh/issues/12") {
			t.Fatalf("book = park %q msg %q, want core_pending with the verdict + issue link", nb.ParkCode, nb.Error)
		}
		if len(spy.called()) != 0 {
			t.Fatal("needs-human must not re-admit")
		}
	})
}

// --- SubmitCore concurrency + idempotency ---

// issueCounter is an httptest server that counts add-work issue POSTs and echoes an
// incrementing issue number.
func issueCounter(t *testing.T, count *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues") {
			n := atomic.AddInt32(count, 1)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, fmt.Sprintf(`{"number":%d,"html_url":"https://gh/issues/%d","labels":[{"name":"data:add-work"}]}`, n, n))
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSubmitCoreConcurrentSingleIssue: two concurrent SubmitCore calls for one book open
// exactly one issue (the per-book lock serializes; the loser reuses the recorded row).
func TestSubmitCoreConcurrentSingleIssue(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Needs Core", string(state.ParkCoreNeeded))
	var issues int32
	gh := issueCounter(t, &issues)
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, nil, nil)
	p := CoreProposal{Title: "Needs Core", Authors: []string{"A"}, Language: "en", Narrators: []string{"N"}, Sources: "scan"}

	var wg sync.WaitGroup
	rows := make([]store.Contribution, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rows[i], errs[i] = svc.SubmitCore(context.Background(), b, p)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&issues); got != 1 {
		t.Fatalf("issues created = %d, want exactly 1", got)
	}
	if rows[0].Number != rows[1].Number || rows[0].Number != 1 {
		t.Fatalf("callers disagree on the row: %d vs %d", rows[0].Number, rows[1].Number)
	}
	if nb, _ := db.GetBook(context.Background(), b.ID); nb.ParkCode != string(state.ParkCorePending) {
		t.Fatalf("park = %s, want core_pending", nb.ParkCode)
	}
}

// TestSubmitCoreResubmitReusesRecordedIssue: a partial prior submit (issue opened + row
// recorded, but the park flip did not land) reuses the recorded issue on resubmit rather
// than opening a second, and ensures the park flip.
func TestSubmitCoreResubmitReusesRecordedIssue(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Needs Core", string(state.ParkCoreNeeded))
	// The prior submit recorded the core row (issue 77) but left the book at core_needed.
	if _, err := db.UpsertContribution(context.Background(), store.Contribution{
		BookID: b.ID, Kind: store.ContribKindCore, Mode: store.ContribModeIssue,
		Number: 77, URL: "https://gh/issues/77", Status: store.ContribStatusSubmitted,
	}); err != nil {
		t.Fatal(err)
	}
	var issues int32
	gh := issueCounter(t, &issues)
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, &capture{}, nil, nil)
	p := CoreProposal{Title: "Needs Core", Authors: []string{"A"}, Language: "en", Narrators: []string{"N"}, Sources: "scan"}

	row, err := svc.SubmitCore(context.Background(), b, p)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if got := atomic.LoadInt32(&issues); got != 0 {
		t.Fatalf("resubmit opened %d issues, want 0 (reuse the recorded issue)", got)
	}
	if row.Number != 77 {
		t.Fatalf("row = %+v, want the recorded issue #77", row)
	}
	if nb, _ := db.GetBook(context.Background(), b.ID); nb.ParkCode != string(state.ParkCorePending) {
		t.Fatalf("park = %s, want core_pending (the flip is ensured on resubmit)", nb.ParkCode)
	}
}

// --- poller: unauthenticated (tokenless) reads work; a 500 leaves rows unchanged ---

func TestPollTokenlessAndErrorResilience(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Resilient", "")
	addRow(t, db, b.ID, store.ContribKindCharacters, store.ContribModeIssue, 12, store.ContribStatusSubmitted)

	var sawAuth string
	var fail bool
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		// FindIntakePR -> [] then GetIssue open -> no change.
		if strings.HasSuffix(r.URL.Path, "/pulls") {
			io.WriteString(w, `[]`)
			return
		}
		io.WriteString(w, `{"number":12,"html_url":"u","state":"open","labels":[]}`)
	}))
	defer gh.Close()

	cap := &capture{}
	// No credential -> the poller runs unauthenticated.
	svc := newService(t, db, gh.URL, fakeTokenResolver{err: ErrNoCredential}, cap, nil, nil)

	svc.Poll(context.Background())
	if sawAuth != "" {
		t.Fatalf("tokenless poll sent Authorization = %q, want empty", sawAuth)
	}
	if len(cap.all()) != 0 {
		t.Fatalf("open issue, no PR -> no publish; got %d", len(cap.all()))
	}

	// A failing GitHub leaves the row unchanged and does not crash the tick.
	fail = true
	svc.Poll(context.Background())
	if row := getRow(t, db, b.ID, store.ContribKindCharacters); row.Status != store.ContribStatusSubmitted {
		t.Fatalf("after 500 tick = %s, want unchanged submitted", row.Status)
	}
	if len(cap.all()) != 0 {
		t.Fatalf("500 tick published %d, want 0", len(cap.all()))
	}
}

// --- RunPoller returns promptly on ctx cancel ---

func TestRunPollerStopsOnCancel(t *testing.T) {
	db := openDB(t)
	svc := newService(t, db, "http://127.0.0.1:0", fakeTokenResolver{err: ErrNoCredential}, &capture{}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunPoller(ctx, time.Hour) // long interval; must still exit on cancel
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunPoller did not return within 2s of cancel")
	}
}

// --- SubmitCore opens the issue, records the row, and flips the park ---

func TestSubmitCoreHappyPath(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Needs Core", string(state.ParkCoreNeeded))

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues") {
			// An add-work proposal belongs to the CORE repository (the community repo
			// holds only the CC BY-SA sidecars and refuses it).
			if r.URL.Path != "/repos/"+testRepo+"/issues" {
				t.Errorf("add-work issue opened at %s, want the core repo %s", r.URL.Path, testRepo)
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":50,"html_url":"https://gh/issues/50","labels":[{"name":"data"},{"name":"data:add-work"}]}`)
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
	}))
	defer gh.Close()

	cap := &capture{}
	svc := newService(t, db, gh.URL, fakeTokenResolver{token: "ghp_x"}, cap, nil, nil)

	p := CoreProposal{Title: "Needs Core", Authors: []string{"A"}, Language: "en", Narrators: []string{"N"}, Sources: "scan"}
	row, err := svc.SubmitCore(context.Background(), b, p)
	if err != nil {
		t.Fatalf("SubmitCore: %v", err)
	}
	if row.Number != 50 || row.Status != store.ContribStatusSubmitted || row.Kind != store.ContribKindCore || row.Repo != testRepo {
		t.Fatalf("row = %+v", row)
	}
	nb, _ := db.GetBook(context.Background(), b.ID)
	if nb.ParkCode != string(state.ParkCorePending) || nb.Error != corePendingMsg {
		t.Fatalf("park = %s / %q, want core_pending / %q", nb.ParkCode, nb.Error, corePendingMsg)
	}
	if evs := cap.all(); len(evs) != 1 || evs[0].Kind != store.ContribKindCore || evs[0].Status != store.ContribStatusSubmitted {
		t.Fatalf("publishes = %+v", evs)
	}
}

// TestSubmitCoreNoCredential: without a credential SubmitCore refuses before any
// GitHub call.
func TestSubmitCoreNoCredential(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Needs Core", string(state.ParkCoreNeeded))
	svc := newService(t, db, "http://127.0.0.1:0", fakeTokenResolver{err: ErrNoCredential}, &capture{}, nil, nil)
	p := CoreProposal{Title: "T", Authors: []string{"A"}, Language: "en", Narrators: []string{"N"}, Sources: "s"}
	if _, err := svc.SubmitCore(context.Background(), b, p); err != ErrNoCredential {
		t.Fatalf("SubmitCore err = %v, want ErrNoCredential", err)
	}
}

// --- SetWork validates, verifies, records, and re-admits ---

func TestSetWork(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Set Work", string(state.ParkCoreNeeded))
	spy := &readmitSpy{}
	svc := newService(t, db, "", fakeTokenResolver{err: ErrNoCredential}, &capture{}, spy,
		func(_ context.Context, id string) (string, error) { return id, nil }) // resolve: exists

	if err := svc.SetWork(context.Background(), b, "the-work"); err != nil {
		t.Fatalf("SetWork: %v", err)
	}
	nb, _ := db.GetBook(context.Background(), b.ID)
	if nb.WorkID != "the-work" {
		t.Fatalf("work_id = %q", nb.WorkID)
	}
	if got := spy.called(); len(got) != 1 || got[0] != b.ID {
		t.Fatalf("readmit = %v, want [%d]", got, b.ID)
	}
}

func TestSetWorkInvalidSlug(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Bad", string(state.ParkCoreNeeded))
	spy := &readmitSpy{}
	svc := newService(t, db, "", fakeTokenResolver{err: ErrNoCredential}, &capture{}, spy,
		func(context.Context, string) (string, error) {
			t.Fatal("verify must not run for a bad slug")
			return "", nil
		})
	if err := svc.SetWork(context.Background(), b, "Not A Slug!"); err != ErrInvalidSlug {
		t.Fatalf("err = %v, want ErrInvalidSlug", err)
	}
	if len(spy.called()) != 0 {
		t.Fatal("readmit must not run for a bad slug")
	}
}

func TestSetWorkNotFoundUpstream(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Missing", string(state.ParkCoreNeeded))
	svc := newService(t, db, "", fakeTokenResolver{err: ErrNoCredential}, &capture{}, &readmitSpy{},
		func(context.Context, string) (string, error) { return "", ErrWorkNotFound })
	if err := svc.SetWork(context.Background(), b, "ghost-work"); err != ErrWorkNotFound {
		t.Fatalf("err = %v, want ErrWorkNotFound", err)
	}
}

// TestSetWorkAdoptsSurvivor: a slug a merge retired resolves to its survivor, and the
// survivor is what gets recorded.
func TestSetWorkAdoptsSurvivor(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Retired", string(state.ParkCoreNeeded))
	svc := newService(t, db, "", fakeTokenResolver{err: ErrNoCredential}, &capture{}, &readmitSpy{},
		func(context.Context, string) (string, error) { return "the-survivor", nil })
	if err := svc.SetWork(context.Background(), b, "the-retired"); err != nil {
		t.Fatalf("SetWork: %v", err)
	}
	if nb, _ := db.GetBook(context.Background(), b.ID); nb.WorkID != "the-survivor" {
		t.Fatalf("work_id = %q, want the survivor", nb.WorkID)
	}
}
