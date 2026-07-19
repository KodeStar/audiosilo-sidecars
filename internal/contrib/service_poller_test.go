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

func newService(t *testing.T, db *store.DB, ghURL string, tok TokenResolver, cap *capture, spy *readmitSpy, verify func(context.Context, string) error) *Service {
	t.Helper()
	deps := ServiceDeps{
		DB: db, Repo: testRepo, BaseURL: ghURL, Tokens: tok,
		Publish: cap.publish, CorePendingMsg: corePendingMsg, VerifyWork: verify,
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

// --- poller: core merged -> slug extracted, work_id set, book re-admitted ---

func TestPollCoreMergedResolvesSlug(t *testing.T) {
	db := openDB(t)
	b := makeBook(t, db, "Core Book", string(state.ParkCorePending))
	addRow(t, db, b.ID, store.ContribKindCore, store.ContribModeIssue, 30, store.ContribStatusSubmitted)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls") && r.Method == http.MethodGet:
			// FindIntakePR for the core issue -> a merged PR (#40).
			io.WriteString(w, `[{"number":40,"html_url":"https://gh/pull/40","state":"closed","merged":true,"merged_at":"t"}]`)
		case strings.HasSuffix(r.URL.Path, "/pulls/40/files"):
			io.WriteString(w, `[{"filename":"data/works/my/my-work/work.json"}]`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer gh.Close()

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

// --- poller: a merged core row resolves the slug regardless of park state, but only a
// core_pending book is re-admitted ---

func TestPollCoreMergedResolvesRegardlessOfPark(t *testing.T) {
	db := openDB(t)
	// A book that already LEFT core_pending (no park) but whose core PR merged and whose
	// work_id is still empty (a manual retry raced the poller).
	b := makeBook(t, db, "Moved-on Book", "")
	row := addRow(t, db, b.ID, store.ContribKindCore, store.ContribModeIssue, 30, store.ContribStatusSubmitted)
	if err := db.SetContributionStatus(context.Background(), row.ID, store.ContribStatusMerged, 40, "https://gh/pull/40", ""); err != nil {
		t.Fatal(err)
	}

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls/40/files") {
			io.WriteString(w, `[{"filename":"data/works/my/my-work/work.json"}]`)
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusInternalServerError)
	}))
	defer gh.Close()

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
	if row.Number != 50 || row.Status != store.ContribStatusSubmitted || row.Kind != store.ContribKindCore {
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
		func(context.Context, string) error { return nil }) // verify: exists

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
		func(context.Context, string) error { t.Fatal("verify must not run for a bad slug"); return nil })
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
		func(context.Context, string) error { return ErrWorkNotFound })
	if err := svc.SetWork(context.Background(), b, "ghost-work"); err != ErrWorkNotFound {
		t.Fatalf("err = %v, want ErrWorkNotFound", err)
	}
}
